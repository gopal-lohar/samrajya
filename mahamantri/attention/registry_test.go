package attention

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

type fakeParents struct {
	parentOf map[string]string
}

func (f *fakeParents) SessionParent(sessionID string) (string, bool) {
	p, ok := f.parentOf[sessionID]
	return p, ok
}

func newTestRegistry(t *testing.T, parents *fakeParents) *Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.json")
	return NewRegistry(path, parents)
}

func TestRegistryOwnerDirectAndChild(t *testing.T) {
	parents := &fakeParents{parentOf: map[string]string{"ses_child": "ses_top"}}
	r := newTestRegistry(t, parents)

	if _, err := r.Register("ses_top", ""); err != nil {
		t.Fatal(err)
	}

	if owner, ok := r.Owner("ses_top"); !ok || owner != "ses_top" {
		t.Errorf("Owner(top) = %q, %v", owner, ok)
	}
	if owner, ok := r.Owner("ses_child"); !ok || owner != "ses_top" {
		t.Errorf("Owner(child) = %q, %v, want ses_top, true", owner, ok)
	}
	if _, ok := r.Owner("ses_unrelated"); ok {
		t.Error("Owner(unrelated) should be false")
	}
}

func TestRegistryUnregister(t *testing.T) {
	r := newTestRegistry(t, &fakeParents{parentOf: map[string]string{}})
	if _, err := r.Register("ses_a", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.Unregister("ses_a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Owner("ses_a"); ok {
		t.Error("expected ses_a to no longer be owned after Unregister")
	}
	if err := r.Unregister("ses_a"); err != ErrNotRegistered {
		t.Errorf("Unregister on missing instance = %v, want ErrNotRegistered", err)
	}
}

func TestRegistryDuplicateRegister(t *testing.T) {
	r := newTestRegistry(t, &fakeParents{parentOf: map[string]string{}})
	if _, err := r.Register("ses_a", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Register("ses_a", ""); err != ErrAlreadyRegistered {
		t.Errorf("second Register = %v, want ErrAlreadyRegistered", err)
	}
}

func TestRegistryRejectsProtectedSession(t *testing.T) {
	r := newTestRegistry(t, &fakeParents{parentOf: map[string]string{}})
	r.Protect("ses_senapati")

	if _, err := r.Register("ses_senapati", ""); err != ErrProtectedSession {
		t.Errorf("Register(protected) = %v, want ErrProtectedSession", err)
	}
	if _, err := r.Register("ses_other", ""); err != nil {
		t.Errorf("Register(unprotected) = %v, want nil", err)
	}
}

func TestRegistryNeedsAttentionScopedToRegisteredInstances(t *testing.T) {
	r := newTestRegistry(t, &fakeParents{parentOf: map[string]string{}})
	if _, err := r.Register("ses_registered", ""); err != nil {
		t.Fatal(err)
	}

	blocking := opencode.Event{SessionID: "ses_registered", Type: "permission.asked", Severity: opencode.Blocking}
	if !r.NeedsAttention(blocking) {
		t.Error("blocking event from a registered session should need attention")
	}

	sameEventUnregistered := opencode.Event{SessionID: "ses_unregistered", Type: "permission.asked", Severity: opencode.Blocking}
	if r.NeedsAttention(sameEventUnregistered) {
		t.Error("blocking event from an unregistered session must not need attention")
	}

	notRequired := opencode.Event{SessionID: "ses_registered", Type: "session.step.started", Severity: opencode.Info}
	if r.NeedsAttention(notRequired) {
		t.Error("a non-required event from a registered session still shouldn't need attention")
	}
}

func TestRegistryObserveStatusTransitions(t *testing.T) {
	r := newTestRegistry(t, &fakeParents{parentOf: map[string]string{}})
	if _, err := r.Register("ses_a", ""); err != nil {
		t.Fatal(err)
	}

	r.Observe(opencode.Event{SessionID: "ses_a", Type: "permission.asked", Severity: opencode.Blocking})
	if inst, _ := r.Get("ses_a"); inst.Status != "blocked" || inst.Reason != "permission.asked" {
		t.Errorf("after Blocking event: status=%q reason=%q", inst.Status, inst.Reason)
	}

	r.Observe(opencode.Event{SessionID: "ses_a", Type: "session.idle"})
	if inst, _ := r.Get("ses_a"); inst.Status != "idle" {
		t.Errorf("after session.idle: status=%q, want idle", inst.Status)
	}

	r.Observe(opencode.Event{SessionID: "ses_a", Type: "session.text.delta", Severity: opencode.Info})
	if inst, _ := r.Get("ses_a"); inst.Status != "running" {
		t.Errorf("after fresh activity: status=%q, want running", inst.Status)
	}
}

func TestRegistryMarkManual(t *testing.T) {
	r := newTestRegistry(t, &fakeParents{parentOf: map[string]string{}})
	if _, err := r.Register("ses_a", ""); err != nil {
		t.Fatal(err)
	}
	r.Observe(opencode.Event{SessionID: "ses_a", Type: "session.text.delta", Severity: opencode.Info})

	r.MarkManual("ses_a", "session.updated")
	inst, ok := r.Get("ses_a")
	if !ok || inst.Status != "manual" || inst.Reason != "session.updated" {
		t.Errorf("after MarkManual: %+v ok=%v, want status=manual reason=session.updated", inst, ok)
	}

	// MarkManual on a session that isn't registered must be a no-op, not a panic.
	r.MarkManual("ses_missing", "whatever")
}

// Regression: "manual" used to be overwritten by the very next activity
// event, so it was visible for milliseconds. It must last until the turn ends.
func TestRegistryManualSurvivesActivityUntilTurnEnds(t *testing.T) {
	r := newTestRegistry(t, &fakeParents{parentOf: map[string]string{}})
	if _, err := r.Register("ses_a", ""); err != nil {
		t.Fatal(err)
	}
	r.MarkManual("ses_a", "sent it a message")

	r.Observe(opencode.Event{SessionID: "ses_a", Type: "session.step.started"})
	r.Observe(opencode.Event{SessionID: "ses_a", Type: "session.text.delta"})
	if inst, _ := r.Get("ses_a"); inst.Status != "manual" {
		t.Errorf("after activity: status=%q, want manual", inst.Status)
	}

	r.Observe(opencode.Event{SessionID: "ses_a", Type: "session.execution.succeeded"})
	if inst, _ := r.Get("ses_a"); inst.Status != "idle" {
		t.Errorf("after the turn ended: status=%q, want idle", inst.Status)
	}
}

func TestRegistryReloadFromDiskAddsAndRemovesWithoutResettingStatus(t *testing.T) {
	r := newTestRegistry(t, &fakeParents{parentOf: map[string]string{}})
	if _, err := r.Register("ses_a", "keep-me"); err != nil {
		t.Fatal(err)
	}
	r.Observe(opencode.Event{SessionID: "ses_a", Type: "permission.asked", Severity: opencode.Blocking})

	// Hand-edit the file: keep ses_a, add ses_b, as an external harness would.
	data := `{"instances":[{"sessionID":"ses_a","label":"keep-me"},{"sessionID":"ses_b","label":""}]}`
	if err := os.WriteFile(r.path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}

	if inst, ok := r.Get("ses_a"); !ok || inst.Status != "blocked" {
		t.Errorf("ses_a should keep its runtime status across reload, got %+v ok=%v", inst, ok)
	}
	if _, ok := r.Get("ses_b"); !ok {
		t.Error("ses_b should have been added by reload")
	}

	// Now drop ses_b from disk and reload again - it should disappear.
	data = `{"instances":[{"sessionID":"ses_a","label":"keep-me"}]}`
	if err := os.WriteFile(r.path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("ses_b"); ok {
		t.Error("ses_b should have been removed by reload")
	}
}
