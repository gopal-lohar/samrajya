package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gopal-lohar/samrajya/mahamantri/attention"
	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

type fakeBackend struct {
	mu          sync.Mutex
	calls       []string
	created     []opencode.CreateSessionRequest
	prompts     []opencode.PromptRequest
	createErr   error
	promptErr   error
	interrupt   error
	snap        opencode.Snapshot
	proxied     []*http.Request
	proxiedBody []string
}

func (f *fakeBackend) CreateSession(ctx context.Context, req opencode.CreateSessionRequest) (opencode.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "create")
	if f.createErr != nil {
		return opencode.SessionInfo{}, f.createErr
	}
	f.created = append(f.created, req)
	return opencode.SessionInfo{ID: "ses_new", Title: req.Title}, nil
}

func (f *fakeBackend) Prompt(ctx context.Context, id string, req opencode.PromptRequest) (opencode.PromptResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "prompt:"+id)
	if f.promptErr != nil {
		return opencode.PromptResponse{}, f.promptErr
	}
	f.prompts = append(f.prompts, req)
	return opencode.PromptResponse{ID: "msg_1"}, nil
}

func (f *fakeBackend) Interrupt(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "interrupt:"+id)
	return f.interrupt
}

func (f *fakeBackend) Snapshot(ctx context.Context, id string) (opencode.Snapshot, error) {
	return f.snap, nil
}

func (f *fakeBackend) Proxy(prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.proxied = append(f.proxied, r)
		f.proxiedBody = append(f.proxiedBody, string(b))
		f.mu.Unlock()
		w.Write([]byte("proxied"))
	})
}

type fakeReg struct {
	calls     *[]string
	regErr    error
	phases    map[string]string
	byIssue   map[string][]attention.Instance
	protected string
}

func (f *fakeReg) ForIssue(issue string) []attention.Instance { return f.byIssue[issue] }
func (f *fakeReg) IsProtected(id string) bool                 { return id == f.protected }

func (f *fakeReg) Register(id, label string) (attention.Instance, error) {
	*f.calls = append(*f.calls, "register:"+label)
	return attention.Instance{SessionID: id, Label: label}, f.regErr
}

func (f *fakeReg) Update(id string, label, phase *string) (attention.Instance, error) {
	if phase != nil {
		f.phases[id] = *phase
	}
	return attention.Instance{}, nil
}

func setup(t *testing.T, opt Options) (*http.ServeMux, *fakeBackend, *fakeReg) {
	t.Helper()
	be := &fakeBackend{}
	reg := &fakeReg{calls: &be.calls, phases: map[string]string{}}
	mux := http.NewServeMux()
	Routes(mux, be, reg, opt)
	return mux, be, reg
}

func do(mux http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestSpawnCreatesRegistersThenSendsTheTaskInThatOrder(t *testing.T) {
	mux, be, reg := setup(t, Options{Directory: "/repo", DefaultModel: "opencode-go/deepseek-v4.1-flash"})
	rec := do(mux, "POST", "/sainiks", `{"issue":"SEN-30","slug":"Photos Version!","task":"Plan it, do not execute yet."}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	// Registered before the task is sent, so none of the session's events are missed.
	if strings.Join(be.calls, ",") != "create,register:sainik-SEN-30-photos-version,prompt:ses_new" {
		t.Errorf("call order = %v", be.calls)
	}
	c := be.created[0]
	if c.Title != "sainik-SEN-30-photos-version" || c.Model == nil || c.Model.ProviderID != "opencode-go" || c.Model.ID != "deepseek-v4.1-flash" || c.Location.Directory != "/repo" {
		t.Errorf("created = %+v (model %+v)", c, c.Model)
	}
	p := be.prompts[0]
	if p.Text != "Plan it, do not execute yet." || p.Metadata["source"] != "senapati" || p.Delivery != "queue" {
		t.Errorf("task prompt = %+v", p)
	}
	if reg.phases["ses_new"] != "started" {
		t.Errorf("phase = %q", reg.phases["ses_new"])
	}
	var out map[string]string
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["sessionID"] != "ses_new" || out["title"] != "sainik-SEN-30-photos-version" {
		t.Errorf("response = %v", out)
	}
}

func TestSpawnLetsSenapatiPickTheModelAndPhase(t *testing.T) {
	mux, be, reg := setup(t, Options{DefaultModel: "opencode-go/deepseek-v4.1-flash"})
	rec := do(mux, "POST", "/sainiks", `{"issue":"SEN-31","task":"x","model":"openai/gpt-6-sol","phase":"planning"}`)
	if rec.Code != http.StatusCreated {
		t.Fatal(rec.Body)
	}
	if m := be.created[0].Model; m.ProviderID != "openai" || m.ID != "gpt-6-sol" {
		t.Errorf("model = %+v, want the one Senapati chose", m)
	}
	if reg.phases["ses_new"] != "planning" {
		t.Errorf("phase = %q", reg.phases["ses_new"])
	}
	if be.created[0].Title != "sainik-SEN-31-task" {
		t.Errorf("title = %q, want the default slug", be.created[0].Title)
	}
}

func TestSpawnValidatesInputBeforeTouchingOpencode(t *testing.T) {
	for _, body := range []string{`{"task":"x"}`, `{"issue":"SEN-1"}`, `{"issue":"SEN-1","task":"x","model":"gpt-6-sol"}`, `not json`} {
		mux, be, _ := setup(t, Options{})
		if rec := do(mux, "POST", "/sainiks", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s -> %d, want 400", body, rec.Code)
		}
		if len(be.calls) != 0 {
			t.Errorf("%s reached opencode: %v", body, be.calls)
		}
	}
}

func TestSpawnReportsWhichStepFailedAndTheSessionIfOneExists(t *testing.T) {
	mux, be, _ := setup(t, Options{})
	be.createErr = errors.New("opencode: bad request: unknown model openai/nope")
	rec := do(mux, "POST", "/sainiks", `{"issue":"SEN-1","task":"x","model":"openai/nope"}`)
	if rec.Code == http.StatusCreated || !strings.Contains(rec.Body.String(), "unknown model openai/nope") {
		t.Errorf("create failure: %d %s", rec.Code, rec.Body)
	}

	mux, be, _ = setup(t, Options{})
	be.promptErr = errors.New("boom")
	rec = do(mux, "POST", "/sainiks", `{"issue":"SEN-1","task":"x"}`)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "ses_new") || !strings.Contains(rec.Body.String(), "task could not be sent") {
		t.Errorf("prompt failure should name the orphaned session: %d %s", rec.Code, rec.Body)
	}
}

func TestMessageWithInterruptInterruptsFirstThenSends(t *testing.T) {
	mux, be, _ := setup(t, Options{})
	rec := do(mux, "POST", "/sainiks/ses_a/message", `{"text":"stop testing, run only the login tests","interrupt":true}`)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	if strings.Join(be.calls, ",") != "interrupt:ses_a,prompt:ses_a" {
		t.Errorf("calls = %v", be.calls)
	}
	if be.prompts[0].Metadata["source"] != "senapati" {
		t.Errorf("message not tagged as Senapati's: %+v", be.prompts[0])
	}
}

// The default is to queue: a busy sainik gets the message when its current
// turn ends, so Senapati hands over a non-urgent update and moves on.
func TestMessageQueuesByDefaultWithoutInterrupting(t *testing.T) {
	mux, be, _ := setup(t, Options{})
	rec := do(mux, "POST", "/sainiks/ses_a/message", `{"text":"one more thing"}`)
	if strings.Join(be.calls, ",") != "prompt:ses_a" || be.prompts[0].Delivery != "queue" {
		t.Errorf("calls = %v prompts = %+v", be.calls, be.prompts)
	}
	if !strings.Contains(rec.Body.String(), `"delivery":"queue"`) {
		t.Errorf("response = %s", rec.Body)
	}
}

func TestMessageDeliveryModes(t *testing.T) {
	for body, want := range map[string]string{
		`{"text":"x","delivery":"queue"}`:     "prompt:ses_a/queue",
		`{"text":"x","delivery":"steer"}`:     "prompt:ses_a/steer",
		`{"text":"x","delivery":"interrupt"}`: "interrupt:ses_a,prompt:ses_a/steer",
		`{"text":"x","interrupt":true}`:       "interrupt:ses_a,prompt:ses_a/steer",
	} {
		mux, be, _ := setup(t, Options{})
		if rec := do(mux, "POST", "/sainiks/ses_a/message", body); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body)
		}
		if got := strings.Join(be.calls, ",") + "/" + be.prompts[0].Delivery; got != want {
			t.Errorf("%s -> %s, want %s", body, got, want)
		}
	}
	mux, be, _ := setup(t, Options{})
	if rec := do(mux, "POST", "/sainiks/ses_a/message", `{"text":"x","delivery":"later"}`); rec.Code != http.StatusBadRequest || len(be.calls) != 0 {
		t.Errorf("unknown delivery: %d calls=%v", rec.Code, be.calls)
	}
}

// One issue, one sainik: a second spawn for the same issue is refused and
// points at the one that exists, unless the split is deliberate.
func TestSpawnRefusesASecondSainikForTheSameIssue(t *testing.T) {
	mux, be, reg := setup(t, Options{})
	reg.byIssue = map[string][]attention.Instance{"SEN-31": {{SessionID: "ses_old", Label: "sainik-SEN-31-plan"}}}
	rec := do(mux, "POST", "/sainiks", `{"issue":"SEN-31","task":"x"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "/sainiks/ses_old/message") || len(be.calls) != 0 {
		t.Errorf("duplicate spawn: %d %s calls=%v", rec.Code, rec.Body, be.calls)
	}
	if rec := do(mux, "POST", "/sainiks", `{"issue":"SEN-31","task":"x","parallel":true}`); rec.Code != http.StatusCreated {
		t.Errorf("parallel spawn: %d %s", rec.Code, rec.Body)
	}
}

func TestMessageDoesNotSendIfTheInterruptFailed(t *testing.T) {
	mux, be, _ := setup(t, Options{})
	be.interrupt = opencode.ErrSessionNotFound
	rec := do(mux, "POST", "/sainiks/ses_gone/message", `{"text":"x","interrupt":true}`)
	if rec.Code != http.StatusNotFound || len(be.prompts) != 0 {
		t.Errorf("status=%d prompts=%d", rec.Code, len(be.prompts))
	}
}

func TestStatusReturnsTheSnapshot(t *testing.T) {
	mux, be, _ := setup(t, Options{})
	be.snap = opencode.Snapshot{SessionID: "ses_a", State: "running", RunningFor: "2h58m0s", Doing: &opencode.ToolActivity{Tool: "shell", Input: "npm test"}}
	rec := do(mux, "GET", "/sainiks/ses_a/status", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"runningFor":"2h58m0s"`) || !strings.Contains(rec.Body.String(), "npm test") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

// Anything through the gateway is Senapati's, and must be tagged as such or a
// person stepping in can't be told apart from it.
func TestGatewayTagsPromptsAsSenapatisAndPreservesTheRest(t *testing.T) {
	mux, be, _ := setup(t, Options{})
	do(mux, "POST", "/opencode/api/session/ses_a/prompt", `{"text":"hello","delivery":"steer","metadata":{"issue":"SEN-1"}}`)
	var sent map[string]any
	json.Unmarshal([]byte(be.proxiedBody[0]), &sent)
	md := sent["metadata"].(map[string]any)
	if md["source"] != "senapati" || md["issue"] != "SEN-1" || sent["text"] != "hello" || sent["delivery"] != "steer" {
		t.Errorf("forwarded body = %v", sent)
	}
	// A caller cannot claim to be someone else.
	do(mux, "POST", "/opencode/api/session/ses_a/prompt", `{"text":"x","metadata":{"source":"mahamantri"}}`)
	json.Unmarshal([]byte(be.proxiedBody[1]), &sent)
	if sent["metadata"].(map[string]any)["source"] != "senapati" {
		t.Errorf("source was not forced: %v", sent)
	}
}

func TestGatewayRefusesToWait(t *testing.T) {
	mux, be, _ := setup(t, Options{})
	for _, path := range []string{"/opencode/api/experimental/session/ses_a/wait", "/opencode/api/session/ses_a/wait"} {
		if rec := do(mux, "POST", path, ""); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "never waits") {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	if len(be.proxied) != 0 {
		t.Error("a wait request reached opencode")
	}
}

// Senapati must not be able to lift its own permissions through the gateway.
func TestGatewayProtectsSenapatisOwnSession(t *testing.T) {
	mux, be, reg := setup(t, Options{})
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
	if len(be.proxied) != 0 {
		t.Errorf("reached opencode: %d requests", len(be.proxied))
	}
	do(mux, "GET", "/opencode/api/session/ses_senapati", "")
	do(mux, "PATCH", "/opencode/api/session/ses_sainik", `{"title":"x"}`)
	if len(be.proxied) != 2 {
		t.Errorf("reading its own session and changing a sainik must pass: %d proxied", len(be.proxied))
	}
}

func TestGatewayLeavesEverythingElseUntouched(t *testing.T) {
	mux, be, _ := setup(t, Options{})
	body := `{"model":{"id":"deepseek-v4.1-flash","providerID":"opencode-go"}}`
	do(mux, "POST", "/opencode/api/session/ses_a/model", body)
	do(mux, "POST", "/opencode/api/session/ses_a/prompt/extra", `{"text":"x"}`)
	if be.proxiedBody[0] != body || strings.Contains(be.proxiedBody[1], "senapati") {
		t.Errorf("non-prompt requests must pass through untouched: %q", be.proxiedBody)
	}
}

func TestGatewayPassesNonJSONPromptBodiesThroughUnchanged(t *testing.T) {
	mux, be, _ := setup(t, Options{})
	do(mux, "POST", "/opencode/api/session/ses_a/prompt", "not json")
	if be.proxiedBody[0] != "not json" {
		t.Errorf("body = %q", be.proxiedBody[0])
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Photos Version!":                     "photos-version",
		"":                                    "task",
		"---":                                 "task",
		"Fix  the   login redirect (SEN-142)": "fix-the-login-redirect-sen-142",
		strings.Repeat("a", 60):               strings.Repeat("a", 40),
	} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
