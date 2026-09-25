package attention

import (
	"encoding/json"
	"fmt"

	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

// SourceMahamantri and SourceSenapati are the metadata.source values
// mahamantri and Senapati (by documented convention, see
// senapati/senapati.md) stamp on every message they send into a session.
// The real opencode TUI never sets metadata at all (confirmed by reading
// its shipped call site), so a user message tagged with neither value was
// most likely typed by a human directly in the local TUI/CLI.
const (
	SourceMahamantri = "mahamantri"
	SourceSenapati   = "senapati"
)

// DetectManualTakeover reports what a person did to a session, if ev shows
// one did, as a short phrase ("sent it a message: ...", "interrupted it").
// Two live-verified signals:
//   - session.inbox.enqueued fires once per message admitted into a session
//     and carries the message's text and metadata itself, so no lookup is
//     needed; a user message with no recognized metadata.source was not
//     sent by mahamantri or Senapati.
//   - session.execution.interrupted with reason "user".
//
// Not proof of human origin - the best signal the API exposes.
func DetectManualTakeover(ev opencode.Event) (action string, ok bool) {
	switch ev.Type {
	case "session.inbox.enqueued":
		var data struct {
			Item struct {
				Type    string `json:"type"`
				Payload struct {
					Text     string            `json:"text"`
					Metadata map[string]string `json:"metadata"`
				} `json:"payload"`
			} `json:"item"`
		}
		if err := json.Unmarshal(ev.Raw, &data); err != nil || data.Item.Type != "user" {
			return "", false
		}
		source := data.Item.Payload.Metadata["source"]
		if source == SourceMahamantri || source == SourceSenapati {
			return "", false
		}
		return fmt.Sprintf("sent it a message: %q", truncate(data.Item.Payload.Text, 200)), true

	case "session.execution.interrupted":
		var data struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(ev.Raw, &data); err != nil || data.Reason != "user" {
			return "", false
		}
		return "interrupted it", true
	}
	return "", false
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
