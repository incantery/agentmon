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
	case "user":
		payloads = append(payloads, p.userPayloads(rl)...)
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
