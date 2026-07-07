// Package watch tails Claude Code transcript files and turns appended
// lines into derived events with restart-safe identity: a tail always
// feeds its parser from offset 0 (the parser contract) and a persisted
// watermark drops events that were already emitted in a prior run.
package watch

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/incantery/agentmon/internal/state"
	"github.com/incantery/agentmon/internal/transcript"
)

type tail struct {
	path      string
	sessionID string
	agentID   string // "" for main transcripts
	agentType string // from the .meta.json sidecar, once read
	metaDone  bool   // sidecar read (or not applicable); stop retrying
	parser    *transcript.Parser
	readPos   int64 // bytes fed to parser (complete lines only)
	lastSize  int64 // size at previous poll, for activity detection
	mark      state.Watermark
	project   string
	synced    bool // parser state reflects file[0:readPos]
}

// deriveIdentity maps a transcript path to its session and agent identity.
// Main transcripts (<project>/<sid>.jsonl) have agentID "". Subagent
// transcripts live under a directory named after the parent session:
//
//	<project>/<sid>/subagents/agent-<id>.jsonl
//	<project>/<sid>/subagents/workflows/<wf>/agent-<id>.jsonl
//
// and take the parent session's ID plus their own agent ID, so per-session
// aggregations include the session's whole agent fleet.
func deriveIdentity(path string) (sessionID, agentID string) {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	parts := strings.Split(filepath.ToSlash(filepath.Dir(path)), "/")
	for i := len(parts) - 1; i > 0; i-- {
		if parts[i] == "subagents" {
			return parts[i-1], base
		}
	}
	return base, ""
}

// newTail resumes a file at a persisted watermark. knownSize is the file
// size recorded when the watermark was saved: while the file still has
// exactly that size there is nothing new, so nothing is parsed; the
// first observed change triggers a replay from offset 0.
func newTail(path string, mark state.Watermark, knownSize int64) *tail {
	sessionID, agentID := deriveIdentity(path)
	return &tail{
		path:      path,
		sessionID: sessionID,
		agentID:   agentID,
		metaDone:  agentID == "", // main transcripts have no sidecar
		parser:    transcript.NewParser(sessionID),
		readPos:   knownSize,
		lastSize:  knownSize,
		mark:      mark,
		synced:    knownSize == 0,
	}
}

// loadMeta reads the agent file's .meta.json sidecar for agentType. The
// sidecar may appear after the transcript, so a missing file is retried
// on every poll until it is read once; a torn/partial read (caught
// mid-write) is retried too, rather than latching agentType blank forever.
// description and spawnDepth are deliberately not shipped.
func (t *tail) loadMeta() {
	if t.metaDone {
		return
	}
	b, err := os.ReadFile(strings.TrimSuffix(t.path, ".jsonl") + ".meta.json")
	if err != nil {
		return
	}
	var m struct {
		AgentType string `json:"agentType"`
	}
	if json.Unmarshal(b, &m) != nil {
		// Torn/partial write (sidecar caught mid-write): do NOT latch
		// metaDone, so the next poll retries instead of losing agentType
		// for this file forever.
		return
	}
	t.metaDone = true
	t.agentType = m.AgentType
}

func (t *tail) poll() (events []transcript.Event, grew bool, err error) {
	t.loadMeta()
	fi, err := os.Stat(t.path)
	if err != nil {
		return nil, false, err
	}
	size := fi.Size()
	grew = size != t.lastSize
	t.lastSize = size

	switch {
	case size < t.readPos:
		// Rewritten/truncated: prior offsets are invalid. Clear the
		// watermark — this file gets a fresh identity space.
		t.parser = transcript.NewParser(t.sessionID)
		t.readPos = 0
		t.mark = state.Watermark{}
		t.synced = true
	case !t.synced && size != t.readPos:
		// Resumed tail seeing its first change: replay from zero; the
		// watermark drops everything already emitted.
		t.parser = transcript.NewParser(t.sessionID)
		t.readPos = 0
		t.synced = true
	}
	if size == t.readPos {
		return nil, grew, nil
	}

	f, err := os.Open(t.path)
	if err != nil {
		return nil, grew, err
	}
	defer f.Close()
	if _, err := f.Seek(t.readPos, io.SeekStart); err != nil {
		return nil, grew, err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err == nil {
			events = append(events, t.filter(t.parser.Line(t.readPos, line))...)
			t.readPos += int64(len(line))
			continue
		}
		if err == io.EOF {
			// A trailing line without '\n' may be mid-write: leave it
			// unconsumed; readPos does not advance past it.
			return events, grew, nil
		}
		return events, grew, err
	}
}

// filter drops events covered by the watermark and advances it.
func (t *tail) filter(evs []transcript.Event) []transcript.Event {
	var out []transcript.Event
	for _, ev := range evs {
		if t.mark.Covers(ev.Offset, ev.Seq) {
			continue
		}
		t.mark = state.Watermark{Offset: ev.Offset, Seq: ev.Seq, Set: true}
		if ev.Project != "" {
			t.project = ev.Project
		}
		out = append(out, ev)
	}
	return out
}
