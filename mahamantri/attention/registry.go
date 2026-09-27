package attention

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

var (
	ErrAlreadyRegistered = errors.New("instance already registered")
	ErrNotRegistered     = errors.New("instance not registered")
	ErrEmptySessionID    = errors.New("sessionID is required")
	ErrProtectedSession  = errors.New("sessionID is reserved (senapati's own session)")
)

// parentLookup is the one thing Registry needs from *opencode.Client, kept
// as an interface so it can be exercised with a fake in tests.
type parentLookup interface {
	SessionParent(sessionID string) (parentID string, ok bool)
}

// Instance is a manager-registered top-level opencode session to watch.
// SessionID/Label/RegisteredAt are persisted to disk; Status/Reason/
// LastEvent/LastEventAt are runtime-only, rebuilt from the live stream, and
// never written to disk so they can't go stale across a restart.
type Instance struct {
	SessionID    string    `json:"sessionID"`
	Label        string    `json:"label,omitempty"`
	RegisteredAt time.Time `json:"registeredAt"`
	// Phase is a free-form note the manager keeps on the session ("planning",
	// "awaiting plan approval", "executing"...). Persisted, shown in the TUI
	// and in the handoff to a rotated Senapati, so state that would otherwise
	// live only in its context survives.
	Phase string `json:"phase,omitempty"`

	Status      string          `json:"status"` // "running" | "idle" | "blocked" | "manual"
	Reason      string          `json:"reason,omitempty"`
	LastEvent   *opencode.Event `json:"lastEvent,omitempty"`
	LastEventAt time.Time       `json:"lastEventAt,omitzero"`
}

type Registry struct {
	mu        sync.Mutex
	instances map[string]*Instance
	path      string
	parents   parentLookup
	protected string
}

// NewRegistry does not read path itself - call ReloadFromDisk once after
// construction (and then periodically via PollFile) to load it.
func NewRegistry(path string, parents parentLookup) *Registry {
	return &Registry{
		instances: make(map[string]*Instance),
		path:      path,
		parents:   parents,
	}
}

// Protect designates a sessionID that Register must always reject - used to
// stop Senapati from registering itself and creating a feedback loop where
// it receives prompts about its own activity.
func (r *Registry) Protect(sessionID string) {
	r.mu.Lock()
	r.protected = sessionID
	r.mu.Unlock()
}

func (r *Registry) Register(sessionID, label string) (Instance, error) {
	if sessionID == "" {
		return Instance{}, ErrEmptySessionID
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if sessionID == r.protected {
		return Instance{}, ErrProtectedSession
	}
	if _, exists := r.instances[sessionID]; exists {
		return Instance{}, ErrAlreadyRegistered
	}
	inst := &Instance{
		SessionID:    sessionID,
		Label:        label,
		RegisteredAt: time.Now().UTC(),
		Status:       "running",
	}
	r.instances[sessionID] = inst
	if err := r.saveLocked(); err != nil {
		delete(r.instances, sessionID)
		return Instance{}, err
	}
	return *inst, nil
}

// Update changes an instance's label and/or phase; nil leaves a field alone.
func (r *Registry) Update(sessionID string, label, phase *string) (Instance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, ok := r.instances[sessionID]
	if !ok {
		return Instance{}, ErrNotRegistered
	}
	if label != nil {
		inst.Label = *label
	}
	if phase != nil {
		inst.Phase = *phase
	}
	if err := r.saveLocked(); err != nil {
		return Instance{}, err
	}
	return *inst, nil
}

func (r *Registry) Unregister(sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.instances[sessionID]; !exists {
		return ErrNotRegistered
	}
	delete(r.instances, sessionID)
	return r.saveLocked()
}

func (r *Registry) List() []Instance {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Instance, 0, len(r.instances))
	for _, inst := range r.instances {
		out = append(out, *inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RegisteredAt.Before(out[j].RegisteredAt) })
	return out
}

// Get returns a single instance by sessionID, avoiding an O(n) List() scan
// in the attention-event forwarding loop.
func (r *Registry) Get(sessionID string) (Instance, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, ok := r.instances[sessionID]
	if !ok {
		return Instance{}, false
	}
	return *inst, true
}

func (r *Registry) isRegistered(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.instances[sessionID]
	return ok
}

// Owner walks sessionID's ancestry (via parentLookup) and returns the
// registered top-level instance it belongs to, if any. Bounded to guard
// against a malformed/cyclic parent chain.
func (r *Registry) Owner(sessionID string) (instanceID string, ok bool) {
	id := sessionID
	for i := 0; i < 64; i++ {
		if id == "" {
			return "", false
		}
		if r.isRegistered(id) {
			return id, true
		}
		parent, seen := r.parents.SessionParent(id)
		if !seen {
			return "", false
		}
		id = parent
	}
	return "", false
}

// NeedsAttention reports whether ev is both attention-required and belongs
// to a registered instance. This is the one filter both the HTTP API and
// the TUI apply - defined once here so they can't drift apart.
func (r *Registry) NeedsAttention(ev opencode.Event) bool {
	if !Required(ev) {
		return false
	}
	_, owned := r.Owner(ev.SessionID)
	return owned
}

// Observe must see every event (not just attention-required ones) for a
// registered instance's status to correctly flip back to "running" after
// fresh activity following an "idle"/"blocked" state.
func (r *Registry) Observe(ev opencode.Event) {
	if ev.SessionID == "" {
		return
	}
	owner, ok := r.Owner(ev.SessionID)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, ok := r.instances[owner]
	if !ok {
		return
	}
	evCopy := ev
	inst.LastEvent = &evCopy
	inst.LastEventAt = time.Now().UTC()
	switch {
	case ev.Severity == opencode.Blocking, ev.Type == "session.execution.failed":
		inst.Status = "blocked"
		inst.Reason = ev.Type
	case ev.Type == "session.idle", ev.Type == "session.execution.succeeded", ev.Type == "session.execution.interrupted":
		inst.Status = "idle"
		inst.Reason = ""
	default:
		// A person-driven turn stays "manual" until it ends (the idle case
		// above) - ordinary activity events must not erase that.
		if inst.Status != "manual" {
			inst.Status = "running"
			inst.Reason = ""
		}
	}
}

// MarkManual overrides an instance's status to "manual" - called from the
// attention-forwarding loop once DetectManualTakeover confirms a human, not
// mahamantri or Senapati's own tooling, produced the latest activity on it.
// Kept separate from Observe so the override lands after that event's
// baseline status update, in a fixed order.
func (r *Registry) MarkManual(sessionID, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, ok := r.instances[sessionID]
	if !ok {
		return
	}
	inst.Status = "manual"
	inst.Reason = reason
	inst.LastEventAt = time.Now().UTC()
}

type registryFile struct {
	Instances []Instance `json:"instances"`
}

// ReloadFromDisk re-reads path and reconciles additions/removals into
// memory. Entries present both on disk and in memory keep their runtime
// Status/Reason/LastEvent untouched - only the registered set changes.
// A missing file is treated as an empty registry, not an error.
func (r *Registry) ReloadFromDisk() error {
	data, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f registryFile
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	onDisk := make(map[string]Instance, len(f.Instances))
	for _, inst := range f.Instances {
		onDisk[inst.SessionID] = inst
	}
	for id := range r.instances {
		if _, ok := onDisk[id]; !ok {
			delete(r.instances, id)
		}
	}
	for id, inst := range onDisk {
		if existing, ok := r.instances[id]; ok {
			existing.Label, existing.Phase = inst.Label, inst.Phase
			continue
		}
		r.instances[id] = &Instance{
			SessionID:    inst.SessionID,
			Label:        inst.Label,
			Phase:        inst.Phase,
			RegisteredAt: inst.RegisteredAt,
			Status:       "running",
		}
	}
	return nil
}

// PollFile reloads path from disk every interval until ctx is cancelled, so
// a hand-edited registry file is picked up without any HTTP call.
func (r *Registry) PollFile(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.ReloadFromDisk()
		case <-ctx.Done():
			return
		}
	}
}

// saveLocked writes the persisted fields of the current instance set to
// path. Callers must already hold r.mu. Writes via a temp file + rename for
// atomicity, so a crash mid-write can't leave a truncated registry file.
func (r *Registry) saveLocked() error {
	f := registryFile{Instances: make([]Instance, 0, len(r.instances))}
	for _, inst := range r.instances {
		f.Instances = append(f.Instances, Instance{
			SessionID:    inst.SessionID,
			Label:        inst.Label,
			Phase:        inst.Phase,
			RegisteredAt: inst.RegisteredAt,
		})
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}
