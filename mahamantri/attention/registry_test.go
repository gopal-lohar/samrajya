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
	step := func(ev opencode.Event, want string) {
		t.Helper()
		r.Observe(ev)
		if inst, _ := r.Get("ses_a"); inst.Status != want {
			t.Errorf("after %s: status=%q, want %q", ev.Type, inst.Status, want)
		}
	}
	step(opencode.Event{SessionID: "ses_a", Type: "session.execution.started"}, "running")
	step(opencode.Event{SessionID: "ses_a", Type: "permission.asked", Severity: opencode.Blocking, Summary: "permission request per_1"}, "blocked")
	step(opencode.Event{SessionID: "ses_a", Type: "permission.replied"}, "running")
	step(opencode.Event{SessionID: "ses_a", Type: "session.execution.succeeded"}, "idle")
	// Regression: any event used to mean "running" - opening the session in
	// the TUI (session.viewed) made an idle sainik look busy.
	step(opencode.Event{SessionID: "ses_a", Type: "session.viewed"}, "idle")
	step(opencode.Event{SessionID: "ses_a", Type: "session.usage.updated"}, "idle")
	step(opencode.Event{SessionID: "ses_a", Type: "session.execution.started"}, "running")
	step(opencode.Event{SessionID: "ses_a", Type: "session.execution.failed"}, "failed")
}

// Regression: a sainik's subagent finishing was reported as the sainik
// finishing, and flipped its status to idle mid-turn.
func TestSubagentCompletionIsNotTheSainiksCompletion(t *testing.T) {
	r := newTestRegistry(t, &fakeParents{parentOf: map[string]string{"ses_child": "ses_a"}})
	r.Register("ses_a", "")
	r.Observe(opencode.Event{SessionID: "ses_a", Type: "session.execution.started"})

	done := opencode.Event{SessionID: "ses_child", Type: "session.execution.succeeded"}
	r.Observe(done)
	if r.NeedsAttention(done) {
		t.Error("a subagent finishing must not be reported")
	}
	if inst, _ := r.Get("ses_a"); inst.Status != "running" {
		t.Errorf("status = %q, want running", inst.Status)
	}

	asked := opencode.Event{SessionID: "ses_child", Type: "permission.asked", Severity: opencode.Blocking}
	if !r.NeedsAttention(asked) {
		t.Error("a subagent waiting on a permission holds up the sainik and must be reported")
	}
	if own := (opencode.Event{SessionID: "ses_a", Type: "session.execution.succeeded"}); !r.NeedsAttention(own) {
		t.Error("the sainik's own completion must be reported")
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

func TestInstanceIssueAndForIssue(t *testing.T) {
	reg := newTestRegistry(t, &fakeParents{})
	reg.Register("ses_a", "sainik-SEN-31-request-info")
	reg.Register("ses_b", "sainik-sen-32")
	reg.Register("ses_c", "something else")
	for label, want := range map[string]string{
		"sainik-SEN-31-request-info": "SEN-31",
		"sainik-sen-32":              "SEN-32",
		"sainik-SEN-310-x":           "SEN-310",
		"something else":             "",
		"sainik-task":                "",
	} {
		if got := (Instance{Label: label}).Issue(); got != want {
			t.Errorf("Issue(%q) = %q, want %q", label, got, want)
		}
	}
	if got := reg.ForIssue("sen-31"); len(got) != 1 || got[0].SessionID != "ses_a" {
		t.Errorf("ForIssue(sen-31) = %+v", got)
	}
	if got := reg.ForIssue("SEN-3"); len(got) != 0 {
		t.Errorf("ForIssue(SEN-3) must not match SEN-31: %+v", got)
	}
}

func TestExpectedInterruptIsConsumedOnce(t *testing.T) {
	reg := newTestRegistry(t, &fakeParents{})
	if reg.TakeExpectedInterrupt("ses_a") {
		t.Error("nothing was announced")
	}
	reg.ExpectInterrupt("ses_a")
	if !reg.TakeExpectedInterrupt("ses_a") || reg.TakeExpectedInterrupt("ses_a") {
		t.Error("an announced interrupt must be recognised exactly once")
	}
}

func TestNewAndReloadedSainiksStartIdleUntilSeeded(t *testing.T) {
	r := newTestRegistry(t, &fakeParents{})
	r.Register("ses_a", "")
	r.Register("ses_b", "")
	if inst, _ := r.Get("ses_a"); inst.Status != "idle" {
		t.Errorf("a just-created session has not been given work yet: %q", inst.Status)
	}
	r.SeedRunning(map[string]bool{"ses_b": true})
	a, _ := r.Get("ses_a")
	b, _ := r.Get("ses_b")
	if a.Status != "idle" || b.Status != "running" {
		t.Errorf("after seeding: a=%q b=%q", a.Status, b.Status)
	}
}
