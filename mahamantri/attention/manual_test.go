package attention

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

func enqueued(payload string) opencode.Event {
	return opencode.Event{
		Type: "session.inbox.enqueued",
		Raw:  json.RawMessage(`{"inboxID":"msg_1","sessionID":"ses_a","item":{"type":"user","payload":` + payload + `,"delivery":"steer"}}`),
	}
}

// Shapes below are real payloads captured from a live server.
func TestDetectManualTakeoverUntaggedMessage(t *testing.T) {
	action, ok := DetectManualTakeover(enqueued(`{"text":"stop, I am taking over"}`))
	if !ok || !strings.Contains(action, "stop, I am taking over") {
		t.Errorf("untagged message: action=%q ok=%v, want a takeover quoting the text", action, ok)
	}
}

func TestDetectManualTakeoverIgnoresOwnTaggedMessages(t *testing.T) {
	for _, src := range []string{SourceMahamantri, SourceSenapati} {
		if action, ok := DetectManualTakeover(enqueued(`{"text":"hi","metadata":{"source":"` + src + `"}}`)); ok {
			t.Errorf("source=%s flagged as manual (%q)", src, action)
		}
	}
}

func TestDetectManualTakeoverOtherSourceTagStillManual(t *testing.T) {
	if _, ok := DetectManualTakeover(enqueued(`{"text":"hi","metadata":{"source":"something-else"}}`)); !ok {
		t.Error("an unrecognized source tag should still count as manual")
	}
}

func TestDetectManualTakeoverInterruptReason(t *testing.T) {
	user := opencode.Event{Type: "session.execution.interrupted", Raw: json.RawMessage(`{"sessionID":"ses_a","reason":"user"}`)}
	if action, ok := DetectManualTakeover(user); !ok || action != "interrupted it" {
		t.Errorf("reason=user: action=%q ok=%v", action, ok)
	}
	for _, reason := range []string{"shutdown", "superseded", "inactivity"} {
		ev := opencode.Event{Type: "session.execution.interrupted", Raw: json.RawMessage(`{"reason":"` + reason + `"}`)}
		if _, ok := DetectManualTakeover(ev); ok {
			t.Errorf("reason=%s flagged as manual", reason)
		}
	}
}

func TestDetectManualTakeoverIgnoresNonUserItemsAndOtherEvents(t *testing.T) {
	nonUser := opencode.Event{Type: "session.inbox.enqueued", Raw: json.RawMessage(`{"item":{"type":"synthetic","payload":{"text":"x"}}}`)}
	if _, ok := DetectManualTakeover(nonUser); ok {
		t.Error("a non-user inbox item was flagged as manual")
	}
	if _, ok := DetectManualTakeover(opencode.Event{Type: "session.text.delta", Raw: json.RawMessage(`{}`)}); ok {
		t.Error("an unrelated event type was flagged as manual")
	}
	if _, ok := DetectManualTakeover(opencode.Event{Type: "session.inbox.enqueued"}); ok {
		t.Error("an event with no payload was flagged as manual")
	}
}
