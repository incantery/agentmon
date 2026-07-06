package transcript

import (
	"bytes"
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/incantery/agentmon/internal/pricing"
)

// MaxContentBytes caps every content field (prompt text, tool input,
// tool-result content) even at full level.
const MaxContentBytes = 2048

// Parser derives events from one session's transcript lines, fed in file
// order. It is not safe for concurrent use.
//
// A Parser must observe its file from offset 0: it emits session_started
// for the first line it sees and carries timestamp/project state across
// lines. Resuming mid-file with a fresh Parser changes event identity
// (a spurious session_started shifts that line's seq numbers) and loses
// carried state — a resuming consumer must replay from offset 0 or add a
// state snapshot API first.
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
	case "assistant":
		payloads = append(payloads, p.assistantPayloads(rl)...)
	case "system":
		if rl.Subtype == "turn_duration" {
			payloads = append(payloads, TurnCompletedPayload{DurationMs: rl.DurationMs, Messages: rl.MessageCount})
		} else {
			p.Skipped["system:"+rl.Subtype]++
		}
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

// truncate caps s at MaxContentBytes, trimming at most a split trailing
// rune (an invalid byte elsewhere in s must not shrink the whole prefix),
// and appending an ellipsis when cut.
func truncate(s string) string {
	if len(s) <= MaxContentBytes {
		return s
	}
	cut := s[:MaxContentBytes]
	for i := 0; i < utf8.UTFMax-1 && len(cut) > 0; i++ {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r != utf8.RuneError || size != 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

// rawMessage covers .message on user and assistant lines.
type rawMessage struct {
	Role       string `json:"role"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheCreation            struct {
			Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
			Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
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
	if len(msg.Content) == 0 || string(msg.Content) == "null" {
		p.Skipped["user:badcontent"]++
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
	am.Cache5mTokens = msg.Usage.CacheCreation.Ephemeral5m
	am.Cache1hTokens = msg.Usage.CacheCreation.Ephemeral1h
	u := pricing.Usage{
		InputTokens:        am.InputTokens,
		OutputTokens:       am.OutputTokens,
		CacheReadTokens:    am.CacheReadTokens,
		Cache5mWriteTokens: am.Cache5mTokens,
		Cache1hWriteTokens: am.Cache1hTokens,
	}
	if u.Cache5mWriteTokens+u.Cache1hWriteTokens == 0 {
		// Older lines carry only the total: bill it all at the cheaper
		// 5m rate (conservative-low, documented in the plan).
		u.Cache5mWriteTokens = am.CacheCreationTokens
	}
	if usd, priced := pricing.Cost(am.Model, u); priced {
		am.CostUSD = &usd
	}
	return append([]Payload{am}, calls...)
}
