# agentmon Milestone 1: Transcript Parser Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A pure Go library that turns Claude Code transcript JSONL into agentmon's derived-event stream, a redaction layer enforcing content levels, and an `agentmon parse` debug command proving it end to end.

**Architecture:** `internal/transcript` is a pure, line-driven parser (bytes in, `[]Event` out — no I/O), `internal/redact` strips content fields for `metadata` level, and `cmd/agentmon` wraps them in a `parse` subcommand that streams a transcript file. Spec: `docs/superpowers/specs/2026-07-06-agentmon-design.md`.

**Tech Stack:** Go 1.25, stdlib only (encoding/json, bufio, flag). No external dependencies in this milestone.

## Global Constraints

- Module path: `github.com/incantery/agentmon`; Go 1.25; **stdlib only**.
- Unknown/new line types, unknown content blocks, and malformed JSON lines must **never** return an error or panic — they are counted in `Parser.Skipped` and skipped.
- Content fields (`text`, `input`, `content`) are truncated to `MaxContentBytes = 2048` bytes (UTF-8 safe) even at `full` level.
- `thinking` blocks are dropped entirely — never shipped at any level.
- Redaction happens by `redact.Apply`; at `metadata` level no prompt/file/tool content may survive in any payload field.
- Every task ends with `go build ./... && go test ./...` green and a commit.

## Transcript format reference (verified against real files, Claude Code v2.1.200)

One file per session: `~/.claude/projects/<encoded-project>/<session-uuid>.jsonl`. Each line is a JSON object with a `type` field. Types this milestone consumes:

| line `type` | fields we use |
|---|---|
| `user` | `message.content` (string = human prompt, OR array of blocks: `text`, `tool_result{content, is_error}`), `timestamp`, `cwd` |
| `assistant` | `message.model`, `message.stop_reason`, `message.usage.{input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens}`, `message.content` blocks (`text`, `tool_use{name,input}`, `thinking`), `timestamp`, `cwd` |
| `system` | `subtype` — only `"turn_duration"`: `durationMs`, `messageCount`, `timestamp` |
| `ai-title` | `aiTitle` |
| `permission-mode` | `permissionMode` |

Observed-but-skipped types: `mode`, `last-prompt`, `file-history-snapshot`, `queue-operation`, `attachment`. Some lines (`ai-title`, `permission-mode`, …) have **no timestamp** — the parser carries the last-seen timestamp forward (zero time before any is seen). `tool_result.content` is either a string or an array of `{type:"text",text}` blocks.

---

### Task 1: Module scaffold + event model

**Files:**
- Create: `go.mod`, `.gitignore`, `Makefile`
- Create: `internal/transcript/event.go`
- Test: `internal/transcript/event_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `transcript.Event` struct; `transcript.EventType` (string) with consts `SessionStarted, SessionTitle, UserPrompt, AssistantMessage, ToolCall, ToolResult, PermissionMode, TurnCompleted`; `transcript.Payload` interface `{ EventType() EventType }`; payload structs `SessionStartedPayload{CWD}`, `SessionTitlePayload{Title}`, `UserPromptPayload{Chars, Text}`, `AssistantMessagePayload{Model, InputTokens, OutputTokens, CacheReadTokens, CacheCreationTokens, StopReason, Text}`, `ToolCallPayload{Name, Input}`, `ToolResultPayload{OK, Content}`, `PermissionModePayload{Mode}`, `TurnCompletedPayload{DurationMs, Messages}`; `AllEventTypes []EventType`; `AllPayloads() []Payload`.

- [ ] **Step 1: Scaffold the module**

```bash
cd /Users/sethlowie/go/src/github.com/incantery/agentmon
go mod init github.com/incantery/agentmon
printf 'bin/\n' > .gitignore
cat > Makefile <<'EOF'
build:
	go build -o bin/agentmon ./cmd/agentmon

test:
	go test ./...

.PHONY: build test
EOF
```

Then edit `go.mod` so the go directive reads `go 1.25` (needed for the `omitzero` JSON tag).

- [ ] **Step 2: Write the failing test**

`internal/transcript/event_test.go`:

```go
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
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/transcript/`
Expected: FAIL — `undefined: Event` (package doesn't compile yet).

- [ ] **Step 4: Write the implementation**

`internal/transcript/event.go`:

```go
// Package transcript parses Claude Code session transcript JSONL into
// agentmon's derived-event stream. It is pure: bytes in, events out.
package transcript

import "time"

type EventType string

const (
	SessionStarted   EventType = "session_started"
	SessionTitle     EventType = "session_title"
	UserPrompt       EventType = "user_prompt"
	AssistantMessage EventType = "assistant_message"
	ToolCall         EventType = "tool_call"
	ToolResult       EventType = "tool_result"
	PermissionMode   EventType = "permission_mode"
	TurnCompleted    EventType = "turn_completed"
)

// AllEventTypes lists every event type this package can produce.
// Watcher-produced types (session_idle, session_ended, spool_evicted)
// belong to milestone 2, not here.
var AllEventTypes = []EventType{
	SessionStarted, SessionTitle, UserPrompt, AssistantMessage,
	ToolCall, ToolResult, PermissionMode, TurnCompleted,
}

// Event is the envelope defined in the design spec. Identity for
// server-side dedupe is (machine, session_id, offset, seq); Machine is
// stamped by the shipper, not the parser.
type Event struct {
	Machine   string    `json:"machine,omitempty"`
	Project   string    `json:"project,omitempty"`
	SessionID string    `json:"session_id"`
	Offset    int64     `json:"offset"`
	Seq       int       `json:"seq"`
	TS        time.Time `json:"ts,omitzero"`
	Type      EventType `json:"type"`
	Payload   Payload   `json:"payload"`
}

type Payload interface {
	EventType() EventType
}

type SessionStartedPayload struct {
	CWD string `json:"cwd,omitempty"`
}

type SessionTitlePayload struct {
	Title string `json:"title"`
}

type UserPromptPayload struct {
	Chars int    `json:"chars"`
	Text  string `json:"text,omitempty"` // cleared at metadata level
}

type AssistantMessagePayload struct {
	Model               string `json:"model,omitempty"`
	InputTokens         int64  `json:"input_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64  `json:"cache_creation_tokens,omitempty"`
	StopReason          string `json:"stop_reason,omitempty"`
	Text                string `json:"text,omitempty"` // cleared at metadata level
}

type ToolCallPayload struct {
	Name  string `json:"name"`
	Input string `json:"input,omitempty"` // cleared at metadata level
}

type ToolResultPayload struct {
	OK      bool   `json:"ok"`
	Content string `json:"content,omitempty"` // cleared at metadata level
}

type PermissionModePayload struct {
	Mode string `json:"mode"`
}

type TurnCompletedPayload struct {
	DurationMs int64 `json:"duration_ms"`
	Messages   int   `json:"messages"`
}

func (SessionStartedPayload) EventType() EventType   { return SessionStarted }
func (SessionTitlePayload) EventType() EventType     { return SessionTitle }
func (UserPromptPayload) EventType() EventType       { return UserPrompt }
func (AssistantMessagePayload) EventType() EventType { return AssistantMessage }
func (ToolCallPayload) EventType() EventType         { return ToolCall }
func (ToolResultPayload) EventType() EventType       { return ToolResult }
func (PermissionModePayload) EventType() EventType   { return PermissionMode }
func (TurnCompletedPayload) EventType() EventType    { return TurnCompleted }

// AllPayloads returns a zero value of every payload type. The redact
// package's property test walks this list, so every new payload type
// MUST be added here.
func AllPayloads() []Payload {
	return []Payload{
		SessionStartedPayload{}, SessionTitlePayload{}, UserPromptPayload{},
		AssistantMessagePayload{}, ToolCallPayload{}, ToolResultPayload{},
		PermissionModePayload{}, TurnCompletedPayload{},
	}
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/transcript/`
Expected: PASS (3 tests).

- [ ] **Step 6: Commit**

```bash
git add go.mod .gitignore Makefile internal/transcript/
git commit -m "feat(transcript): module scaffold + derived-event model"
```

---

### Task 2: Parser core — lifecycle, simple lines, tolerance

**Files:**
- Create: `internal/transcript/parser.go`
- Test: `internal/transcript/parser_test.go`

**Interfaces:**
- Consumes: Task 1's `Event`, `Payload`, payload structs.
- Produces: `transcript.NewParser(sessionID string) *Parser`; `(*Parser).Line(offset int64, data []byte) []Event`; `Parser.Skipped map[string]int`. Later tasks add `case` arms to `Line`'s type switch and helper methods on `*Parser`.

- [ ] **Step 1: Write the failing tests**

`internal/transcript/parser_test.go`:

```go
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
```

Note: `TestTimestampCarryAndProjectFromCWD` will only fully pass after Task 3 adds `user` handling — but the parts it asserts (timestamp carry, project capture) come from the shared line preamble written in this task, and the `user` line contributes no events yet, so it passes now and keeps passing.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/transcript/`
Expected: FAIL — `undefined: NewParser`.

- [ ] **Step 3: Write the implementation**

`internal/transcript/parser.go`:

```go
package transcript

import (
	"bytes"
	"encoding/json"
	"time"
	"unicode/utf8"
)

// MaxContentBytes caps every content field (prompt text, tool input,
// tool-result content) even at full level.
const MaxContentBytes = 2048

// Parser derives events from one session's transcript lines, fed in file
// order. It is not safe for concurrent use.
type Parser struct {
	sessionID string
	project   string // first-seen cwd
	started   bool
	lastTS    time.Time

	// Skipped counts line/block types that produced no events, keyed by
	// type (plus "malformed" for undecodable lines). New Claude Code
	// releases must surface here, never as errors.
	Skipped map[string]int
}

func NewParser(sessionID string) *Parser {
	return &Parser{sessionID: sessionID, Skipped: map[string]int{}}
}

// rawLine covers the union of top-level fields across observed line types
// (Claude Code v2.1.200). Missing fields simply stay zero.
type rawLine struct {
	Type           string          `json:"type"`
	Timestamp      string          `json:"timestamp"`
	CWD            string          `json:"cwd"`
	AITitle        string          `json:"aiTitle"`
	PermissionMode string          `json:"permissionMode"`
	Subtype        string          `json:"subtype"`
	DurationMs     int64           `json:"durationMs"`
	MessageCount   int             `json:"messageCount"`
	Message        json.RawMessage `json:"message"`
}

// Line parses one transcript line (offset = byte offset of the line start
// in the file) and returns the derived events, possibly none.
func (p *Parser) Line(offset int64, data []byte) []Event {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil
	}
	var rl rawLine
	if err := json.Unmarshal(data, &rl); err != nil || rl.Type == "" {
		p.Skipped["malformed"]++
		return nil
	}
	ts := p.lastTS
	if rl.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339Nano, rl.Timestamp); err == nil {
			ts = t
			p.lastTS = t
		}
	}
	if rl.CWD != "" && p.project == "" {
		p.project = rl.CWD
	}

	var payloads []Payload
	if !p.started {
		p.started = true
		payloads = append(payloads, SessionStartedPayload{CWD: rl.CWD})
	}

	switch rl.Type {
	case "ai-title":
		payloads = append(payloads, SessionTitlePayload{Title: rl.AITitle})
	case "permission-mode":
		payloads = append(payloads, PermissionModePayload{Mode: rl.PermissionMode})
	default:
		p.Skipped[rl.Type]++
	}

	events := make([]Event, len(payloads))
	for i, pl := range payloads {
		events[i] = Event{
			Project:   p.project,
			SessionID: p.sessionID,
			Offset:    offset,
			Seq:       i,
			TS:        ts,
			Type:      pl.EventType(),
			Payload:   pl,
		}
	}
	return events
}

// truncate caps s at MaxContentBytes, trimming back to a UTF-8 boundary
// and appending an ellipsis when cut.
func truncate(s string) string {
	if len(s) <= MaxContentBytes {
		return s
	}
	cut := s[:MaxContentBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}
```

Note: `truncate` is unused until Task 3; Go allows unused package-level funcs, so the build stays green. `events` built from an empty `payloads` returns an empty non-nil slice — `collect` in tests uses `append`, which treats it the same as nil.

One subtlety: in `TestUnknownTypesAreCountedNotFatal` the first valid line is unknown-typed, so it emits `session_started` AND counts the type as skipped. That is intended behavior (the skip counter tracks line types that contributed no *type-specific* events).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/transcript/`
Expected: PASS. If `TestFirstValidLineEmitsSessionStarted` fails on a `reflect.DeepEqual` mismatch of empty-vs-nil, compare lengths and elements instead — but as written, both sides are non-nil slices of 2, so it passes.

- [ ] **Step 5: Commit**

```bash
git add internal/transcript/parser.go internal/transcript/parser_test.go
git commit -m "feat(transcript): line-driven parser core — lifecycle, titles, permission mode, tolerance"
```

---

### Task 3: `user` lines — prompts and tool results

**Files:**
- Modify: `internal/transcript/parser.go` (add `case "user":` + helpers)
- Test: `internal/transcript/parser_test.go` (append tests)

**Interfaces:**
- Consumes: Task 2's `Parser`, `rawLine`, `truncate`.
- Produces: `(*Parser).userPayloads(rl rawLine) []Payload` (unexported); `rawMessage` and `rawBlock` structs reused by Task 4; `flattenContent(raw json.RawMessage) string`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/transcript/parser_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/transcript/`
Expected: the four new tests FAIL (user lines currently fall into `default:` and produce nothing beyond `session_started`).

- [ ] **Step 3: Write the implementation**

In `parser.go`, add to the type switch in `Line`:

```go
	case "user":
		payloads = append(payloads, p.userPayloads(rl)...)
```

and append to the file:

```go
// rawMessage covers .message on user and assistant lines.
type rawMessage struct {
	Role       string          `json:"role"`
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"`
	Usage      struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	Content json.RawMessage `json:"content"` // string or []rawBlock
}

// rawBlock covers content blocks on user and assistant messages, and the
// blocks inside a tool_result's own content array.
type rawBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

func (p *Parser) userPayloads(rl rawLine) []Payload {
	var msg rawMessage
	if err := json.Unmarshal(rl.Message, &msg); err != nil {
		p.Skipped["user:badmessage"]++
		return nil
	}
	// A plain string content is a human prompt.
	var s string
	if err := json.Unmarshal(msg.Content, &s); err == nil {
		return []Payload{UserPromptPayload{Chars: utf8.RuneCountInString(s), Text: truncate(s)}}
	}
	var blocks []rawBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		p.Skipped["user:badcontent"]++
		return nil
	}
	var out []Payload
	var text bytes.Buffer
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(b.Text)
		case "tool_result":
			out = append(out, ToolResultPayload{OK: !b.IsError, Content: truncate(flattenContent(b.Content))})
		default:
			p.Skipped["user:block:"+b.Type]++
		}
	}
	if text.Len() > 0 {
		prompt := UserPromptPayload{Chars: utf8.RuneCountInString(text.String()), Text: truncate(text.String())}
		out = append([]Payload{prompt}, out...)
	}
	return out
}

// flattenContent renders a tool_result content field (string, or array of
// text blocks) to plain text.
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []rawBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var buf bytes.Buffer
	for _, b := range blocks {
		if b.Type == "text" {
			if buf.Len() > 0 {
				buf.WriteByte('\n')
			}
			buf.WriteString(b.Text)
		}
	}
	return buf.String()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/transcript/`
Expected: PASS, including all Task-2 tests (the `user` line in `TestTimestampCarryAndProjectFromCWD` now also emits a `user_prompt`, which that test tolerates because it only inspects `got[0]`, `got[1]`, and the last event — verify this; if the count shifted the "last" assertion, the last event is still the late `ai-title` title, so it holds).

- [ ] **Step 5: Commit**

```bash
git add internal/transcript/
git commit -m "feat(transcript): derive user_prompt and tool_result from user lines"
```

---

### Task 4: `assistant` lines — messages and tool calls

**Files:**
- Modify: `internal/transcript/parser.go` (add `case "assistant":` + helper)
- Test: `internal/transcript/parser_test.go` (append tests)

**Interfaces:**
- Consumes: Task 3's `rawMessage`, `rawBlock`, `truncate`.
- Produces: `(*Parser).assistantPayloads(rl rawLine) []Payload` (unexported). Event order within one assistant line: `assistant_message` first (seq after any `session_started`), then one `tool_call` per `tool_use` block in block order.

- [ ] **Step 1: Write the failing tests**

Append to `internal/transcript/parser_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/transcript/`
Expected: the two new tests FAIL (assistant lines fall into `default:`).

- [ ] **Step 3: Write the implementation**

In `parser.go`, add to the type switch in `Line`:

```go
	case "assistant":
		payloads = append(payloads, p.assistantPayloads(rl)...)
```

and append:

```go
func (p *Parser) assistantPayloads(rl rawLine) []Payload {
	var msg rawMessage
	if err := json.Unmarshal(rl.Message, &msg); err != nil {
		p.Skipped["assistant:badmessage"]++
		return nil
	}
	var blocks []rawBlock
	// Assistant content should always be an array; tolerate anything else
	// by emitting the message event with empty text.
	_ = json.Unmarshal(msg.Content, &blocks)

	var text bytes.Buffer
	var calls []Payload
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(b.Text)
		case "tool_use":
			calls = append(calls, ToolCallPayload{Name: b.Name, Input: truncate(string(b.Input))})
		case "thinking":
			// dropped by design: never shipped at any level
		default:
			p.Skipped["assistant:block:"+b.Type]++
		}
	}
	am := AssistantMessagePayload{
		Model:               msg.Model,
		InputTokens:         msg.Usage.InputTokens,
		OutputTokens:        msg.Usage.OutputTokens,
		CacheReadTokens:     msg.Usage.CacheReadInputTokens,
		CacheCreationTokens: msg.Usage.CacheCreationInputTokens,
		StopReason:          msg.StopReason,
		Text:                truncate(text.String()),
	}
	return append([]Payload{am}, calls...)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/transcript/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/transcript/
git commit -m "feat(transcript): derive assistant_message and tool_call from assistant lines"
```

---

### Task 5: `system` turn_duration → turn_completed

**Files:**
- Modify: `internal/transcript/parser.go` (add `case "system":`)
- Test: `internal/transcript/parser_test.go` (append test)

**Interfaces:**
- Consumes: Task 2's parser skeleton.
- Produces: `turn_completed` events with `TurnCompletedPayload{DurationMs, Messages}`.

- [ ] **Step 1: Write the failing test**

Append to `internal/transcript/parser_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/transcript/`
Expected: `TestSystemTurnDuration` FAILS.

- [ ] **Step 3: Write the implementation**

Add to the type switch in `Line`:

```go
	case "system":
		if rl.Subtype == "turn_duration" {
			payloads = append(payloads, TurnCompletedPayload{DurationMs: rl.DurationMs, Messages: rl.MessageCount})
		} else {
			p.Skipped["system:"+rl.Subtype]++
		}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/transcript/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/transcript/
git commit -m "feat(transcript): derive turn_completed from system turn_duration lines"
```

---

### Task 6: Redaction — the `metadata` level

**Files:**
- Create: `internal/redact/redact.go`
- Test: `internal/redact/redact_test.go`

**Interfaces:**
- Consumes: `transcript.Event`, `transcript.AllPayloads()`, payload structs.
- Produces: `redact.Level` (string) with consts `Metadata Level = "metadata"`, `Full Level = "full"`; `redact.Valid(l Level) bool`; `redact.Apply(level Level, ev transcript.Event) transcript.Event` (pure — returns a modified copy).

- [ ] **Step 1: Write the failing tests**

`internal/redact/redact_test.go`:

```go
package redact

import (
	"reflect"
	"testing"

	"github.com/incantery/agentmon/internal/transcript"
)

func TestFullIsIdentity(t *testing.T) {
	ev := transcript.Event{Type: transcript.UserPrompt, Payload: transcript.UserPromptPayload{Chars: 5, Text: "hello"}}
	if got := Apply(Full, ev); !reflect.DeepEqual(got, ev) {
		t.Errorf("Full must not modify events: %+v", got)
	}
}

func TestValid(t *testing.T) {
	if !Valid(Metadata) || !Valid(Full) || Valid(Level("loud")) {
		t.Error("Valid is wrong")
	}
}

// allowedStrings are payload string fields that carry no prompt/file/tool
// content and may survive metadata level. Everything else must be cleared.
var allowedStrings = map[string]bool{
	"Title": true, "Model": true, "StopReason": true,
	"Mode": true, "CWD": true, "Name": true,
}

// TestMetadataClearsAllContentFields walks every payload type registered in
// transcript.AllPayloads, fills every string field, applies Metadata, and
// asserts no non-allowlisted string survives. Adding a payload type without
// updating Apply (or the allowlist) fails this test.
func TestMetadataClearsAllContentFields(t *testing.T) {
	for _, proto := range transcript.AllPayloads() {
		v := reflect.New(reflect.TypeOf(proto)).Elem()
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).Kind() == reflect.String {
				v.Field(i).SetString("SECRET")
			}
		}
		ev := transcript.Event{Type: proto.EventType(), Payload: v.Interface().(transcript.Payload)}
		got := Apply(Metadata, ev)
		gv := reflect.ValueOf(got.Payload)
		gt := gv.Type()
		for i := 0; i < gv.NumField(); i++ {
			if gv.Field(i).Kind() != reflect.String {
				continue
			}
			name := gt.Field(i).Name
			if allowedStrings[name] {
				continue
			}
			if gv.Field(i).String() != "" {
				t.Errorf("%s.%s survived metadata redaction: %q", gt.Name(), name, gv.Field(i).String())
			}
		}
	}
}

func TestMetadataKeepsCounts(t *testing.T) {
	ev := transcript.Event{Type: transcript.UserPrompt, Payload: transcript.UserPromptPayload{Chars: 42, Text: "secret"}}
	got := Apply(Metadata, ev).Payload.(transcript.UserPromptPayload)
	if got.Chars != 42 || got.Text != "" {
		t.Errorf("got %+v", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/redact/`
Expected: FAIL — `undefined: Apply` (package doesn't exist yet).

- [ ] **Step 3: Write the implementation**

`internal/redact/redact.go`:

```go
// Package redact enforces the per-machine content level from the design
// spec: at Metadata level, no prompt/file/tool content leaves the machine.
package redact

import "github.com/incantery/agentmon/internal/transcript"

type Level string

const (
	Metadata Level = "metadata"
	Full     Level = "full"
)

func Valid(l Level) bool { return l == Metadata || l == Full }

// Apply returns ev with content fields cleared when level is Metadata.
// It never mutates its input.
func Apply(level Level, ev transcript.Event) transcript.Event {
	if level == Full {
		return ev
	}
	switch pl := ev.Payload.(type) {
	case transcript.UserPromptPayload:
		pl.Text = ""
		ev.Payload = pl
	case transcript.AssistantMessagePayload:
		pl.Text = ""
		ev.Payload = pl
	case transcript.ToolCallPayload:
		pl.Input = ""
		ev.Payload = pl
	case transcript.ToolResultPayload:
		pl.Content = ""
		ev.Payload = pl
	}
	return ev
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./...`
Expected: PASS (both packages).

- [ ] **Step 5: Commit**

```bash
git add internal/redact/
git commit -m "feat(redact): metadata/full levels with reflection-guarded content clearing"
```

---

### Task 7: `agentmon parse` CLI

**Files:**
- Create: `cmd/agentmon/main.go`, `cmd/agentmon/parse.go`
- Create: `cmd/agentmon/testdata/session.jsonl`
- Test: `cmd/agentmon/parse_test.go`

**Interfaces:**
- Consumes: `transcript.NewParser`, `Parser.Line`, `Parser.Skipped`; `redact.Apply`, `redact.Valid`, `redact.Level`.
- Produces: `agentmon parse [--level metadata|full] <file.jsonl>` printing one event JSON per line to stdout, skip summary to stderr; `runParse(stdout, stderr io.Writer, path string, level redact.Level) error` (unit-tested seam).

- [ ] **Step 1: Create the fixture**

`cmd/agentmon/testdata/session.jsonl` (9 lines; covers every handled type, an unknown type, and a malformed line):

```
{"type":"permission-mode","permissionMode":"auto","sessionId":"fix-1"}
{"type":"ai-title","aiTitle":"Fix the login bug","sessionId":"fix-1"}
{"type":"user","message":{"role":"user","content":"Fix the login bug please"},"timestamp":"2026-07-06T10:00:00.000Z","cwd":"/home/u/proj","sessionId":"fix-1","origin":{"kind":"human"}}
{"type":"assistant","message":{"model":"claude-fable-5","role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Looking now."},{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/home/u/proj/login.go"}}],"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":25,"cache_read_input_tokens":50,"cache_creation_input_tokens":10}},"timestamp":"2026-07-06T10:00:05.000Z","cwd":"/home/u/proj","sessionId":"fix-1"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"package main"}]},"timestamp":"2026-07-06T10:00:06.000Z","cwd":"/home/u/proj","sessionId":"fix-1"}
{"type":"assistant","message":{"model":"claude-fable-5","role":"assistant","content":[{"type":"text","text":"Fixed."}],"stop_reason":"end_turn","usage":{"input_tokens":200,"output_tokens":10}},"timestamp":"2026-07-06T10:00:10.000Z","cwd":"/home/u/proj","sessionId":"fix-1"}
{"type":"system","subtype":"turn_duration","durationMs":10500,"messageCount":4,"timestamp":"2026-07-06T10:00:10.500Z","sessionId":"fix-1"}
{"type":"file-history-snapshot","messageId":"m1"}
{not json
```

- [ ] **Step 2: Write the failing test**

`cmd/agentmon/parse_test.go`:

```go
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
```

Note the title assertion uses `Fix the login bug"` (closing quote) so it can't accidentally match the redacted prompt `Fix the login bug please`.

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./cmd/agentmon/`
Expected: FAIL — `undefined: runParse`.

- [ ] **Step 4: Write the implementation**

`cmd/agentmon/parse.go`:

```go
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/incantery/agentmon/internal/redact"
	"github.com/incantery/agentmon/internal/transcript"
)

// runParse streams one transcript file through the parser and prints each
// derived event as a JSON line. The session ID is the filename stem, as in
// ~/.claude/projects/<project>/<session-uuid>.jsonl.
func runParse(stdout, stderr io.Writer, path string, level redact.Level) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	p := transcript.NewParser(sessionID)
	enc := json.NewEncoder(stdout)

	// ReadBytes (not Scanner): transcript lines routinely exceed Scanner's
	// default token limit.
	r := bufio.NewReaderSize(f, 1<<20)
	var offset int64
	for {
		line, err := r.ReadBytes('\n')
		for _, ev := range p.Line(offset, line) {
			if encErr := enc.Encode(redact.Apply(level, ev)); encErr != nil {
				return encErr
			}
		}
		offset += int64(len(line))
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if len(p.Skipped) > 0 {
		fmt.Fprintf(stderr, "skipped: %v\n", p.Skipped)
	}
	return nil
}
```

`cmd/agentmon/main.go`:

```go
// agentmon: telemetry for AI coding-agent sessions.
// Milestone 1 ships only the parse debug command; watch/serve/sessions
// arrive in later milestones (see docs/superpowers/specs/).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/incantery/agentmon/internal/redact"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage: agentmon parse [--level metadata|full] <session.jsonl>`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "parse":
		fs := flag.NewFlagSet("parse", flag.ExitOnError)
		level := fs.String("level", string(redact.Full), `content level: "metadata" or "full"`)
		fs.Parse(os.Args[2:])
		if fs.NArg() != 1 || !redact.Valid(redact.Level(*level)) {
			usage()
		}
		if err := runParse(os.Stdout, os.Stderr, fs.Arg(0), redact.Level(*level)); err != nil {
			fmt.Fprintln(os.Stderr, "agentmon:", err)
			os.Exit(1)
		}
	default:
		usage()
	}
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go build ./... && go test ./...`
Expected: PASS across all three packages.

- [ ] **Step 6: Verify against a real transcript**

```bash
make build
F=$(/bin/ls -t ~/.claude/projects/*/*.jsonl | head -1)
bin/agentmon parse --level metadata "$F" | head -20
bin/agentmon parse --level metadata "$F" | python3 -c "import sys,json,collections; c=collections.Counter(json.loads(l)['type'] for l in sys.stdin); print(dict(c))"
```

Expected: JSON event lines with a plausible type distribution (assistant_message + tool_call dominating), a skip summary on stderr listing types like `mode`, `last-prompt`, `attachment`, `file-history-snapshot`, `queue-operation` — and **no exceptions/panics**. Spot-check that no prompt text appears at metadata level: `bin/agentmon parse --level metadata "$F" | grep -c '"text"'` should print `0`.

- [ ] **Step 7: Commit**

```bash
git add cmd/agentmon/
git commit -m "feat(cli): agentmon parse — stream a transcript as derived events"
```

---

## Done when

- `go test ./...` green; `make build` produces `bin/agentmon`.
- `bin/agentmon parse --level metadata <any real transcript>` streams events without error and leaks no content.
- Milestone 2 (watch + spool) can consume `transcript.NewParser`/`Line` unchanged.
