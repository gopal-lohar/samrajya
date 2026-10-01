package senapati

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/attention"
	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
	"github.com/gopal-lohar/samrajya/mahamantri/state"
)

const (
	outboxSize     = 256
	attemptTimeout = 30 * time.Second
	maxRetryWait   = 10 * time.Second
)

// PromptSender is the subset of *opencode.Client Manager needs - kept
// narrow and interface-typed so tests can use a fake.
type PromptSender interface {
	CreateSession(ctx context.Context, req opencode.CreateSessionRequest) (opencode.SessionInfo, error)
	GetSession(ctx context.Context, sessionID string) (opencode.SessionInfo, error)
	Prompt(ctx context.Context, sessionID string, req opencode.PromptRequest) (opencode.PromptResponse, error)
	SwitchModel(ctx context.Context, sessionID string, model opencode.SessionModel) error
	SwitchAgent(ctx context.Context, sessionID, agent string) error
	SetPermissions(ctx context.Context, sessionID string, rules []opencode.PermissionRule) error
	PutInstruction(ctx context.Context, sessionID, key, value string) error
	ListModels(ctx context.Context) ([]opencode.ModelInfo, error)
	LatestAssistantMessage(ctx context.Context, sessionID string) (opencode.AssistantMessage, bool, error)
}

// Manager owns Senapati's session lifecycle: creating or resuming it on
// startup, rotating to a fresh session once context usage crosses
// threshold, and delivering messages into whichever session is current.
//
// Everything sent to Senapati goes through one FIFO outbox drained by a
// single worker (Run), which gives three guarantees: messages arrive in the
// order they were received (Linear updates made seconds apart must not be
// reordered), a slow or unreachable opencode server never blocks the
// webhook or the event loop, and rotation happens in exactly one place.
type Manager struct {
	client    PromptSender
	statePath string
	threshold float64
	registry  *attention.Registry
	retryBase time.Duration

	outbox chan string

	mu       sync.RWMutex // guards everything below; only Start and the worker write
	current  state.SenapatiRecord
	history  []state.SenapatiRecord
	seq      int
	baseReq  opencode.CreateSessionRequest // Start's request, minus Title - reused on every rotation
	lastErr  error
	briefing func() (string, error) // builds the briefing text; nil = none
	observed string                 // provider/model of the latest completed turn
}

func New(client PromptSender, statePath string, threshold float64, registry *attention.Registry) *Manager {
	return &Manager{
		client:    client,
		statePath: statePath,
		threshold: threshold,
		registry:  registry,
		retryBase: time.Second,
		outbox:    make(chan string, outboxSize),
	}
}

// SetBriefing sets how the briefing text is built (called for every new
// session, so edits to the instructions file apply at the next rotation).
func (m *Manager) SetBriefing(build func() (string, error)) { m.briefing = build }

// Model is the model Senapati runs on: the configured one, or - when none
// is configured and the server default applies - the one its latest turn
// actually used, or "" before it has answered anything.
func (m *Manager) Model() string {
	if md := m.baseReq.Model; md != nil {
		return md.ProviderID + "/" + md.ID
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.observed
}

// Start resolves Senapati's current session: it resumes the saved one if it
// still exists on the server, and creates a fresh "Senapati-<n>" only when
// there is none or the server says it is gone. Any other failure (wrong
// password, server down) aborts rather than creating a duplicate session.
func (m *Manager) Start(ctx context.Context, req opencode.CreateSessionRequest) (string, error) {
	m.baseReq = req

	saved, ok, err := state.Load(m.statePath)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", m.statePath, err)
	}
	m.history, m.seq = saved.History, saved.Seq
	if saved.Current.SessionID != "" && m.seq < len(m.history)+1 {
		m.seq = len(m.history) + 1 // state written before Seq existed
	}

	if ok && saved.Current.SessionID != "" {
		_, err := m.client.GetSession(ctx, saved.Current.SessionID)
		switch {
		case err == nil:
			m.current = saved.Current
			if err := m.prepare(ctx, true); err != nil {
				return "", err
			}
			return m.current.SessionID, nil
		case errors.Is(err, opencode.ErrSessionNotFound):
			// Deleted on the server - fall through and create a fresh one.
		default:
			return "", fmt.Errorf("checking saved Senapati session %s: %w", saved.Current.SessionID, err)
		}
	}

	rec, err := m.create(ctx)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.current, m.seq = rec, m.seq+1
	err = state.Save(m.statePath, m.stateLocked())
	m.mu.Unlock()
	if err != nil {
		return "", fmt.Errorf("saving %s: %w", m.statePath, err)
	}
	if err := m.prepare(ctx, false); err != nil {
		return "", err
	}
	return rec.SessionID, nil
}

// prepare makes the current session ready to act as Senapati. A resumed one
// is pinned to the configured agent, model and permissions first, because it
// may predate that config (a session made without a model silently runs on
// whatever the server's default is; one made without the permissions can do
// the work itself instead of handing it to a sainik). Then its role is
// installed, and a session that has never been briefed is told about it.
func (m *Manager) prepare(ctx context.Context, resumed bool) error {
	id := m.SessionID()
	if resumed {
		if a := m.baseReq.Agent; a != "" {
			if err := m.client.SwitchAgent(ctx, id, a); err != nil {
				return fmt.Errorf("switching Senapati to agent %q: %w", a, err)
			}
		}
		if md := m.baseReq.Model; md != nil {
			if err := m.client.SwitchModel(ctx, id, *md); err != nil {
				return fmt.Errorf("switching Senapati to model %s/%s: %w", md.ProviderID, md.ID, err)
			}
		}
		if p := m.baseReq.Permissions; p != nil {
			if err := m.client.SetPermissions(ctx, id, p); err != nil {
				return fmt.Errorf("restricting Senapati's tools: %w", err)
			}
		}
	}
	first, err := m.install(ctx, id)
	if err != nil || first == "" || m.Current().BriefedAt != nil {
		return err
	}
	if err := m.sendMessage(ctx, id, first); err != nil {
		return fmt.Errorf("briefing Senapati: %w", err)
	}
	return m.markBriefed()
}

// activation is the first message of a session whose role is installed as a
// durable instruction: the role itself is already in front of it.
const activation = "You are now running as Senapati. Your role, tools and rules are in your instructions " +
	"(" + InstructionKey + "); they replace anything earlier in this conversation. " +
	"Reply to this with one short line; do not act on it."

// install puts the briefing on the session as a durable instruction - on
// every start, so an edited instructions file reaches a resumed session -
// and returns the first message such a session should get: the short
// activation, or, when the server cannot hold instruction entries, the
// whole briefing as before. "" means there is no briefing configured.
func (m *Manager) install(ctx context.Context, id string) (first string, err error) {
	if m.briefing == nil {
		return "", nil
	}
	text, err := m.briefing()
	if err != nil {
		return "", err
	}
	if err := m.client.PutInstruction(ctx, id, InstructionKey, text); err != nil {
		return text + "\n\nReply to this briefing with one short line; do not act on it.", nil
	}
	return activation, nil
}

func (m *Manager) markBriefed() error {
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.current.BriefedAt = &now
	return state.Save(m.statePath, m.stateLocked())
}

func (m *Manager) sendMessage(ctx context.Context, sessionID, text string) error {
	_, err := m.client.Prompt(ctx, sessionID, opencode.PromptRequest{
		Text:     text,
		Delivery: "queue",
		Metadata: map[string]string{"source": attention.SourceMahamantri},
	})
	return err
}

// create makes the next "Senapati-<n>" session on the server without
// touching any manager state, so a failure leaves everything as it was.
func (m *Manager) create(ctx context.Context) (state.SenapatiRecord, error) {
	req := m.baseReq
	req.Title = fmt.Sprintf("Senapati-%d", m.seq+1)
	info, err := m.client.CreateSession(ctx, req)
	if err != nil {
		return state.SenapatiRecord{}, fmt.Errorf("creating session %s: %w", req.Title, err)
	}
	return state.SenapatiRecord{SessionID: info.ID, Title: req.Title, CreatedAt: time.Now().UTC()}, nil
}

func (m *Manager) stateLocked() state.SenapatiState {
	return state.SenapatiState{Current: m.current, History: m.history, Seq: m.seq}
}

func (m *Manager) SessionID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current.SessionID
}

func (m *Manager) Current() state.SenapatiRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

func (m *Manager) History() []state.SenapatiRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]state.SenapatiRecord(nil), m.history...)
}

// LastError is the most recent delivery or rotation problem, or nil once a
// later delivery succeeds cleanly.
func (m *Manager) LastError() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastErr
}

func (m *Manager) setError(err error) {
	m.mu.Lock()
	m.lastErr = err
	m.mu.Unlock()
}

// Forward queues text for delivery to Senapati. It never blocks on the
// network; it fails only if the outbox is full.
func (m *Manager) Forward(text string) error {
	select {
	case m.outbox <- text:
		return nil
	default:
		return fmt.Errorf("senapati outbox full (%d messages waiting) - is the opencode server reachable?", outboxSize)
	}
}

// Run drains the outbox in order until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	for {
		select {
		case text := <-m.outbox:
			m.deliver(ctx, text)
		case <-ctx.Done():
			return
		}
	}
}

// deliver sends one message, retrying transient failures (server down,
// wrong password - fixable while the message waits) with capped backoff.
// A message the server rejects outright is dropped and reported instead,
// so one bad message can't wedge the queue.
func (m *Manager) deliver(ctx context.Context, text string) {
	wait := m.retryBase
	for {
		err := m.send(ctx, text)
		if err == nil || errors.Is(err, opencode.ErrBadRequest) || errors.Is(err, opencode.ErrConflict) || errors.Is(err, opencode.ErrSessionNotFound) {
			return
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
		if wait *= 2; wait > maxRetryWait {
			wait = maxRetryWait
		}
	}
}

func (m *Manager) send(ctx context.Context, text string) error {
	ctx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()

	// Rotation is housekeeping: failing to measure or rotate must never stop
	// the message itself from being delivered.
	rotErr := m.rotateIfNeeded(ctx)

	err := m.sendMessage(ctx, m.SessionID(), text)
	if err != nil {
		m.setError(fmt.Errorf("delivering to Senapati: %w", err))
		return err
	}
	m.setError(rotErr)
	return nil
}

// usageFraction reports the current session's token usage as a fraction of
// its model's context limit, from the latest completed assistant message
// (Session.Info's own tokens field was observed unpopulated live) matched
// against ListModels. Returns 0 if no assistant message has completed yet.
func (m *Manager) usageFraction(ctx context.Context, sessionID string) (float64, error) {
	asst, ok, err := m.client.LatestAssistantMessage(ctx, sessionID)
	if err != nil || !ok {
		return 0, err
	}
	m.mu.Lock()
	m.observed = asst.ProviderID + "/" + asst.ModelID
	m.mu.Unlock()
	models, err := m.client.ListModels(ctx)
	if err != nil {
		return 0, err
	}
	for _, mdl := range models {
		if mdl.ID == asst.ModelID && mdl.ProviderID == asst.ProviderID && mdl.Limit.Context > 0 {
			t := asst.Tokens
			return float64(t.Input+t.Output+t.Reasoning+t.Cache.Read+t.Cache.Write) / float64(mdl.Limit.Context), nil
		}
	}
	return 0, fmt.Errorf("model %s/%s is not in the server's model list", asst.ProviderID, asst.ModelID)
}

// rotateIfNeeded retires the current session into history and starts a new
// one, seeded with a handoff summary, once usage has crossed threshold. The
// new session is created first and state is only swapped once that
// succeeds, so a failure never leaves the old session half-retired.
func (m *Manager) rotateIfNeeded(ctx context.Context) error {
	old := m.Current()
	fraction, err := m.usageFraction(ctx, old.SessionID)
	if err != nil {
		return fmt.Errorf("checking context usage: %w", err)
	}
	if fraction < m.threshold {
		return nil
	}

	rec, err := m.create(ctx)
	if err != nil {
		return fmt.Errorf("rotating Senapati: %w", err)
	}
	now := time.Now().UTC()
	old.RotatedAt = &now
	m.mu.Lock()
	m.history = append(m.history, old)
	m.current, m.seq = rec, m.seq+1
	saveErr := state.Save(m.statePath, m.stateLocked())
	m.mu.Unlock()

	var sainiks []attention.Instance
	if m.registry != nil {
		m.registry.Protect(rec.SessionID)
		sainiks = m.registry.List()
	}
	// The new session is briefed in the same message that hands over from
	// the old one. If the briefing can't be built the handoff still goes out
	// and the problem is reported; the session simply stays un-briefed.
	text := HandoffSummary(sainiks, old.SessionID)
	first, briefErr := m.install(ctx, rec.SessionID)
	briefed := briefErr == nil && first != ""
	if briefed {
		text = first + "\n\n" + text
	}
	if err := m.sendMessage(ctx, rec.SessionID, text); err != nil {
		return fmt.Errorf("sending handoff to %s: %w", rec.Title, err)
	}
	if briefed {
		if err := m.markBriefed(); err != nil {
			saveErr = err
		}
	}
	if briefErr != nil {
		return briefErr
	}
	if saveErr != nil {
		return fmt.Errorf("saving %s: %w", m.statePath, saveErr)
	}
	return nil
}
