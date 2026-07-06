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
