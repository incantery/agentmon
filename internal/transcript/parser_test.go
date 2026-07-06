package transcript

import (
	"reflect"
	"testing"
	"time"
)

// collect feeds lines to a parser sequentially, advancing offsets the way
// the real file reader does (offset = byte offset of line start).
func collect(t *testing.T, p *Parser, lines ...string) []Event {
	t.Helper()
	var out []Event
	var off int64
	for _, l := range lines {
		out = append(out, p.Line(off, []byte(l+"\n"))...)
		off += int64(len(l) + 1)
	}
	return out
}

func TestFirstValidLineEmitsSessionStarted(t *testing.T) {
	p := NewParser("sess-1")
	got := collect(t, p,
		`{"type":"permission-mode","permissionMode":"auto","sessionId":"sess-1"}`,
	)
	want := []Event{
		{SessionID: "sess-1", Offset: 0, Seq: 0, Type: SessionStarted, Payload: SessionStartedPayload{}},
		{SessionID: "sess-1", Offset: 0, Seq: 1, Type: PermissionMode, Payload: PermissionModePayload{Mode: "auto"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestAITitle(t *testing.T) {
	p := NewParser("sess-1")
	got := collect(t, p,
		`{"type":"ai-title","aiTitle":"Fix the login bug","sessionId":"sess-1"}`,
	)
	if len(got) != 2 || got[1].Type != SessionTitle {
		t.Fatalf("got %+v", got)
	}
	if got[1].Payload.(SessionTitlePayload).Title != "Fix the login bug" {
		t.Errorf("payload = %+v", got[1].Payload)
	}
}

func TestUnknownTypesAreCountedNotFatal(t *testing.T) {
	p := NewParser("sess-1")
	got := collect(t, p,
		`{"type":"file-history-snapshot","messageId":"m1"}`,
		`{not json`,
		``,
		`{"type":"file-history-snapshot","messageId":"m2"}`,
	)
	// first valid line still triggers session_started, nothing else
	if len(got) != 1 || got[0].Type != SessionStarted {
		t.Fatalf("got %+v", got)
	}
	if p.Skipped["file-history-snapshot"] != 2 {
		t.Errorf("Skipped = %v", p.Skipped)
	}
	if p.Skipped["malformed"] != 1 {
		t.Errorf("Skipped = %v", p.Skipped)
	}
}

func TestTimestampCarryAndProjectFromCWD(t *testing.T) {
	p := NewParser("sess-1")
	got := collect(t, p,
		`{"type":"ai-title","aiTitle":"early, no ts","sessionId":"sess-1"}`,
		`{"type":"user","message":{"role":"user","content":"hi"},"timestamp":"2026-07-06T10:00:00.000Z","cwd":"/home/u/proj","sessionId":"sess-1"}`,
		`{"type":"ai-title","aiTitle":"late, no ts","sessionId":"sess-1"}`,
	)
	ts := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	// events before any timestamp have zero TS
	if !got[0].TS.IsZero() || !got[1].TS.IsZero() {
		t.Errorf("early events should have zero TS: %+v", got[:2])
	}
	last := got[len(got)-1]
	if last.Type != SessionTitle || !last.TS.Equal(ts) {
		t.Errorf("late title should carry last-seen ts: %+v", last)
	}
	if last.Project != "/home/u/proj" {
		t.Errorf("project = %q, want /home/u/proj", last.Project)
	}
}
