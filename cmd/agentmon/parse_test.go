package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/incantery/agentmon/internal/redact"
)

// Fixture-derived events, in order: session_started, permission_mode,
// session_title, user_prompt, assistant_message, tool_call, tool_result,
// assistant_message, turn_completed = 9 lines of output.

func TestParseFullLevel(t *testing.T) {
	var out, errb bytes.Buffer
	if err := runParse(&out, &errb, "testdata/session.jsonl", redact.Full); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 9 {
		t.Fatalf("got %d event lines, want 9:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], `"type":"session_started"`) {
		t.Errorf("first event: %s", lines[0])
	}
	if !strings.Contains(lines[5], `"name":"Read"`) || !strings.Contains(lines[5], "login.go") {
		t.Errorf("tool_call should carry input at full level: %s", lines[5])
	}
	if !strings.Contains(lines[8], `"duration_ms":10500`) {
		t.Errorf("last event: %s", lines[8])
	}
	if !strings.Contains(errb.String(), "file-history-snapshot") || !strings.Contains(errb.String(), "malformed") {
		t.Errorf("skip summary missing: %s", errb.String())
	}
}

func TestParseMetadataLevelStripsContent(t *testing.T) {
	var out, errb bytes.Buffer
	if err := runParse(&out, &errb, "testdata/session.jsonl", redact.Metadata); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, secret := range []string{"login.go", "Fix the login bug please", "package main", "Looking now."} {
		if strings.Contains(s, secret) {
			t.Errorf("metadata output leaks %q", secret)
		}
	}
	// but structure survives
	if !strings.Contains(s, `"chars":24`) || !strings.Contains(s, `"name":"Read"`) {
		t.Errorf("metadata output missing expected fields:\n%s", s)
	}
	// the session title is explicitly allowed at metadata level (spec)
	if !strings.Contains(s, "Fix the login bug\"") {
		t.Errorf("session title should survive metadata level:\n%s", s)
	}
}
