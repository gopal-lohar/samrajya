package senapati

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
	"github.com/gopal-lohar/samrajya/mahamantri/state"
)

type fakeSender struct {
	mu            sync.Mutex
	created       []opencode.CreateSessionRequest
	createErr     error
	getErr        error
	promptErrs    []error // consumed one per Prompt call; nil entry = success
	prompts       []opencode.PromptRequest
	promptTargets []string
	assistant     opencode.AssistantMessage
	hasAssistant  bool
	models        []opencode.ModelInfo
	nextID        int
	switchedModel []opencode.SessionModel
	switchedAgent []string
	switchErr     error
	permissions   map[string][]opencode.PermissionRule
	instructions  map[string]string // sessionID -> durable role text
	instructErr   error             // e.g. an older server without instruction entries
}

func (f *fakeSender) CreateSession(ctx context.Context, req opencode.CreateSessionRequest) (opencode.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return opencode.SessionInfo{}, f.createErr
	}
	f.created = append(f.created, req)
	f.nextID++
	return opencode.SessionInfo{ID: "ses_" + strconv.Itoa(f.nextID), Title: req.Title}, nil
}

func (f *fakeSender) GetSession(ctx context.Context, sessionID string) (opencode.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return opencode.SessionInfo{}, f.getErr
	}
	return opencode.SessionInfo{ID: sessionID}, nil
}

func (f *fakeSender) Prompt(ctx context.Context, sessionID string, req opencode.PromptRequest) (opencode.PromptResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.promptErrs) > 0 {
		err := f.promptErrs[0]
		f.promptErrs = f.promptErrs[1:]
		if err != nil {
			return opencode.PromptResponse{}, err
		}
	}
	f.prompts = append(f.prompts, req)
	f.promptTargets = append(f.promptTargets, sessionID)
	return opencode.PromptResponse{ID: "msg", SessionID: sessionID}, nil
}

func (f *fakeSender) SwitchModel(ctx context.Context, sessionID string, model opencode.SessionModel) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.switchErr != nil {
		return f.switchErr
	}
	f.switchedModel = append(f.switchedModel, model)
	return nil
}

func (f *fakeSender) SwitchAgent(ctx context.Context, sessionID, agent string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.switchedAgent = append(f.switchedAgent, agent)
	return nil
}

func (f *fakeSender) SetPermissions(ctx context.Context, sessionID string, rules []opencode.PermissionRule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.permissions == nil {
		f.permissions = map[string][]opencode.PermissionRule{}
	}
	f.permissions[sessionID] = rules
	return nil
}

func (f *fakeSender) PutInstruction(ctx context.Context, sessionID, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.instructErr != nil {
		return f.instructErr
	}
	if key != InstructionKey {
		return errors.New("unexpected instruction key " + key)
	}
	if f.instructions == nil {
		f.instructions = map[string]string{}
	}
	f.instructions[sessionID] = value
	return nil
}

func (f *fakeSender) ListModels(ctx context.Context) ([]opencode.ModelInfo, error) {
	return f.models, nil
}

func (f *fakeSender) LatestAssistantMessage(ctx context.Context, sessionID string) (opencode.AssistantMessage, bool, error) {
	return f.assistant, f.hasAssistant, nil
}

func (f *fakeSender) promptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

func newTestManager(t *testing.T, f *fakeSender) *Manager {
	t.Helper()
	m := New(f, filepath.Join(t.TempDir(), "senapati.json"), 0.4, nil)
	m.retryBase = time.Millisecond
	return m
}

func started(t *testing.T, f *fakeSender) *Manager {
	t.Helper()
	m := newTestManager(t, f)
	if _, err := m.Start(context.Background(), opencode.CreateSessionRequest{}); err != nil {
		t.Fatal(err)
	}
	return m
}

func highUsage(f *fakeSender) {
	f.hasAssistant = true
	f.assistant = opencode.AssistantMessage{ModelID: "m1", ProviderID: "p1", Tokens: opencode.TokenUsage{Input: 500}}
	f.models = []opencode.ModelInfo{{ID: "m1", ProviderID: "p1", Limit: opencode.ModelLimit{Context: 1000}}}
}

func TestStartCreatesFreshWhenNoState(t *testing.T) {
	f := &fakeSender{}
	m := started(t, f)
	if len(f.created) != 1 || f.created[0].Title != "Senapati-1" || m.SessionID() == "" {
		t.Errorf("created=%+v id=%q", f.created, m.SessionID())
	}
}

func TestStartResumesWhenSessionStillExists(t *testing.T) {
	f := &fakeSender{}
	m := started(t, f)
	first := m.SessionID()

	m2 := New(f, m.statePath, 0.4, nil)
	second, err := m2.Start(context.Background(), opencode.CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if second != first || len(f.created) != 1 {
		t.Errorf("second=%q first=%q created=%d, want a resume without creating another", second, first, len(f.created))
	}
}

func TestStartCreatesFreshOnlyWhenServerSaysSessionIsGone(t *testing.T) {
	f := &fakeSender{}
	m := started(t, f)
	first := m.SessionID()

	f.getErr = opencode.ErrSessionNotFound
	m2 := New(f, m.statePath, 0.4, nil)
	second, err := m2.Start(context.Background(), opencode.CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if second == first || len(f.created) != 2 {
		t.Fatalf("second=%q first=%q created=%d, want a fresh session", second, first, len(f.created))
	}
	if got := f.created[1].Title; got != "Senapati-2" {
		t.Errorf("replacement title = %q, want Senapati-2 (numbers must not be reused)", got)
	}
}

// Regression: any GetSession error used to be read as "session gone", so a
// wrong password or a server blip silently created a duplicate Senapati.
func TestStartDoesNotDuplicateSessionOnOtherErrors(t *testing.T) {
	for _, getErr := range []error{opencode.ErrUnauthorized, errors.New("connection refused")} {
		f := &fakeSender{}
		m := started(t, f)

		f.getErr = getErr
		m2 := New(f, m.statePath, 0.4, nil)
		if _, err := m2.Start(context.Background(), opencode.CreateSessionRequest{}); !errors.Is(err, getErr) {
			t.Errorf("Start err = %v, want it to wrap %v", err, getErr)
		}
		if len(f.created) != 1 {
			t.Errorf("getErr=%v created a duplicate session (%d created)", getErr, len(f.created))
		}
	}
}

func TestStartMigratesStateWrittenBeforeSeqExisted(t *testing.T) {
	f := &fakeSender{}
	m := newTestManager(t, f)
	old := state.SenapatiState{
		Current: state.SenapatiRecord{SessionID: "ses_gone", Title: "Senapati-2"},
		History: []state.SenapatiRecord{{SessionID: "ses_first", Title: "Senapati-1"}},
	}
	if err := state.Save(m.statePath, old); err != nil {
		t.Fatal(err)
	}
	f.getErr = opencode.ErrSessionNotFound
	if _, err := m.Start(context.Background(), opencode.CreateSessionRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := f.created[0].Title; got != "Senapati-3" {
		t.Errorf("title = %q, want Senapati-3", got)
	}
}

func TestDeliverTagsMetadataAndUsesQueue(t *testing.T) {
	f := &fakeSender{}
	m := started(t, f)
	m.deliver(context.Background(), "hello senapati")
	if len(f.prompts) != 1 {
		t.Fatalf("prompts = %d, want 1", len(f.prompts))
	}
	got := f.prompts[0]
	if got.Text != "hello senapati" || got.Delivery != "queue" || got.Metadata["source"] != "mahamantri" {
		t.Errorf("sent %+v", got)
	}
}

// Linear updates made seconds apart must reach Senapati in the order they
// arrived - it used to spawn one goroutine per webhook, with no ordering.
func TestForwardDeliversInArrivalOrder(t *testing.T) {
	f := &fakeSender{}
	m := started(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	want := []string{"assigned to Senapati", "assigned to Gopal", "assigned to Senapati"}
	for _, text := range want {
		if err := m.Forward(text); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.promptCount() < len(want) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, text := range want {
		if i >= len(f.prompts) || f.prompts[i].Text != text {
			t.Fatalf("delivery order broken at %d: got %+v", i, f.prompts)
		}
	}
}

func TestForwardNeverBlocksAndReportsFullOutbox(t *testing.T) {
	m := started(t, &fakeSender{}) // no worker running: the outbox just fills
	for i := 0; i < outboxSize; i++ {
		if err := m.Forward("x"); err != nil {
			t.Fatalf("Forward %d: %v", i, err)
		}
	}
	if err := m.Forward("one too many"); err == nil {
		t.Error("expected an error once the outbox is full")
	}
}

// A message must survive a temporarily unreachable server or a stale
// password that gets fixed while it waits.
func TestDeliverRetriesTransientFailures(t *testing.T) {
	f := &fakeSender{promptErrs: []error{opencode.ErrUnauthorized, errors.New("connection refused"), nil}}
	m := started(t, f)
	m.deliver(context.Background(), "important")
	if f.promptCount() != 1 || f.prompts[0].Text != "important" {
		t.Errorf("prompts = %+v, want the message delivered after retries", f.prompts)
	}
	if m.LastError() != nil {
		t.Errorf("LastError = %v, want cleared after a successful delivery", m.LastError())
	}
}

func TestDeliverDropsRejectedMessageAndReportsIt(t *testing.T) {
	f := &fakeSender{promptErrs: []error{opencode.ErrBadRequest}}
	m := started(t, f)
	m.deliver(context.Background(), "bad") // must return instead of retrying forever
	if f.promptCount() != 0 {
		t.Error("a rejected message must not be delivered")
	}
	if !errors.Is(m.LastError(), opencode.ErrBadRequest) {
		t.Errorf("LastError = %v, want ErrBadRequest", m.LastError())
	}
}

// Regression: a failed usage check used to abort delivery entirely, so a
// model missing from the catalog silently stopped every message.
func TestDeliveryIsNotBlockedByUsageCheckFailure(t *testing.T) {
	f := &fakeSender{hasAssistant: true, assistant: opencode.AssistantMessage{ModelID: "unlisted", ProviderID: "p"}}
	m := started(t, f)
	m.deliver(context.Background(), "still delivered")
	if f.promptCount() != 1 {
		t.Fatal("message not delivered because the usage check failed")
	}
	if m.LastError() == nil {
		t.Error("the usage-check problem should still be reported")
	}
}

func TestRotatesAtThresholdAndHandsOff(t *testing.T) {
	f := &fakeSender{}
	highUsage(f)
	m := started(t, f)
	first := m.SessionID()

	m.deliver(context.Background(), "trigger rotation")

	if m.SessionID() == first {
		t.Fatal("session should have rotated")
	}
	if h := m.History(); len(h) != 1 || h[0].SessionID != first || h[0].RotatedAt == nil {
		t.Errorf("History() = %+v, want the retired first session with RotatedAt set", h)
	}
	if f.created[1].Title != "Senapati-2" {
		t.Errorf("rotated title = %q", f.created[1].Title)
	}
	// handoff first, then the forwarded message, both into the NEW session
	if len(f.prompts) != 2 || f.promptTargets[0] != m.SessionID() || f.promptTargets[1] != m.SessionID() {
		t.Errorf("prompts=%d targets=%v, want handoff + message both sent to %s", len(f.prompts), f.promptTargets, m.SessionID())
	}
	saved, _, _ := state.Load(m.statePath)
	if saved.Current.SessionID != m.SessionID() || saved.Seq != 2 || len(saved.History) != 1 {
		t.Errorf("persisted state = %+v", saved)
	}
}

// Regression: a failed rotation used to append the old session to history
// before creating the new one, leaving it both current and retired.
func TestFailedRotationLeavesStateUntouched(t *testing.T) {
	f := &fakeSender{}
	highUsage(f)
	m := started(t, f)
	first := m.SessionID()

	f.createErr = errors.New("server hiccup")
	m.deliver(context.Background(), "msg")

	if m.SessionID() != first || len(m.History()) != 0 {
		t.Errorf("current=%q history=%+v, want the original session untouched", m.SessionID(), m.History())
	}
	if f.promptCount() != 1 || f.promptTargets[0] != first {
		t.Error("the message should still be delivered to the current session")
	}
}

func TestDoesNotRotateBelowThreshold(t *testing.T) {
	f := &fakeSender{}
	highUsage(f)
	f.assistant.Tokens.Input = 100
	m := started(t, f)
	first := m.SessionID()
	m.deliver(context.Background(), "no rotation needed")
	if m.SessionID() != first || len(f.created) != 1 {
		t.Errorf("rotated unexpectedly: id=%q created=%d", m.SessionID(), len(f.created))
	}
}

func withBriefing(m *Manager) *Manager {
	m.SetBriefing(func() (string, error) { return "BRIEFING", nil })
	return m
}

// The user's Senapati never got a role at all - it was a stock agent that
// just said "Noted". Every new session must be briefed before anything else:
// the role as a durable instruction (a first message is lost when opencode
// compacts the context, and with it Senapati's idea that it is a manager),
// then a short activation message.
func TestNewSessionIsBriefedBeforeAnythingElse(t *testing.T) {
	f := &fakeSender{}
	m := withBriefing(newTestManager(t, f))
	if _, err := m.Start(context.Background(), opencode.CreateSessionRequest{}); err != nil {
		t.Fatal(err)
	}
	if f.instructions[m.SessionID()] != "BRIEFING" {
		t.Fatalf("durable instruction = %q, want the briefing", f.instructions[m.SessionID()])
	}
	if len(f.prompts) != 1 || f.prompts[0].Text != activation || f.prompts[0].Metadata["source"] != "mahamantri" {
		t.Fatalf("prompts = %+v, want exactly the activation first", f.prompts)
	}
	if m.Current().BriefedAt == nil {
		t.Error("BriefedAt not recorded")
	}
	m.deliver(context.Background(), "first real message")
	if f.prompts[1].Text != "first real message" {
		t.Errorf("order = %+v", f.prompts)
	}
}

func TestResumedSessionIsBriefedOnlyIfItNeverWas(t *testing.T) {
	f := &fakeSender{}
	first := newTestManager(t, f) // no briefing configured: like the user's existing session
	first.Start(context.Background(), opencode.CreateSessionRequest{})
	if len(f.prompts) != 0 {
		t.Fatal("no briefing configured, nothing should have been sent")
	}

	second := withBriefing(New(f, first.statePath, 0.4, nil))
	if _, err := second.Start(context.Background(), opencode.CreateSessionRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(f.prompts) != 1 {
		t.Fatalf("an unbriefed session should be briefed on resume: %d prompts", len(f.prompts))
	}

	third := withBriefing(New(f, first.statePath, 0.4, nil))
	third.SetBriefing(func() (string, error) { return "EDITED BRIEFING", nil })
	third.Start(context.Background(), opencode.CreateSessionRequest{})
	if len(f.prompts) != 1 {
		t.Errorf("an already-briefed session must not be messaged again: %d prompts", len(f.prompts))
	}
	if f.instructions[third.SessionID()] != "EDITED BRIEFING" {
		t.Errorf("a resumed session's role must be refreshed from the current instructions, got %q", f.instructions[third.SessionID()])
	}
}

// Older servers have no instruction entries: the briefing goes back to being
// the first message, as before.
func TestBriefingFallsBackToAMessageWithoutInstructionEntries(t *testing.T) {
	f := &fakeSender{instructErr: opencode.ErrSessionNotFound}
	m := withBriefing(newTestManager(t, f))
	if _, err := m.Start(context.Background(), opencode.CreateSessionRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(f.prompts) != 1 || !strings.HasPrefix(f.prompts[0].Text, "BRIEFING") || !strings.Contains(f.prompts[0].Text, "one short line") {
		t.Fatalf("prompts = %+v, want the whole briefing as the first message", f.prompts)
	}
}

// The guardrail that keeps Senapati from doing sainik work must also reach a
// session created before it existed - like the one the user already has.
func TestPermissionsOnCreateAndOnResume(t *testing.T) {
	f := &fakeSender{}
	rules := Permissions("http://127.0.0.1:4097", nil)
	m := newTestManager(t, f)
	m.Start(context.Background(), opencode.CreateSessionRequest{Permissions: rules})
	if len(f.created[0].Permissions) != len(rules) {
		t.Errorf("created with %d rules, want %d", len(f.created[0].Permissions), len(rules))
	}
	if len(f.permissions) != 0 {
		t.Error("a new session gets its rules at creation, not via a separate call")
	}

	m2 := New(f, m.statePath, 0.4, nil)
	if _, err := m2.Start(context.Background(), opencode.CreateSessionRequest{Permissions: rules}); err != nil {
		t.Fatal(err)
	}
	if len(f.permissions[m2.SessionID()]) != len(rules) {
		t.Errorf("resumed session permissions = %+v", f.permissions[m2.SessionID()])
	}
}

func TestRotatedSessionKeepsThePermissions(t *testing.T) {
	f := &fakeSender{}
	rules := Permissions("http://127.0.0.1:4097", nil)
	m := newTestManager(t, f)
	m.Start(context.Background(), opencode.CreateSessionRequest{Permissions: rules})
	highUsage(f)
	m.deliver(context.Background(), "trigger")
	if len(f.created) != 2 || len(f.created[1].Permissions) != len(rules) {
		t.Errorf("rotated session created without the permissions: %+v", f.created)
	}
}

// Regression: sessions were created without a model, so they silently ran on
// the server default (a free model) instead of the one the user chose.
func TestResumedSessionIsPinnedToConfiguredModelAndAgent(t *testing.T) {
	f := &fakeSender{}
	m := newTestManager(t, f)
	m.Start(context.Background(), opencode.CreateSessionRequest{})

	want := opencode.SessionModel{ID: "gpt-6-sol", ProviderID: "openai"}
	m2 := New(f, m.statePath, 0.4, nil)
	if _, err := m2.Start(context.Background(), opencode.CreateSessionRequest{Agent: "senapati", Model: &want}); err != nil {
		t.Fatal(err)
	}
	if len(f.switchedModel) != 1 || f.switchedModel[0] != want {
		t.Errorf("switchedModel = %+v, want %+v", f.switchedModel, want)
	}
	if len(f.switchedAgent) != 1 || f.switchedAgent[0] != "senapati" {
		t.Errorf("switchedAgent = %v", f.switchedAgent)
	}
	if m2.Model() != "openai/gpt-6-sol" {
		t.Errorf("Model() = %q", m2.Model())
	}
}

func TestNewSessionGetsModelAtCreationNotViaSwitch(t *testing.T) {
	f := &fakeSender{}
	m := newTestManager(t, f)
	want := opencode.SessionModel{ID: "gpt-6-sol", ProviderID: "openai"}
	m.Start(context.Background(), opencode.CreateSessionRequest{Model: &want})
	if f.created[0].Model == nil || *f.created[0].Model != want || len(f.switchedModel) != 0 {
		t.Errorf("created=%+v switched=%+v", f.created, f.switchedModel)
	}
}

func TestStartFailsLoudlyWhenConfiguredModelIsRejected(t *testing.T) {
	f := &fakeSender{}
	m := newTestManager(t, f)
	m.Start(context.Background(), opencode.CreateSessionRequest{})

	f.switchErr = errors.New("unknown model openai/nope")
	m2 := New(f, m.statePath, 0.4, nil)
	_, err := m2.Start(context.Background(), opencode.CreateSessionRequest{Model: &opencode.SessionModel{ID: "nope", ProviderID: "openai"}})
	if err == nil || !strings.Contains(err.Error(), "openai/nope") {
		t.Errorf("err = %v, want it to name the model and carry the server's reason", err)
	}
}

func TestModelReportsWhatTheServerActuallyUsedWhenNoneConfigured(t *testing.T) {
	f := &fakeSender{}
	highUsage(f)
	f.assistant.ModelID, f.assistant.ProviderID = "space-bunny-free", "opencode-go"
	f.models = []opencode.ModelInfo{{ID: "space-bunny-free", ProviderID: "opencode-go", Limit: opencode.ModelLimit{Context: 1000000}}}
	m := started(t, f)
	if m.Model() != "" {
		t.Errorf("Model() = %q before any turn, want empty", m.Model())
	}
	m.deliver(context.Background(), "x")
	if m.Model() != "opencode-go/space-bunny-free" {
		t.Errorf("Model() = %q, want the model the latest turn used", m.Model())
	}
}

func TestRotationBriefsTheNewSessionInTheHandoffMessage(t *testing.T) {
	f := &fakeSender{}
	m := withBriefing(newTestManager(t, f))
	m.Start(context.Background(), opencode.CreateSessionRequest{})
	highUsage(f)
	f.prompts, f.promptTargets = nil, nil

	m.deliver(context.Background(), "trigger")

	if len(f.prompts) != 2 || !strings.HasPrefix(f.prompts[0].Text, activation) || !strings.Contains(f.prompts[0].Text, "fresh Senapati session") {
		t.Fatalf("prompts = %+v, want activation+handoff first, then the message", f.prompts)
	}
	if f.instructions[m.SessionID()] != "BRIEFING" {
		t.Error("the rotated-to session needs its role as a durable instruction too")
	}
	if m.Current().BriefedAt == nil {
		t.Error("the rotated-to session should be marked briefed")
	}
}

func TestRotationStillHandsOffWhenBriefingCannotBeBuilt(t *testing.T) {
	f := &fakeSender{}
	m := newTestManager(t, f)
	m.Start(context.Background(), opencode.CreateSessionRequest{})
	m.SetBriefing(func() (string, error) { return "", errors.New("instructions file vanished") })
	highUsage(f)

	m.deliver(context.Background(), "trigger")

	if len(f.prompts) != 2 || !strings.Contains(f.prompts[0].Text, "fresh Senapati session") {
		t.Errorf("handoff must still be sent: %+v", f.prompts)
	}
	if m.LastError() == nil || !strings.Contains(m.LastError().Error(), "instructions file vanished") {
		t.Errorf("LastError = %v, want the briefing problem reported", m.LastError())
	}
}
