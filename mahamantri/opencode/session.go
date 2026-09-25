package opencode

import "sync"

// sessionTracker records sessionID -> parentID as session.created/updated
// events go by, so later events can be tagged as subagent-originated.
type sessionTracker struct {
	mu       sync.Mutex
	parentOf map[string]string
}

func newSessionTracker() *sessionTracker {
	return &sessionTracker{parentOf: make(map[string]string)}
}

func (t *sessionTracker) observe(sessionID, parentID string) {
	if sessionID == "" {
		return
	}
	t.mu.Lock()
	t.parentOf[sessionID] = parentID
	t.mu.Unlock()
}

func (t *sessionTracker) isSubagent(sessionID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.parentOf[sessionID] != ""
}

// parent returns the parentID observed for sessionID and whether anything
// has been observed for it at all (as opposed to a top-level session with
// an empty parentID).
func (t *sessionTracker) parent(sessionID string) (parentID string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	parentID, ok = t.parentOf[sessionID]
	return
}
