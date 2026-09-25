package state

import (
	"encoding/json"
	"errors"
	"os"
	"time"
)

// SenapatiRecord is one Senapati opencode session, current or retired by
// context-usage rotation.
type SenapatiRecord struct {
	SessionID string     `json:"sessionID"`
	Title     string     `json:"title"`
	CreatedAt time.Time  `json:"createdAt"`
	RotatedAt *time.Time `json:"rotatedAt,omitempty"`
	// BriefedAt is when Senapati was told who it is and what to do; a
	// session without it (created before briefings existed) gets one on the
	// next start.
	BriefedAt *time.Time `json:"briefedAt,omitempty"`
}

// SenapatiState is mahamantri's persisted view of Senapati's session
// history - there will be more than one Senapati session over the
// program's lifetime as each one's context fills up.
type SenapatiState struct {
	Current SenapatiRecord   `json:"current"`
	History []SenapatiRecord `json:"history,omitempty"`
	// Seq is the number of the most recently created Senapati-<n> session.
	// Counting history alone would reuse a number whenever a saved session
	// turned out to be gone from the server and was replaced.
	Seq int `json:"seq,omitempty"`
}

// Load reads path. ok is false with a nil error if the file doesn't exist
// yet (first run).
func Load(path string) (SenapatiState, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return SenapatiState{}, false, nil
	}
	if err != nil {
		return SenapatiState{}, false, err
	}
	var s SenapatiState
	if err := json.Unmarshal(data, &s); err != nil {
		return SenapatiState{}, false, err
	}
	return s, true, nil
}

// Save writes s to path via a temp file + rename, the same atomic pattern
// attention.Registry uses for its own JSON file.
func Save(path string, s SenapatiState) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
