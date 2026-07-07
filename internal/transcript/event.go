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
	// Watcher-produced (milestone 2): not derived from transcript lines.
	SessionIdle  EventType = "session_idle"
	SessionEnded EventType = "session_ended"
	SpoolEvicted EventType = "spool_evicted"
)

// AllEventTypes lists every event type this package can produce.
var AllEventTypes = []EventType{
	SessionStarted, SessionTitle, UserPrompt, AssistantMessage,
	ToolCall, ToolResult, PermissionMode, TurnCompleted,
	SessionIdle, SessionEnded, SpoolEvicted,
}

// Event is the envelope defined in the design spec. Identity for
// server-side dedupe is (machine, session_id, agent_id, offset, seq);
// Machine is stamped by the shipper, not the parser.
type Event struct {
	Machine   string `json:"machine,omitempty"`
	Project   string `json:"project,omitempty"`
	SessionID string `json:"session_id"`
	// AgentID/AgentType identify subagent transcripts; empty (and omitted
	// from JSON) for the session's main transcript. Omitempty is load-
	// bearing: main-transcript events must stay byte-identical to those
	// shipped before these fields existed, or Loki dedupe stops absorbing
	// replays.
	AgentID   string    `json:"agent_id,omitempty"`
	AgentType string    `json:"agent_type,omitempty"`
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
	Cache5mTokens       int64  `json:"cache_5m_tokens,omitempty"`
	Cache1hTokens       int64  `json:"cache_1h_tokens,omitempty"`
	// CostUSD is the notional API cost at published rates, stamped at
	// derivation. nil = the model isn't in the pricing table (unpriced,
	// NOT free) — the field is omitted from JSON so dashboards can tell
	// the difference.
	CostUSD    *float64 `json:"cost_usd,omitempty"`
	StopReason string   `json:"stop_reason,omitempty"`
	Text       string   `json:"text,omitempty"` // cleared at metadata level
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

type SessionIdlePayload struct {
	IdleSeconds int64 `json:"idle_seconds"`
}

type SessionEndedPayload struct {
	Reason string `json:"reason"` // "inactive" | "removed"
}

type SpoolEvictedPayload struct {
	Dropped int `json:"dropped"`
}

func (SessionStartedPayload) EventType() EventType   { return SessionStarted }
func (SessionTitlePayload) EventType() EventType     { return SessionTitle }
func (UserPromptPayload) EventType() EventType       { return UserPrompt }
func (AssistantMessagePayload) EventType() EventType { return AssistantMessage }
func (ToolCallPayload) EventType() EventType         { return ToolCall }
func (ToolResultPayload) EventType() EventType       { return ToolResult }
func (PermissionModePayload) EventType() EventType   { return PermissionMode }
func (TurnCompletedPayload) EventType() EventType    { return TurnCompleted }
func (SessionIdlePayload) EventType() EventType      { return SessionIdle }
func (SessionEndedPayload) EventType() EventType     { return SessionEnded }
func (SpoolEvictedPayload) EventType() EventType     { return SpoolEvicted }

// AllPayloads returns a zero value of every payload type. The redact
// package's property test walks this list, so every new payload type
// MUST be added here.
func AllPayloads() []Payload {
	return []Payload{
		SessionStartedPayload{}, SessionTitlePayload{}, UserPromptPayload{},
		AssistantMessagePayload{}, ToolCallPayload{}, ToolResultPayload{},
		PermissionModePayload{}, TurnCompletedPayload{},
		SessionIdlePayload{}, SessionEndedPayload{}, SpoolEvictedPayload{},
	}
}
