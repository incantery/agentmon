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
	opts  Options
	st    *state.State
	sink  Sink
	tails map[string]*tail

	FileErrs int // per-file stat/read errors (never fatal)
	SinkErrs int
}

func New(opts Options, st *state.State, sink Sink) *Watcher {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Watcher{opts: opts, st: st, sink: sink, tails: map[string]*tail{}}
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
	// Files we were tracking that vanished: the session is gone.
	for path, t := range w.tails {
		if seen[path] {
			continue
		}
		fs := w.st.File(path)
		if !fs.Ended {
			w.synthetic(t, fs, now, transcript.SessionEndedPayload{Reason: "removed"})
		}
		delete(w.tails, path)
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
			if !w.opts.Backfill {
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
		w.emit(ev)
	}
	fs.Watermark = t.mark
	fs.Size = t.lastSize
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

// synthetic emits a watcher-generated event, continuing the seq counter
// at the file's last offset so identity never collides with parser events.
func (w *Watcher) synthetic(t *tail, fs *state.FileState, now time.Time, pl transcript.Payload) {
	fs.Watermark.Seq++
	fs.Watermark.Set = true
	t.mark = fs.Watermark
	w.emit(transcript.Event{
		Project:   t.project,
		SessionID: t.sessionID,
		Offset:    fs.Watermark.Offset,
		Seq:       fs.Watermark.Seq,
		TS:        now,
		Type:      pl.EventType(),
		Payload:   pl,
	})
}

// initTimers and tickTimers are completed in the timers task; the core
// task only records activity so state is coherent.
func (w *Watcher) initTimers(fs *state.FileState, fi os.FileInfo, now time.Time) {
	fs.LastActivityUnix = fi.ModTime().Unix()
}

func (w *Watcher) tickTimers(t *tail, fs *state.FileState, grew bool, now time.Time) {
	if grew {
		fs.LastActivityUnix = now.Unix()
		fs.IdleFired = false
		fs.Ended = false
	}
}

func (w *Watcher) scan() []string {
	var out []string
	for _, root := range w.opts.Roots {
		matches, err := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
		if err != nil {
			continue
		}
		out = append(out, matches...)
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
