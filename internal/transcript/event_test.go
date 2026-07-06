package transcript

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEventJSON(t *testing.T) {
	ev := Event{
		Project:   "/p",
		SessionID: "s",
		Offset:    42,
		Seq:       1,
		TS:        time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC),
		Type:      SessionTitle,
		Payload:   SessionTitlePayload{Title: "hi"},
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"project":"/p","session_id":"s","offset":42,"seq":1,"ts":"2026-07-06T10:00:00Z","type":"session_title","payload":{"title":"hi"}}`
	if string(b) != want {
		t.Errorf("got:\n%s\nwant:\n%s", b, want)
	}
}

func TestZeroTimeAndMachineOmitted(t *testing.T) {
	b, err := json.Marshal(Event{SessionID: "s", Type: PermissionMode, Payload: PermissionModePayload{Mode: "auto"}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"session_id":"s","offset":0,"seq":0,"type":"permission_mode","payload":{"mode":"auto"}}`
	if string(b) != want {
		t.Errorf("got:\n%s\nwant:\n%s", b, want)
	}
}

func TestEveryEventTypeHasExactlyOnePayload(t *testing.T) {
	seen := map[EventType]int{}
	for _, pl := range AllPayloads() {
		seen[pl.EventType()]++
	}
	for _, et := range AllEventTypes {
		if seen[et] != 1 {
			t.Errorf("event type %q has %d payloads, want 1", et, seen[et])
		}
	}
	if len(AllPayloads()) != len(AllEventTypes) {
		t.Errorf("AllPayloads has %d entries, AllEventTypes has %d", len(AllPayloads()), len(AllEventTypes))
	}
}
