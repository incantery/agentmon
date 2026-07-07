package watch

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/incantery/agentmon/internal/redact"
	"github.com/incantery/agentmon/internal/state"
	"github.com/incantery/agentmon/internal/transcript"
)

// Sink receives fully-formed (machine-stamped, redacted) events.
type Sink interface {
	Emit(ev transcript.Event) error
}

type Options struct {
	Roots      []string
	Machine    string
	Level      redact.Level
	IdleAfter  time.Duration
	EndedAfter time.Duration
	Backfill   bool // emit pre-existing history on first sighting
	Now        func() time.Time
}

type Watcher struct {
	opts      Options
	startedAt time.Time
	st        *state.State
	sink      Sink
	tails     map[string]*tail

	FileErrs int // per-file stat/read errors (never fatal)
	SinkErrs int
}

func New(opts Options, st *state.State, sink Sink) *Watcher {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Watcher{opts: opts, startedAt: opts.Now(), st: st, sink: sink, tails: map[string]*tail{}}
}

// PollOnce runs one deterministic pass over every transcript file:
// discover, tail, emit, update timers (Task 6), persist state.
func (w *Watcher) PollOnce() error {
	now := w.opts.Now()
	seen := map[string]bool{}
	for _, path := range w.scan() {
		seen[path] = true
		w.pollFile(path, now)
	}
	// Files that vanished: tracked ones get a session_ended synthetic;
	// entries left over from a previous run (file deleted while we were
	// down) are pruned silently — consistent with grandfathering.
	for path, fs := range w.st.Files {
		if seen[path] {
			continue
		}
		if t, ok := w.tails[path]; ok {
			if !fs.Ended && t.agentID == "" {
				w.synthetic(t, fs, now, transcript.SessionEndedPayload{Reason: "removed"})
			}
			delete(w.tails, path)
		}
		w.st.Delete(path)
	}
	return w.st.Save()
}

func (w *Watcher) pollFile(path string, now time.Time) {
	t, tracked := w.tails[path]
	_, known := w.st.Files[path] // must read BEFORE File() get-or-creates
	fs := w.st.File(path)
	if !tracked {
		if !known {
			// First sighting ever: fast-forward unless backfilling, so the
			// first run doesn't flood the sink with historical transcripts.
			// Only files that PREDATE this watcher fast-forward: a file
			// born while we watch (mtime at-or-after start) replays from
			// zero — otherwise a subagent that starts and finishes within
			// one poll interval would be skipped entirely.
			// A known file with an unset watermark (backfill mode, or a
			// shrink-reset saved mid-re-anchor) must NOT fast-forward: its
			// tail simply replays from zero on the next change, which at
			// worst re-emits already-shipped identities (at-least-once,
			// server dedupes) and never skips content.
			fi, err := os.Stat(path)
			if err != nil {
				w.FileErrs++
				w.st.Delete(path)
				return
			}
			if !w.opts.Backfill && fi.ModTime().Before(w.startedAt) {
				fs.Watermark = state.Watermark{Offset: fi.Size(), Seq: -1, Set: true}
				fs.Size = fi.Size()
			}
			w.initTimers(fs, fi, now)
		}
		t = newTail(path, fs.Watermark, fs.Size)
		w.tails[path] = t
	}

	evs, grewNow, err := t.poll()
	if err != nil {
		w.FileErrs++
		return
	}
	for _, ev := range evs {
		ev.AgentID = t.agentID
		ev.AgentType = t.agentType
		w.emit(ev)
	}
	fs.Watermark = t.mark
	fs.Size = t.lastSize
	if t.project != "" {
		fs.Project = t.project
	}
	if len(evs) > 0 {
		last := evs[len(evs)-1].Type
		fs.MidTurn = last != transcript.TurnCompleted && last != transcript.SessionEnded
	}
	w.tickTimers(t, fs, grewNow, now)
}

// emit stamps machine identity, applies the content level, and hands the
// event to the sink. Redaction here is the enforcement point the spec
// demands: nothing reaches a sink unredacted.
func (w *Watcher) emit(ev transcript.Event) {
	ev.Machine = w.opts.Machine
	if err := w.sink.Emit(redact.Apply(w.opts.Level, ev)); err != nil {
		w.SinkErrs++
	}
}

// synthetic emits a watcher-generated event. Synthetics get an identity
// space disjoint from parser events — negative seq (-2, -3, …; parser
// seqs are ≥ 0 and -1 is the fast-forward sentinel) — and never advance
// the ship watermark, so they can never cover a future parser event.
func (w *Watcher) synthetic(t *tail, fs *state.FileState, now time.Time, pl transcript.Payload) {
	fs.SynthSeq++
	project := t.project
	if project == "" {
		project = fs.Project
	}
	w.emit(transcript.Event{
		Project:   project,
		SessionID: t.sessionID,
		Offset:    fs.Watermark.Offset,
		Seq:       -1 - fs.SynthSeq,
		TS:        now,
		Type:      pl.EventType(),
		Payload:   pl,
	})
}

// initTimers seeds timer state at first sighting. Activity starts at the
// file's mtime, and a file already past EndedAfter is marked Ended
// SILENTLY — historical transcripts must not fire a wave of
// session_ended events when the watcher first runs on a machine.
func (w *Watcher) initTimers(fs *state.FileState, fi os.FileInfo, now time.Time) {
	fs.LastActivityUnix = fi.ModTime().Unix()
	if now.Sub(fi.ModTime()) >= w.opts.EndedAfter {
		fs.Ended = true
	}
}

func (w *Watcher) tickTimers(t *tail, fs *state.FileState, grew bool, now time.Time) {
	if t.agentID != "" {
		// Agent files share the parent's session_id: a subagent going
		// quiet must not report the interactive session idle or ended.
		// Session lifecycle belongs to the main transcript alone.
		return
	}
	if grew {
		fs.LastActivityUnix = now.Unix()
		fs.IdleFired = false
		fs.Ended = false
		return
	}
	if fs.LastActivityUnix == 0 {
		fs.LastActivityUnix = now.Unix()
		return
	}
	idle := now.Sub(time.Unix(fs.LastActivityUnix, 0))
	if !fs.Ended && idle >= w.opts.EndedAfter {
		fs.Ended = true
		w.synthetic(t, fs, now, transcript.SessionEndedPayload{Reason: "inactive"})
		return
	}
	if fs.MidTurn && !fs.IdleFired && !fs.Ended && idle >= w.opts.IdleAfter {
		fs.IdleFired = true
		w.synthetic(t, fs, now, transcript.SessionIdlePayload{IdleSeconds: int64(idle.Seconds())})
	}
}

func (w *Watcher) scan() []string {
	// Three transcript layouts (spec, "Watching"): the main session file
	// plus Task-tool and Workflow subagent files nested under a directory
	// named after the parent session.
	globs := []string{
		filepath.Join("*", "*.jsonl"),
		filepath.Join("*", "*", "subagents", "*.jsonl"),
		filepath.Join("*", "*", "subagents", "workflows", "*", "*.jsonl"),
	}
	var out []string
	for _, root := range w.opts.Roots {
		for _, g := range globs {
			matches, err := filepath.Glob(filepath.Join(root, g))
			if err != nil {
				continue
			}
			out = append(out, matches...)
		}
	}
	sort.Strings(out)
	return out
}

// Run polls on a fixed interval until ctx is done.
func (w *Watcher) Run(ctx context.Context, interval time.Duration) error {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := w.PollOnce(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
