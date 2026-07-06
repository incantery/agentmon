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
	write(t, path, line1)
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
		Now: func() time.Time { return time.Date(2026, 7, 6, 13, 0, 0, 0, time.UTC) },
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
