package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gopal-lohar/samrajya/mahamantri/attention"
	"github.com/gopal-lohar/samrajya/mahamantri/claudecode"
	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

type fakeSessions struct {
	specs       map[string]claudecode.Spec
	prompts     []string // id|delivery|text
	interrupted []string
	deleted     []string
	models      map[string]string
	createErr   error
}

func (f *fakeSessions) Create(spec claudecode.Spec) (string, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	id := "u-" + string(rune('0'+len(f.specs)))
	f.specs[id] = spec
	return id, nil
}
func (f *fakeSessions) Prompt(id, text, delivery string) error {
	if _, ok := f.specs[id]; !ok {
		return opencode.ErrSessionNotFound
	}
	f.prompts = append(f.prompts, id+"|"+delivery+"|"+text)
	return nil
}
func (f *fakeSessions) Interrupt(id string) error {
	f.interrupted = append(f.interrupted, id)
	return nil
}
func (f *fakeSessions) Info(id string) (claudecode.Info, error) {
	if _, ok := f.specs[id]; !ok {
		return claudecode.Info{}, opencode.ErrSessionNotFound
	}
	return claudecode.Info{ID: id, State: "idle", LastReply: "done it"}, nil
}
func (f *fakeSessions) ActiveSessions(context.Context) (map[string]bool, error) {
	return map[string]bool{"u-0": true}, nil
}
func (f *fakeSessions) SetModel(id, model string) error { f.models[id] = model; return nil }
func (f *fakeSessions) Delete(id string) error          { f.deleted = append(f.deleted, id); return nil }
func (f *fakeSessions) Limits() []claudecode.Limit {
	return []claudecode.Limit{{Provider: "anthropic", Status: "allowed"}}
}

func claudeSetup(t *testing.T, opt ClaudeOptions) (*http.ServeMux, *fakeSessions, *fakeReg) {
	t.Helper()
	s := &fakeSessions{specs: map[string]claudecode.Spec{}, models: map[string]string{}}
	reg := &fakeReg{registered: map[string]string{}}
	mux := http.NewServeMux()
	ClaudeRoutes(mux, s, reg, opt)
	return mux, s, reg
}

func TestClaudeCreateMakesARegisteredSainikAndSendsItsTask(t *testing.T) {
	mux, s, reg := claudeSetup(t, ClaudeOptions{Directory: "/repo", DefaultModel: "claude-opus-5", Args: []string{"--dangerously-skip-permissions"}})
	rec := do(mux, "POST", "/claude/session", `{"title":"sainik-ENG-33-fix-login","text":"Plan ENG-33."}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var out map[string]string
	json.Unmarshal(rec.Body.Bytes(), &out)
	spec := s.specs[out["id"]]
	if spec.Dir != "/repo" || spec.Model != "claude-opus-5" || spec.Title != "sainik-ENG-33-fix-login" || spec.Args[0] != "--dangerously-skip-permissions" {
		t.Errorf("spec = %+v", spec)
	}
	if reg.registered[out["id"]] != "sainik-ENG-33-fix-login" {
		t.Errorf("registered = %v", reg.registered)
	}
	if len(s.prompts) != 1 || s.prompts[0] != out["id"]+"|queue|Plan ENG-33." {
		t.Errorf("prompts = %v", s.prompts)
	}
}

func TestClaudeCreateKeepsAnExplicitModelAndAllowsNoTask(t *testing.T) {
	mux, s, _ := claudeSetup(t, ClaudeOptions{DefaultModel: "claude-opus-5"})
	rec := do(mux, "POST", "/claude/session", `{"title":"sainik-ENG-1-x","model":"gpt-6-sol"}`)
	var out map[string]string
	json.Unmarshal(rec.Body.Bytes(), &out)
	if s.specs[out["id"]].Model != "gpt-6-sol" || len(s.prompts) != 0 {
		t.Errorf("spec=%+v prompts=%v", s.specs[out["id"]], s.prompts)
	}
}

func TestClaudeCreateEnforcesTitleAndOneSainikPerIssue(t *testing.T) {
	mux, s, reg := claudeSetup(t, ClaudeOptions{})
	for _, body := range []string{`{"title":"fix it"}`, `{}`, `nope`} {
		if rec := do(mux, "POST", "/claude/session", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s -> %d", body, rec.Code)
		}
	}
	reg.byIssue = map[string][]attention.Instance{"ENG-1": {{SessionID: "u-old", Label: "sainik-ENG-1-a"}}}
	if rec := do(mux, "POST", "/claude/session", `{"title":"sainik-ENG-1-b"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "u-old") {
		t.Errorf("duplicate: %d %s", rec.Code, rec.Body)
	}
	if len(s.specs) != 0 {
		t.Error("nothing should have been created")
	}
	if rec := do(mux, "POST", "/claude/session?parallel=true", `{"title":"sainik-ENG-1-b"}`); rec.Code != http.StatusCreated {
		t.Errorf("parallel: %d", rec.Code)
	}
}

func TestClaudePromptInterruptModelInfoDelete(t *testing.T) {
	mux, s, reg := claudeSetup(t, ClaudeOptions{})
	s.specs["u-9"] = claudecode.Spec{}
	if rec := do(mux, "POST", "/claude/session/u-9/prompt", `{"text":"more"}`); rec.Code != http.StatusOK || s.prompts[0] != "u-9|queue|more" {
		t.Errorf("prompt: %d %v", rec.Code, s.prompts)
	}
	do(mux, "POST", "/claude/session/u-9/prompt", `{"text":"now","delivery":"steer"}`)
	if s.prompts[1] != "u-9|steer|now" {
		t.Errorf("steer: %v", s.prompts)
	}
	for _, bad := range []string{`{"text":""}`, `{"text":"x","delivery":"interrupt"}`} {
		if rec := do(mux, "POST", "/claude/session/u-9/prompt", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s -> %d", bad, rec.Code)
		}
	}
	if rec := do(mux, "POST", "/claude/session/missing/prompt", `{"text":"x"}`); rec.Code != http.StatusNotFound {
		t.Errorf("missing: %d", rec.Code)
	}
	do(mux, "POST", "/claude/session/u-9/interrupt", "")
	do(mux, "POST", "/claude/session/u-9/model", `{"model":"gpt-6-luna"}`)
	if len(s.interrupted) != 1 || s.models["u-9"] != "gpt-6-luna" {
		t.Errorf("interrupted=%v models=%v", s.interrupted, s.models)
	}
	if rec := do(mux, "GET", "/claude/session/u-9", ""); !strings.Contains(rec.Body.String(), `"lastReply":"done it"`) {
		t.Errorf("info: %s", rec.Body)
	}
	if rec := do(mux, "GET", "/claude/session/active", ""); !strings.Contains(rec.Body.String(), `"u-0":{"type":"running"}`) {
		t.Errorf("active: %s", rec.Body)
	}
	if rec := do(mux, "GET", "/claude/limits", ""); !strings.Contains(rec.Body.String(), `"provider":"anthropic"`) {
		t.Errorf("limits: %s", rec.Body)
	}
	if rec := do(mux, "DELETE", "/claude/session/u-9", ""); rec.Code != http.StatusNoContent || reg.unregistered[0] != "u-9" || s.deleted[0] != "u-9" {
		t.Errorf("delete: %d %v %v", rec.Code, reg.unregistered, s.deleted)
	}
}

// Senapati must not be able to drive its own session through the API.
func TestClaudeAPIProtectsSenapatisOwnSession(t *testing.T) {
	mux, s, reg := claudeSetup(t, ClaudeOptions{})
	reg.protected = "u-sen"
	s.specs["u-sen"] = claudecode.Spec{}
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/claude/session/u-sen/prompt", `{"text":"x"}`},
		{"POST", "/claude/session/u-sen/interrupt", ""},
		{"POST", "/claude/session/u-sen/model", `{"model":"x"}`},
		{"DELETE", "/claude/session/u-sen", ""},
	} {
		if rec := do(mux, c.method, c.path, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: %d", c.method, c.path, rec.Code)
		}
	}
	if len(s.prompts)+len(s.interrupted)+len(s.deleted)+len(s.models) != 0 {
		t.Error("something reached the session")
	}
}

func TestClaudeUnknownPathsAnswerWithTheAPI(t *testing.T) {
	mux, _, _ := claudeSetup(t, ClaudeOptions{})
	for _, path := range []string{"/", "/opencode/api/session", "/sainiks"} {
		if rec := do(mux, "GET", path, ""); !strings.Contains(rec.Body.String(), "/claude/session/{id}/prompt") {
			t.Errorf("%s: %s", path, rec.Body)
		}
	}
}
