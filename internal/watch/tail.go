// Package watch tails Claude Code transcript files and turns appended
// lines into derived events with restart-safe identity: a tail always
// feeds its parser from offset 0 (the parser contract) and a persisted
// watermark drops events that were already emitted in a prior run.
package watch

import (
	"bufio"
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
	parser    *transcript.Parser
	readPos   int64 // bytes fed to parser (complete lines only)
	lastSize  int64 // size at previous poll, for activity detection
	mark      state.Watermark
	project   string
	synced    bool // parser state reflects file[0:readPos]
}

// newTail resumes a file at a persisted watermark. knownSize is the file
// size recorded when the watermark was saved: while the file still has
// exactly that size there is nothing new, so nothing is parsed; the
// first observed change triggers a replay from offset 0.
func newTail(path string, mark state.Watermark, knownSize int64) *tail {
	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	return &tail{
		path:      path,
		sessionID: sessionID,
		parser:    transcript.NewParser(sessionID),
		readPos:   knownSize,
		lastSize:  knownSize,
		mark:      mark,
		synced:    knownSize == 0,
	}
}

func (t *tail) poll() (events []transcript.Event, grew bool, err error) {
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
