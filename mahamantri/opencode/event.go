package opencode

import "encoding/json"

type Severity int

const (
	Info Severity = iota
	Warning
	Blocking
)

func (s Severity) String() string {
	switch s {
	case Blocking:
		return "blocking"
	case Warning:
		return "warning"
	default:
		return "info"
	}
}

func (s Severity) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// Event is the entire contract with the (future) event manager: deliberately
// flat, no interfaces. Raw keeps the original "data" JSON so a caller can
// look past Summary without this program growing an extension mechanism.
type Event struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	SessionID string          `json:"sessionID"`
	Subagent  bool            `json:"subagent"`
	Severity  Severity        `json:"severity"`
	Summary   string          `json:"summary"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

// envelope is the real wire shape observed on GET /api/event for opencode
// v2.0.11: {"id","type","data",...}. "created"/"location"/"durable" also
// appear on session-scoped events but aren't needed for classification.
type envelope struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func decodeEnvelope(raw []byte) (envelope, error) {
	var env envelope
	err := json.Unmarshal(raw, &env)
	return env, err
}
