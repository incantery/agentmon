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
