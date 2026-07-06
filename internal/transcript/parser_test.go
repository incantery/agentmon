package transcript

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

func TestUserPromptString(t *testing.T) {
	p := NewParser("sess-1")
	got := collect(t, p,
		`{"type":"user","message":{"role":"user","content":"Fix the login bug please"},"timestamp":"2026-07-06T10:00:00.000Z","cwd":"/home/u/proj","sessionId":"sess-1"}`,
	)
	// session_started + user_prompt
	if len(got) != 2 {
		t.Fatalf("got %d events: %+v", len(got), got)
	}
	want := UserPromptPayload{Chars: 24, Text: "Fix the login bug please"}
	if got[1].Type != UserPrompt || got[1].Payload.(UserPromptPayload) != want {
		t.Errorf("got %+v, want %+v", got[1], want)
	}
}

func TestUserToolResults(t *testing.T) {
	p := NewParser("sess-1")
	got := collect(t, p,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"package main"},{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"boom"}],"is_error":true}]},"timestamp":"2026-07-06T10:00:06.000Z","sessionId":"sess-1"}`,
	)
	if len(got) != 3 { // session_started + 2 tool_results
		t.Fatalf("got %d events: %+v", len(got), got)
	}
	r1 := got[1].Payload.(ToolResultPayload)
	r2 := got[2].Payload.(ToolResultPayload)
	if !r1.OK || r1.Content != "package main" {
		t.Errorf("r1 = %+v", r1)
	}
	if r2.OK || r2.Content != "boom" {
		t.Errorf("r2 = %+v", r2)
	}
	if got[1].Seq != 1 || got[2].Seq != 2 {
		t.Errorf("seq = %d, %d", got[1].Seq, got[2].Seq)
	}
}

func TestUserTextBlocksBecomeOnePrompt(t *testing.T) {
	p := NewParser("sess-1")
	got := collect(t, p,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"line one"},{"type":"text","text":"line two"}]},"sessionId":"sess-1"}`,
	)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	pl := got[1].Payload.(UserPromptPayload)
	if pl.Text != "line one\nline two" || pl.Chars != 17 {
		t.Errorf("payload = %+v", pl)
	}
}

func TestLongPromptTruncatedButCharsExact(t *testing.T) {
	long := ""
	for i := 0; i < 3000; i++ {
		long += "a"
	}
	p := NewParser("sess-1")
	got := collect(t, p,
		`{"type":"user","message":{"role":"user","content":"`+long+`"},"sessionId":"sess-1"}`,
	)
	pl := got[1].Payload.(UserPromptPayload)
	if pl.Chars != 3000 {
		t.Errorf("Chars = %d, want 3000", pl.Chars)
	}
	if len(pl.Text) != MaxContentBytes+len("…") {
		t.Errorf("len(Text) = %d, want %d", len(pl.Text), MaxContentBytes+len("…"))
	}
}

func TestUserLineSkipCounters(t *testing.T) {
	p := NewParser("sess-1")
	got := collect(t, p,
		`{"type":"user","message":"not an object","sessionId":"sess-1"}`,
		`{"type":"user","message":{"role":"user","content":null},"sessionId":"sess-1"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"image","source":"x"}]},"sessionId":"sess-1"}`,
	)
	if len(got) != 1 || got[0].Type != SessionStarted {
		t.Fatalf("expected only session_started, got %+v", got)
	}
	if p.Skipped["user:badmessage"] != 1 || p.Skipped["user:badcontent"] != 1 || p.Skipped["user:block:image"] != 1 {
		t.Errorf("Skipped = %v", p.Skipped)
	}
}

func TestAssistantMessageWithToolUse(t *testing.T) {
	p := NewParser("sess-1")
	got := collect(t, p,
		`{"type":"assistant","message":{"model":"claude-fable-5","role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Looking now."},{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/home/u/proj/login.go"}}],"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":25,"cache_read_input_tokens":50,"cache_creation_input_tokens":10}},"timestamp":"2026-07-06T10:00:05.000Z","cwd":"/home/u/proj","sessionId":"sess-1"}`,
	)
	if len(got) != 3 { // session_started + assistant_message + tool_call
		t.Fatalf("got %d events: %+v", len(got), got)
	}
	am := got[1].Payload.(AssistantMessagePayload)
	want := AssistantMessagePayload{
		Model: "claude-fable-5", InputTokens: 100, OutputTokens: 25,
		CacheReadTokens: 50, CacheCreationTokens: 10,
		StopReason: "tool_use", Text: "Looking now.",
	}
	// claude-fable-5 is priced, so the parser stamps CostUSD; assert it's
	// non-nil separately, then align the pointer so the rest of the
	// struct can still be compared with ==.
	if am.CostUSD == nil {
		t.Fatal("fable-5 must be priced")
	}
	want.CostUSD = am.CostUSD
	if am != want {
		t.Errorf("assistant_message = %+v, want %+v", am, want)
	}
	tc := got[2].Payload.(ToolCallPayload)
	if tc.Name != "Read" || tc.Input != `{"file_path":"/home/u/proj/login.go"}` {
		t.Errorf("tool_call = %+v", tc)
	}
	if p.Skipped["assistant:block:thinking"] != 0 {
		t.Errorf("thinking must be silently dropped, not counted: %v", p.Skipped)
	}
}

func TestAssistantTextOnly(t *testing.T) {
	p := NewParser("sess-1")
	got := collect(t, p,
		`{"type":"assistant","message":{"model":"claude-fable-5","role":"assistant","content":[{"type":"text","text":"Fixed."}],"stop_reason":"end_turn","usage":{"input_tokens":200,"output_tokens":10}},"timestamp":"2026-07-06T10:00:10.000Z","sessionId":"sess-1"}`,
	)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	am := got[1].Payload.(AssistantMessagePayload)
	if am.StopReason != "end_turn" || am.Text != "Fixed." || am.InputTokens != 200 {
		t.Errorf("assistant_message = %+v", am)
	}
}

func TestAssistantLineSkipCounters(t *testing.T) {
	p := NewParser("sess-1")
	got := collect(t, p,
		`{"type":"assistant","message":"not an object","sessionId":"sess-1"}`,
		`{"type":"assistant","message":{"model":"m","role":"assistant","content":[{"type":"server_tool_use","name":"x"},{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":2}},"sessionId":"sess-1"}`,
	)
	// session_started + one assistant_message (from the second line)
	if len(got) != 2 || got[1].Type != AssistantMessage {
		t.Fatalf("got %+v", got)
	}
	am := got[1].Payload.(AssistantMessagePayload)
	if am.Text != "ok" || am.InputTokens != 1 {
		t.Errorf("assistant_message = %+v", am)
	}
	if p.Skipped["assistant:badmessage"] != 1 || p.Skipped["assistant:block:server_tool_use"] != 1 {
		t.Errorf("Skipped = %v", p.Skipped)
	}
}

func TestSystemTurnDuration(t *testing.T) {
	p := NewParser("sess-1")
	got := collect(t, p,
		`{"type":"system","subtype":"turn_duration","durationMs":296959,"messageCount":87,"timestamp":"2026-07-06T10:26:06.551Z","sessionId":"sess-1"}`,
		`{"type":"system","subtype":"something_new","sessionId":"sess-1"}`,
	)
	if len(got) != 2 { // session_started + turn_completed
		t.Fatalf("got %+v", got)
	}
	tc := got[1].Payload.(TurnCompletedPayload)
	if tc.DurationMs != 296959 || tc.Messages != 87 {
		t.Errorf("turn_completed = %+v", tc)
	}
	if p.Skipped["system:something_new"] != 1 {
		t.Errorf("Skipped = %v", p.Skipped)
	}
}

func TestAssistantCostStamping(t *testing.T) {
	p := NewParser("sess-1")
	got := collect(t, p,
		`{"type":"assistant","message":{"model":"claude-fable-5","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":6199,"output_tokens":383,"cache_read_input_tokens":18341,"cache_creation_input_tokens":3899,"cache_creation":{"ephemeral_5m_input_tokens":3000,"ephemeral_1h_input_tokens":899}}},"timestamp":"2026-07-08T10:00:00.000Z","sessionId":"sess-1"}`,
	)
	am := got[1].Payload.(AssistantMessagePayload)
	if am.Cache5mTokens != 3000 || am.Cache1hTokens != 899 {
		t.Errorf("cache split = %d/%d", am.Cache5mTokens, am.Cache1hTokens)
	}
	if am.CostUSD == nil {
		t.Fatal("fable-5 must be priced")
	}
	want := 6199*10.0/1e6 + 383*50.0/1e6 + 18341*1.0/1e6 + 3000*12.5/1e6 + 899*20.0/1e6
	if diff := *am.CostUSD - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost = %v want %v", *am.CostUSD, want)
	}
}

func TestAssistantCostFallbackWithoutSplit(t *testing.T) {
	p := NewParser("sess-1")
	got := collect(t, p,
		`{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":100,"output_tokens":10,"cache_creation_input_tokens":1000}},"sessionId":"sess-1"}`,
	)
	am := got[1].Payload.(AssistantMessagePayload)
	if am.CostUSD == nil {
		t.Fatal("haiku must be priced via prefix")
	}
	// no split → all cache_creation treated as 5m (1.25×)
	want := 100*1.0/1e6 + 10*5.0/1e6 + 1000*1.25/1e6
	if diff := *am.CostUSD - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost = %v want %v", *am.CostUSD, want)
	}
}

func TestUnknownModelUnpriced(t *testing.T) {
	p := NewParser("sess-1")
	got := collect(t, p,
		`{"type":"assistant","message":{"model":"qwen3:8b","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":100,"output_tokens":10}},"sessionId":"sess-1"}`,
	)
	am := got[1].Payload.(AssistantMessagePayload)
	if am.CostUSD != nil {
		t.Errorf("unknown model must be unpriced (nil), got %v", *am.CostUSD)
	}
	// and the JSON must omit the field entirely
	b, _ := json.Marshal(am)
	if strings.Contains(string(b), "cost_usd") {
		t.Errorf("unpriced payload must omit cost_usd: %s", b)
	}
}

func TestTruncateInvalidUTF8EarlyByteDoesNotWipePrefix(t *testing.T) {
	s := `{"k":"` + "\xff" + strings.Repeat("a", 3000) + `"}`
	got := truncate(s)
	if len(got) < MaxContentBytes-utf8.UTFMax {
		t.Fatalf("truncate destroyed content: len=%d, want ~%d", len(got), MaxContentBytes)
	}
	if len(got) > MaxContentBytes+len("…") {
		t.Fatalf("truncate result too long: %d", len(got))
	}
}
