package watch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/incantery/agentmon/internal/state"
	"github.com/incantery/agentmon/internal/transcript"
)

const line1 = `{"type":"user","message":{"role":"user","content":"hello"},"timestamp":"2026-07-06T10:00:00.000Z","cwd":"/p","sessionId":"s1"}` + "\n"
const line2 = `{"type":"ai-title","aiTitle":"T","sessionId":"s1"}` + "\n"

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func types(evs []transcript.Event) []transcript.EventType {
	var out []transcript.EventType
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

func TestFreshFileEmitsEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	write(t, path, line1)
	tl := newTail(path, state.Watermark{}, 0)
	evs, grew, err := tl.poll()
	if err != nil || !grew {
		t.Fatalf("grew=%v err=%v", grew, err)
	}
	// session_started + user_prompt
	if got := types(evs); len(got) != 2 || got[0] != transcript.SessionStarted || got[1] != transcript.UserPrompt {
		t.Fatalf("types = %v", got)
	}
	if tl.project != "/p" || tl.sessionID != "s1" {
		t.Errorf("project=%q session=%q", tl.project, tl.sessionID)
	}
	if !tl.mark.Set {
		t.Error("watermark not advanced")
	}
}

func TestResumeUnchangedParsesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	write(t, path, line1)
	fi, _ := os.Stat(path)
	tl := newTail(path, state.Watermark{Offset: 0, Seq: 1, Set: true}, fi.Size())
	evs, grew, err := tl.poll()
	if err != nil || grew || len(evs) != 0 {
		t.Fatalf("unchanged resume: evs=%v grew=%v err=%v", evs, grew, err)
	}
}

func TestResumeThenAppendReplaysWithoutDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	write(t, path, line1)
	fi, _ := os.Stat(path)
	// watermark says line1's two events (seq 0,1 at offset 0) were emitted
	tl := newTail(path, state.Watermark{Offset: 0, Seq: 1, Set: true}, fi.Size())
	appendTo(t, path, line2)
	evs, grew, err := tl.poll()
	if err != nil || !grew {
		t.Fatalf("grew=%v err=%v", grew, err)
	}
	// replay saw line1 again but the watermark drops it; only session_title emits
	if got := types(evs); len(got) != 1 || got[0] != transcript.SessionTitle {
		t.Fatalf("types = %v (duplicates leaked or new event lost)", got)
	}
	if evs[0].Offset != int64(len(line1)) || evs[0].Seq != 0 {
		t.Errorf("identity = (%d,%d), want (%d,0)", evs[0].Offset, evs[0].Seq, len(line1))
	}
}

func TestShrinkResetsIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	write(t, path, line1+line2)
	tl := newTail(path, state.Watermark{}, 0)
	tl.poll()
	write(t, path, line2) // rewritten smaller
	evs, _, err := tl.poll()
	if err != nil {
		t.Fatal(err)
	}
	// fresh identity space: session_started + session_title from offset 0
	if got := types(evs); len(got) != 2 || got[0] != transcript.SessionStarted {
		t.Fatalf("types after rewrite = %v", got)
	}
	if evs[0].Offset != 0 {
		t.Errorf("rewrite must restart offsets at 0, got %d", evs[0].Offset)
	}
}

func TestPartialLineLeftForNextPoll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	write(t, path, line1+`{"type":"ai-ti`) // torn mid-write
	tl := newTail(path, state.Watermark{}, 0)
	evs, _, _ := tl.poll()
	if got := types(evs); len(got) != 2 {
		t.Fatalf("partial line must not parse: %v", got)
	}
	appendTo(t, path, `tle","aiTitle":"T","sessionId":"s1"}`+"\n")
	evs2, _, _ := tl.poll()
	if got := types(evs2); len(got) != 1 || got[0] != transcript.SessionTitle {
		t.Fatalf("completed line lost: %v", got)
	}
}
