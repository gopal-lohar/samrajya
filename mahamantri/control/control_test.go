package control

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopal-lohar/samrajya/mahamantri/attention"
	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

// fakeOpencode stands in for the proxied opencode server: it records what
// reached it and answers like the real one (session create returns
// {"data":{"id":...}}).
type fakeOpencode struct {
	requests []string // "METHOD path?query"
	bodies   []map[string]any
	status   int
}

func (f *fakeOpencode) Proxy(prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, prefix)
		if r.URL.RawQuery != "" {
			path += "?" + r.URL.RawQuery
		}
		f.requests = append(f.requests, r.Method+" "+path)
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		f.bodies = append(f.bodies, body)
		if f.status != 0 {
			w.WriteHeader(f.status)
			w.Write([]byte(`{"error":"nope"}`))
			return
		}
		if r.Method == http.MethodPost && path == "/api/session" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":{"id":"ses_new","title":"x"}}`))
			return
		}
		w.Write([]byte(`{"data":{}}`))
	})
}

type fakeReg struct {
	registered   map[string]string
	unregistered []string
	byIssue      map[string][]attention.Instance
	protected    string
	interrupted  []string
	regErr       error
}

func (f *fakeReg) Register(id, label string) (attention.Instance, error) {
	if f.regErr != nil {
		return attention.Instance{}, f.regErr
	}
	f.registered[id] = label
	return attention.Instance{SessionID: id, Label: label}, nil
}
func (f *fakeReg) Unregister(id string) error {
	f.unregistered = append(f.unregistered, id)
	return nil
}
func (f *fakeReg) ForIssue(issue string) []attention.Instance { return f.byIssue[issue] }
func (f *fakeReg) IsProtected(id string) bool                 { return id == f.protected }
func (f *fakeReg) ExpectInterrupt(id string)                  { f.interrupted = append(f.interrupted, id) }

var sainikRules = []opencode.PermissionRule{{Action: "*", Resource: "*", Effect: "allow"}}

func setup(t *testing.T, opt Options) (*http.ServeMux, *fakeOpencode, *fakeReg) {
	t.Helper()
	oc := &fakeOpencode{}
	reg := &fakeReg{registered: map[string]string{}}
	mux := http.NewServeMux()
	Routes(mux, oc, reg, opt)
	return mux, oc, reg
}

func do(mux http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestCreatingASessionMakesARegisteredSainikWithTheDefaults(t *testing.T) {
	mux, oc, reg := setup(t, Options{Directory: "/repo", DefaultModel: "openai/gpt-6-sol", Agent: "build", Permissions: sainikRules})
	rec := do(mux, "POST", "/opencode/api/session", `{"title":"sainik-SEN-33-request-info"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"ses_new"`) {
		t.Fatalf("response = %d %s", rec.Code, rec.Body)
	}
	b := oc.bodies[0]
	if b["location"].(map[string]any)["directory"] != "/repo" || b["agent"] != "build" {
		t.Errorf("defaults not applied: %v", b)
	}
	if m := b["model"].(map[string]any); m["providerID"] != "openai" || m["id"] != "gpt-6-sol" {
		t.Errorf("model = %v", m)
	}
	if p := b["permissions"].([]any); len(p) != 1 || p[0].(map[string]any)["effect"] != "allow" {
		t.Errorf("permissions = %v", p)
	}
	if reg.registered["ses_new"] != "sainik-SEN-33-request-info" {
		t.Errorf("registered = %v", reg.registered)
	}
}

func TestCreateKeepsWhatSenapatiSetItself(t *testing.T) {
	mux, oc, _ := setup(t, Options{Directory: "/repo", DefaultModel: "openai/gpt-6-sol", Permissions: sainikRules})
	do(mux, "POST", "/opencode/api/session", `{"title":"sainik-SEN-1-x","model":{"providerID":"opencode-go","id":"flash"},"location":{"directory":"/other"},"permissions":[]}`)
	b := oc.bodies[0]
	if b["model"].(map[string]any)["id"] != "flash" || b["location"].(map[string]any)["directory"] != "/other" || len(b["permissions"].([]any)) != 0 {
		t.Errorf("explicit values were overridden: %v", b)
	}
}

func TestCreateRequiresTheSainikTitle(t *testing.T) {
	for _, body := range []string{`{}`, `{"title":"fix the bug"}`, `{"title":"sainik-fix"}`, `{"title":"sainik-SEN-1-Has Spaces"}`, `not json`} {
		mux, oc, _ := setup(t, Options{})
		if rec := do(mux, "POST", "/opencode/api/session", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s -> %d, want 400", body, rec.Code)
		}
		if len(oc.requests) != 0 {
			t.Errorf("%s reached opencode", body)
		}
	}
}

// One issue, one sainik: a second one is refused and points at the first,
// unless the split is deliberate.
func TestCreateRefusesASecondSainikForTheIssue(t *testing.T) {
	mux, oc, reg := setup(t, Options{})
	reg.byIssue = map[string][]attention.Instance{"SEN-33": {{SessionID: "ses_old", Label: "sainik-SEN-33-plan"}}}
	rec := do(mux, "POST", "/opencode/api/session", `{"title":"sainik-sen-33-again"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "ses_old") || len(oc.requests) != 0 {
		t.Errorf("duplicate: %d %s %v", rec.Code, rec.Body, oc.requests)
	}
	rec = do(mux, "POST", "/opencode/api/session?parallel=true", `{"title":"sainik-SEN-33-part-two"}`)
	if rec.Code != http.StatusOK || oc.requests[0] != "POST /api/session" {
		t.Errorf("parallel: %d %v (the parallel flag must not reach opencode)", rec.Code, oc.requests)
	}
}

func TestFailedCreateIsNotRegistered(t *testing.T) {
	mux, oc, reg := setup(t, Options{})
	oc.status = http.StatusBadRequest
	if rec := do(mux, "POST", "/opencode/api/session", `{"title":"sainik-SEN-1-x"}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "nope") {
		t.Errorf("opencode's answer must be passed back: %d %s", rec.Code, rec.Body)
	}
	if len(reg.registered) != 0 {
		t.Error("nothing was created, nothing should be registered")
	}
}

func TestCreateSaysSoWhenRegistrationFails(t *testing.T) {
	mux, _, reg := setup(t, Options{})
	reg.regErr = errors.New("disk full")
	rec := do(mux, "POST", "/opencode/api/session", `{"title":"sainik-SEN-1-x"}`)
	if !strings.Contains(rec.Header().Get("X-Mahamantri-Warning"), "disk full") {
		t.Errorf("headers = %v", rec.Header())
	}
}

// Anything Senapati prompts is tagged as its own, or a person stepping in
// can't be told apart from it; and it is queued unless it says otherwise.
func TestPromptsAreTaggedAndQueuedByDefault(t *testing.T) {
	mux, oc, _ := setup(t, Options{})
	do(mux, "POST", "/opencode/api/session/ses_a/prompt", `{"text":"hello","metadata":{"issue":"SEN-1","ctx":{"a":1}}}`)
	b := oc.bodies[0]
	md := b["metadata"].(map[string]any)
	if md["source"] != "senapati" || md["issue"] != "SEN-1" || b["text"] != "hello" || b["delivery"] != "queue" {
		t.Errorf("forwarded = %v", b)
	}
	do(mux, "POST", "/opencode/api/session/ses_a/prompt", `{"text":"now","delivery":"steer","metadata":{"source":"mahamantri"}}`)
	b = oc.bodies[1]
	if b["delivery"] != "steer" || b["metadata"].(map[string]any)["source"] != "senapati" {
		t.Errorf("explicit delivery must stay; the source must be forced: %v", b)
	}
}

func TestNonJSONPromptIsPassedThroughForOpencodeToReject(t *testing.T) {
	mux, oc, _ := setup(t, Options{})
	do(mux, "POST", "/opencode/api/session/ses_a/prompt", "not json")
	if len(oc.requests) != 1 {
		t.Error("should still reach opencode")
	}
}

// Regression: Senapati's own interrupt came back to it as "a person
// interrupted it" (opencode reports every API interrupt as reason "user").
func TestInterruptsAreRecordedAsSenapatis(t *testing.T) {
	mux, oc, reg := setup(t, Options{})
	do(mux, "POST", "/opencode/api/session/ses_a/interrupt", "")
	if strings.Join(reg.interrupted, ",") != "ses_a" || len(oc.requests) != 1 {
		t.Errorf("interrupted=%v requests=%v", reg.interrupted, oc.requests)
	}
}

func TestDeletingASessionUnregistersIt(t *testing.T) {
	mux, oc, reg := setup(t, Options{})
	do(mux, "DELETE", "/opencode/api/session/ses_a", "")
	do(mux, "DELETE", "/opencode/api/session/ses_b/message/msg_1", "")
	if strings.Join(reg.unregistered, ",") != "ses_a" || len(oc.requests) != 2 {
		t.Errorf("unregistered=%v requests=%v", reg.unregistered, oc.requests)
	}
	oc.status = http.StatusNotFound
	do(mux, "DELETE", "/opencode/api/session/ses_c", "")
	if len(reg.unregistered) != 1 {
		t.Error("a failed delete must not unregister")
	}
}

func TestGatewayRefusesToWait(t *testing.T) {
	mux, oc, _ := setup(t, Options{})
	for _, path := range []string{"/opencode/api/experimental/session/ses_a/wait", "/opencode/api/session/ses_a/wait"} {
		if rec := do(mux, "POST", path, ""); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "never waits") {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	if len(oc.requests) != 0 {
		t.Error("a wait request reached opencode")
	}
}

// Senapati must not be able to lift its own permissions through the gateway.
func TestGatewayProtectsSenapatisOwnSession(t *testing.T) {
	mux, oc, reg := setup(t, Options{})
	reg.protected = "ses_senapati"
	for _, c := range []struct{ method, path string }{
		{"PATCH", "/opencode/api/session/ses_senapati"},
		{"POST", "/opencode/api/session/ses_senapati/agent"},
		{"DELETE", "/opencode/api/experimental/session/ses_senapati/instructions/entries/samrajya.senapati"},
	} {
		if rec := do(mux, c.method, c.path, `{"permissions":[]}`); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: %d", c.method, c.path, rec.Code)
		}
	}
	if len(oc.requests) != 0 {
		t.Errorf("reached opencode: %v", oc.requests)
	}
	do(mux, "GET", "/opencode/api/session/ses_senapati", "")
	do(mux, "PATCH", "/opencode/api/session/ses_sainik", `{"title":"x"}`)
	if len(oc.requests) != 2 {
		t.Errorf("reading its own session and changing a sainik must pass: %v", oc.requests)
	}
}

func TestEverythingElsePassesThroughUntouched(t *testing.T) {
	mux, oc, _ := setup(t, Options{})
	do(mux, "GET", "/opencode/api/session/ses_a/message?type=assistant&order=desc&limit=1", "")
	do(mux, "POST", "/opencode/api/session/ses_a/permission/per_1/reply", `{"decision":"once"}`)
	if oc.requests[0] != "GET /api/session/ses_a/message?type=assistant&order=desc&limit=1" || oc.bodies[1]["decision"] != "once" || oc.bodies[1]["metadata"] != nil {
		t.Errorf("requests=%v bodies=%v", oc.requests, oc.bodies)
	}
}

// Regression: Senapati probed GET /openapi.json, got a bare 404, and gave up.
func TestUnknownPathsAnswerWithTheAPI(t *testing.T) {
	mux, oc, _ := setup(t, Options{})
	for _, path := range []string{"/openapi.json", "/", "/sainiks"} {
		rec := do(mux, "GET", path, "")
		if !strings.Contains(rec.Body.String(), "/opencode/api/session/{id}/prompt") {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	if len(oc.requests) != 0 {
		t.Error("help must not reach opencode")
	}
}
