package linear

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

// Threads remembers the comment threads Senapati has written in, so a
// person's reply in one is recognised as addressed to it. Linear's comment
// webhook says which thread a reply is in (parentId) but not who wrote that
// thread, so mahamantri learns it from Senapati's own comments as their
// webhooks arrive. Persisted, so it survives a restart; a thread Senapati
// wrote in before this existed is unknown until it comments there again.
type Threads struct {
	mu    sync.Mutex
	path  string
	known map[string]threadEntry
}

type threadEntry struct {
	Issue string    `json:"issue,omitempty"`
	Seen  time.Time `json:"seen"`
}

// LoadThreads reads path; a missing file is an empty index.
func LoadThreads(path string) (*Threads, error) {
	t := &Threads{path: path, known: map[string]threadEntry{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &t.known); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Threads) Has(threadID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.known[threadID]
	return ok
}

// Add records that Senapati wrote in threadID and saves the index.
func (t *Threads) Add(threadID, issue string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.known[threadID] = threadEntry{Issue: issue, Seen: time.Now().UTC()}
	data, err := json.MarshalIndent(t.known, "", "  ")
	if err != nil {
		return err
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, t.path)
}
