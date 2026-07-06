package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/incantery/agentmon/internal/redact"
	"github.com/incantery/agentmon/internal/state"
	"github.com/incantery/agentmon/internal/transcript"
)

type collector struct{ events []transcript.Event }

func (c *collector) Emit(ev transcript.Event) error {
	c.events = append(c.events, ev)
	return nil
}

// newTestWatcher returns a watcher over a temp root plus helpers.
// clock is a *time.Time the test advances manually.
func newTestWatcher(t *testing.T, st *state.State, backfill bool) (*Watcher, *collector, string, *time.Time) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	c := &collector{}
	w := New(Options{
		Roots:      []string{root},
		Machine:    "m1",
		Level:      redact.Metadata,
		IdleAfter:  60 * time.Second,
		EndedAfter: 30 * time.Minute,
		Backfill:   backfill,
		Now:        func() time.Time { return now },
	}, st, c)
	return w, c, filepath.Join(root, "proj"), &now
}

func TestFirstSightingFastForwards(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, _ := newTestWatcher(t, st, false)
	path := filepath.Join(dir, "old.jsonl")
	write(t, path, line1+line2) // pre-existing history
	if err := w.PollOnce(); err != nil {
		t.Fatal(err)
	}
	if len(c.events) != 0 {
		t.Fatalf("fast-forward must not emit history, got %v", types(c.events))
	}
	// but new appends are emitted
	appendTo(t, path, `{"type":"permission-mode","permissionMode":"auto","sessionId":"old"}`+"\n")
	w.PollOnce()
	if got := types(c.events); len(got) != 1 || got[0] != transcript.PermissionMode {
		t.Fatalf("post-fast-forward append lost: %v", got)
	}
}

func TestBackfillEmitsHistory(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, _ := newTestWatcher(t, st, true)
	write(t, filepath.Join(dir, "old.jsonl"), line1)
	w.PollOnce()
	if got := types(c.events); len(got) != 2 {
		t.Fatalf("backfill should emit history: %v", got)
	}
}

func TestMachineStampAndRedaction(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, _ := newTestWatcher(t, st, true)
	write(t, filepath.Join(dir, "s1.jsonl"), line1)
	w.PollOnce()
	for _, ev := range c.events {
		if ev.Machine != "m1" {
			t.Errorf("machine not stamped: %+v", ev)
		}
	}
	// line1 is a user prompt "hello" — at metadata level the text must be gone
	last := c.events[len(c.events)-1]
	if p, ok := last.Payload.(transcript.UserPromptPayload); !ok || p.Text != "" || p.Chars != 5 {
		t.Errorf("redaction failed: %+v", last.Payload)
	}
}

func TestWatermarkPersistsAcrossWatchers(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	st, _ := state.Load(statePath)
	w, c, dir, _ := newTestWatcher(t, st, true)
	path := filepath.Join(dir, "s1.jsonl")
	write(t, path, line1+turnDoneLine)
	w.PollOnce()
	n := len(c.events)
	if n == 0 {
		t.Fatal("no events on first pass")
	}

	// "restart": fresh state from disk, fresh watcher over the same root
	st2, err := state.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	c2 := &collector{}
	w2 := New(Options{
		Roots: []string{filepath.Dir(dir)}, Machine: "m1", Level: redact.Metadata,
		IdleAfter: 60 * time.Second, EndedAfter: 30 * time.Minute,
		Now: func() time.Time { return time.Date(2026, 7, 6, 12, 15, 0, 0, time.UTC) },
	}, st2, c2)
	w2.PollOnce()
	if len(c2.events) != 0 {
		t.Fatalf("restart re-emitted %v", types(c2.events))
	}
	appendTo(t, path, line2)
	w2.PollOnce()
	if got := types(c2.events); len(got) != 1 || got[0] != transcript.SessionTitle {
		t.Fatalf("after restart+append: %v", got)
	}
}

func TestShrinkResetPersistedThenRestartDoesNotSkipContent(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	st, _ := state.Load(statePath)
	w, _, dir, _ := newTestWatcher(t, st, true)
	path := filepath.Join(dir, "s1.jsonl")
	write(t, path, line1+line2)
	w.PollOnce() // emits, watermark anchored

	// Rewrite smaller with a PARTIAL line: the tail's shrink-reset clears
	// the watermark, no complete line re-anchors it, and PollOnce persists
	// that unset watermark.
	write(t, path, `{"type":"ai-ti`)
	w.PollOnce()
	if st.File(path).Watermark.Set {
		t.Fatal("precondition: watermark should be unset after shrink+partial")
	}

	// Restart: fresh state from disk, fresh watcher (non-backfill, like a
	// real daemon restart). Completing the file must NOT be fast-forwarded
	// past.
	st2, err := state.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	c2 := &collector{}
	w2 := New(Options{
		Roots: []string{filepath.Dir(dir)}, Machine: "m1", Level: redact.Metadata,
		IdleAfter: 60 * time.Second, EndedAfter: 30 * time.Minute,
		Now: func() time.Time { return time.Date(2026, 7, 6, 12, 10, 0, 0, time.UTC) },
	}, st2, c2)
	appendTo(t, path, `tle","aiTitle":"T","sessionId":"s1"}`+"\n")
	w2.PollOnce()
	got := types(c2.events)
	if len(got) != 2 || got[0] != transcript.SessionStarted || got[1] != transcript.SessionTitle {
		t.Fatalf("rewritten content was skipped after restart: %v", got)
	}
}

func TestRemovedFileEmitsSessionEnded(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, _ := newTestWatcher(t, st, true)
	path := filepath.Join(dir, "s1.jsonl")
	write(t, path, line1)
	w.PollOnce()
	os.Remove(path)
	c.events = nil
	w.PollOnce()
	if len(c.events) != 1 || c.events[0].Type != transcript.SessionEnded {
		t.Fatalf("got %v", types(c.events))
	}
	if p := c.events[0].Payload.(transcript.SessionEndedPayload); p.Reason != "removed" {
		t.Errorf("reason = %q", p.Reason)
	}
	if _, ok := st.Files[path]; ok {
		t.Error("state entry not deleted")
	}
	c.events = nil
	w.PollOnce()
	if len(c.events) != 0 {
		t.Error("removed file reported twice")
	}
}

// midTurnLine leaves the session mid-turn (assistant message, no turn_completed)
const midTurnLine = `{"type":"assistant","message":{"model":"m","role":"assistant","content":[{"type":"text","text":"working"}],"usage":{"input_tokens":1,"output_tokens":1}},"timestamp":"2026-07-06T12:00:01.000Z","cwd":"/p","sessionId":"s1"}` + "\n"
const turnDoneLine = `{"type":"system","subtype":"turn_duration","durationMs":5,"messageCount":2,"timestamp":"2026-07-06T12:00:02.000Z","sessionId":"s1"}` + "\n"

func TestIdleFiresOnceMidTurnAndRearms(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, now := newTestWatcher(t, st, true)
	path := filepath.Join(dir, "s1.jsonl")
	write(t, path, line1+midTurnLine)
	w.PollOnce() // emits + records activity, MidTurn=true
	c.events = nil

	*now = now.Add(61 * time.Second)
	w.PollOnce()
	if got := types(c.events); len(got) != 1 || got[0] != transcript.SessionIdle {
		t.Fatalf("want one session_idle, got %v", got)
	}
	if p := c.events[0].Payload.(transcript.SessionIdlePayload); p.IdleSeconds < 60 {
		t.Errorf("idle_seconds = %d", p.IdleSeconds)
	}
	c.events = nil
	*now = now.Add(10 * time.Second)
	w.PollOnce()
	if len(c.events) != 0 {
		t.Fatalf("idle fired twice: %v", types(c.events))
	}
	// activity re-arms
	appendTo(t, path, midTurnLine)
	w.PollOnce()
	c.events = nil
	*now = now.Add(61 * time.Second)
	w.PollOnce()
	if got := types(c.events); len(got) != 1 || got[0] != transcript.SessionIdle {
		t.Fatalf("idle did not re-arm: %v", got)
	}
}

func TestNoIdleAfterTurnCompleted(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, now := newTestWatcher(t, st, true)
	write(t, filepath.Join(dir, "s1.jsonl"), line1+midTurnLine+turnDoneLine)
	w.PollOnce()
	c.events = nil
	*now = now.Add(2 * time.Minute)
	w.PollOnce()
	if len(c.events) != 0 {
		t.Fatalf("idle fired after turn_completed: %v", types(c.events))
	}
}

func TestEndedFiresOnceThenActivityResumes(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, now := newTestWatcher(t, st, true)
	path := filepath.Join(dir, "s1.jsonl")
	write(t, path, line1+turnDoneLine)
	w.PollOnce()
	c.events = nil
	*now = now.Add(31 * time.Minute)
	w.PollOnce()
	if got := types(c.events); len(got) != 1 || got[0] != transcript.SessionEnded {
		t.Fatalf("want one session_ended, got %v", got)
	}
	if p := c.events[0].Payload.(transcript.SessionEndedPayload); p.Reason != "inactive" {
		t.Errorf("reason = %q", p.Reason)
	}
	c.events = nil
	*now = now.Add(1 * time.Minute)
	w.PollOnce()
	if len(c.events) != 0 {
		t.Fatalf("ended fired twice: %v", types(c.events))
	}
	appendTo(t, path, midTurnLine)
	w.PollOnce()
	if got := types(c.events); len(got) != 1 || got[0] != transcript.AssistantMessage {
		t.Fatalf("resume after ended: %v", got)
	}
}

func TestSyntheticAfterFastForwardDoesNotSwallowResume(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, now := newTestWatcher(t, st, false) // non-backfill: fast-forward
	path := filepath.Join(dir, "s1.jsonl")
	write(t, path, line1) // pre-existing content, recent mtime
	// Anchor mtime to the test's fake clock rather than real wall-clock
	// time: initTimers seeds activity from fi.ModTime(), and this test
	// must be deterministic regardless of when it actually runs.
	if err := os.Chtimes(path, *now, *now); err != nil {
		t.Fatal(err)
	}
	w.PollOnce() // first sighting: fast-forward, no events
	*now = now.Add(31 * time.Minute)
	w.PollOnce() // session_ended{inactive}
	if got := types(c.events); len(got) != 1 || got[0] != transcript.SessionEnded {
		t.Fatalf("setup: want one session_ended, got %v", got)
	}
	if c.events[0].Seq >= 0 {
		t.Errorf("synthetic seq must be negative, got %d", c.events[0].Seq)
	}
	c.events = nil
	// The session resumes: the appended line's events must NOT be covered.
	appendTo(t, path, `{"type":"permission-mode","permissionMode":"auto","sessionId":"s1"}`+"\n")
	w.PollOnce()
	if got := types(c.events); len(got) != 1 || got[0] != transcript.PermissionMode {
		t.Fatalf("resumed session's events were swallowed: %v", got)
	}
}

func TestStaleStateEntriesPrunedOnRestart(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	st, _ := state.Load(statePath)
	w, _, dir, _ := newTestWatcher(t, st, true)
	path := filepath.Join(dir, "s1.jsonl")
	write(t, path, line1)
	w.PollOnce() // tracked + persisted
	os.Remove(path)
	// simulate downtime: fresh state + fresh watcher, file already gone
	st2, _ := state.Load(statePath)
	if _, ok := st2.Files[path]; !ok {
		t.Fatal("setup: entry should be persisted")
	}
	c2 := &collector{}
	w2 := New(Options{
		Roots: []string{filepath.Dir(dir)}, Machine: "m1", Level: redact.Metadata,
		IdleAfter: 60 * time.Second, EndedAfter: 30 * time.Minute,
		Now: func() time.Time { return time.Date(2026, 7, 6, 12, 5, 0, 0, time.UTC) },
	}, st2, c2)
	w2.PollOnce()
	if _, ok := st2.Files[path]; ok {
		t.Error("stale entry not pruned")
	}
	if len(c2.events) != 0 {
		t.Errorf("silent prune must emit nothing, got %v", types(c2.events))
	}
}

func TestHistoricalFilesGrandfatheredSilently(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, now := newTestWatcher(t, st, false)
	path := filepath.Join(dir, "ancient.jsonl")
	write(t, path, line1)
	oldTime := now.Add(-48 * time.Hour)
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	w.PollOnce()
	*now = now.Add(31 * time.Minute)
	w.PollOnce()
	if len(c.events) != 0 {
		t.Fatalf("grandfathered file emitted %v", types(c.events))
	}
	if !st.File(path).Ended {
		t.Error("ancient file not marked Ended")
	}
}
