// Package claudecode runs agent sessions on Claude Code, the way an opencode
// server runs them for the opencode harness. Claude Code has no server: a
// session is a transcript on disk, and mahamantri gives it a process only
// while it has work - `claude -p --input-format stream-json --output-format
// stream-json`, resumed with --resume - and lets the process exit once the
// session has been idle a while.
//
// Verified live against Claude Code 2.1.29x:
//   - a user message written to stdin mid-turn is folded into the running
//     turn ("steer"); "queue" (after the turn) is therefore done here, by
//     holding the message until the turn's result arrives;
//   - {"type":"control_request","request":{"subtype":"interrupt"}} stops the
//     turn - the running tool call included - and the session continues;
//   - every turn reports subscription usage (rate_limit_event) and ends with a
//     result event carrying the reply, usage and is_error; a usage limit is an
//     assistant message with "error":"rate_limit" and text such as
//     "You've hit your session limit · resets 2pm (UTC)";
//   - `claude agents --json` lists every Claude session on the machine with
//     its pid - ours included - which is how a person opening a session with
//     `claude --resume` is noticed.
//
// Events are reported in the same shape the opencode harness produces
// (opencode.Event with session.execution.* types), so the registry, notices
// and TUI work unchanged.
package claudecode

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

// ErrNotFound is opencode.ErrSessionNotFound, so callers handle a missing
// session the same way on either harness.
var ErrNotFound = opencode.ErrSessionNotFound

// ContinueAfterLimit is sent to a session when the usage limit that stopped
// it has reset. The interrupted request is already in its transcript.
const ContinueAfterLimit = "Your previous turn was cut off by a usage limit, which has now reset. Continue where you left off."

// Options configures the harness.
type Options struct {
	Binary    string        // path to claude; default "claude"
	StateFile string        // where sessions are remembered across restarts
	IdleExit  time.Duration // a session's process exits after this long idle; default 10m
	PollEvery time.Duration // how often takeover is checked; default 5s
	Logger    *log.Logger
}

// Spec is how a session is run. Args carries the permission and tool flags
// (see senapati.ClaudeSenapatiArgs / ClaudeSainikArgs).
type Spec struct {
	Title string   `json:"title"`
	Dir   string   `json:"dir"`
	Model string   `json:"model,omitempty"`
	Agent string   `json:"agent,omitempty"`
	Role  string   `json:"role,omitempty"` // --append-system-prompt
	Args  []string `json:"args,omitempty"`
	// FallbackModel, when set, is switched to - and the session continued at
	// once - when the session hits a usage limit on its current model.
	FallbackModel string `json:"fallbackModel,omitempty"`
}

// Info is a session as mahamantri sees it.
type Info struct {
	ID           string     `json:"id"`
	Title        string     `json:"title"`
	Dir          string     `json:"dir"`
	Model        string     `json:"model,omitempty"`
	State        string     `json:"state"` // running | idle | manual | limited
	LastReply    string     `json:"lastReply,omitempty"`
	LastError    string     `json:"lastError,omitempty"`
	LimitedUntil *time.Time `json:"limitedUntil,omitempty"`
	Queued       int        `json:"queued"`
}

// Limit is the latest usage report for one provider's subscription.
type Limit struct {
	Provider string            `json:"provider"`
	Status   string            `json:"status"` // allowed | ... ; anything else means limited
	Windows  map[string]Window `json:"windows,omitempty"`
	ResetsAt *time.Time        `json:"resetsAt,omitempty"`
	Seen     time.Time         `json:"seen"`
}

// Window is one usage window ("five_hour", "seven_day").
type Window struct {
	Utilization float64   `json:"utilization"`
	ResetsAt    time.Time `json:"resetsAt"`
}

type session struct {
	ID           string     `json:"id"`
	Spec         Spec       `json:"spec"`
	Started      bool       `json:"started"` // has a transcript: resume it
	LastReply    string     `json:"lastReply,omitempty"`
	LastError    string     `json:"lastError,omitempty"`
	LimitedUntil *time.Time `json:"limitedUntil,omitempty"`
	BusyAtExit   bool       `json:"busyAtExit,omitempty"`
	UsedTokens   int        `json:"usedTokens,omitempty"`
	Context      int        `json:"context,omitempty"`
	UsedModel    string     `json:"usedModel,omitempty"`

	proc         *proc
	pending      []string
	busy         bool
	manual       bool
	manualPID    int
	interrupting chan struct{}
	errCode      string // "error" of the turn's API error message, if any
	lastActive   time.Time
	limitTimer   *time.Timer
}

func (s *session) state() string {
	switch {
	case s.manual:
		return "manual"
	case s.busy:
		return "running"
	case s.LimitedUntil != nil:
		return "limited"
	}
	return "idle"
}

type proc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	mu     sync.Mutex // serialises writes to stdin
	done   chan struct{}
	stderr *tail
}

func (p *proc) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err = p.stdin.Write(append(data, '\n'))
	return err
}

// Harness runs Claude Code sessions and reports their events.
type Harness struct {
	opt      Options
	out      chan<- opencode.Event
	queue    chan opencode.Event
	mu       sync.Mutex
	sessions map[string]*session
	limits   map[string]Limit
	now      func() time.Time
	seq      int
	// ours is every pid this harness started: `claude agents` can still list
	// one of them briefly after it exits, and that is not a person.
	ours map[int]bool
}

// New loads the state file and starts forwarding events to out. Call Run to
// start takeover detection and idle exit.
func New(opt Options, out chan<- opencode.Event) (*Harness, error) {
	if opt.Binary == "" {
		opt.Binary = "claude"
	}
	if opt.IdleExit <= 0 {
		opt.IdleExit = 10 * time.Minute
	}
	if opt.PollEvery <= 0 {
		opt.PollEvery = 5 * time.Second
	}
	if opt.Logger == nil {
		opt.Logger = log.New(io.Discard, "", 0)
	}
	h := &Harness{opt: opt, out: out, queue: make(chan opencode.Event, 4096),
		sessions: map[string]*session{}, limits: map[string]Limit{}, now: time.Now, ours: map[int]bool{}}
	if err := h.load(); err != nil {
		return nil, err
	}
	go func() {
		for ev := range h.queue {
			out <- ev
		}
	}()
	return h, nil
}

// --- persistence ---

type stateFile struct {
	Sessions []*session `json:"sessions"`
}

func (h *Harness) load() error {
	if h.opt.StateFile == "" {
		return nil
	}
	data, err := os.ReadFile(h.opt.StateFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f stateFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("%s: %w", h.opt.StateFile, err)
	}
	for _, s := range f.Sessions {
		h.sessions[s.ID] = s
	}
	return nil
}

func (h *Harness) saveLocked() {
	if h.opt.StateFile == "" {
		return
	}
	f := stateFile{}
	for _, s := range h.sessions {
		f.Sessions = append(f.Sessions, s)
	}
	sort.Slice(f.Sessions, func(i, j int) bool { return f.Sessions[i].ID < f.Sessions[j].ID })
	data, _ := json.MarshalIndent(f, "", "  ")
	tmp := h.opt.StateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		h.opt.Logger.Printf("claude: saving %s: %v", h.opt.StateFile, err)
		return
	}
	os.Rename(tmp, h.opt.StateFile)
}

// --- events ---

func (h *Harness) emitLocked(id, typ string, sev opencode.Severity, summary string, raw map[string]any) {
	h.seq++
	data, _ := json.Marshal(raw)
	ev := opencode.Event{ID: fmt.Sprintf("claude-%d", h.seq), Type: typ, SessionID: id, Severity: sev, Summary: summary, Raw: data}
	select {
	case h.queue <- ev:
	default:
		h.opt.Logger.Printf("claude: event queue full, dropped %s for %s", typ, id)
	}
}

// --- session management ---

// Create registers a new session. It gets a process with its first prompt.
func (h *Harness) Create(spec Spec) (string, error) {
	if spec.Dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		spec.Dir = wd
	}
	if st, err := os.Stat(spec.Dir); err != nil || !st.IsDir() {
		return "", fmt.Errorf("%w: directory %q does not exist", opencode.ErrBadRequest, spec.Dir)
	}
	id := newUUID()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions[id] = &session{ID: id, Spec: spec}
	h.saveLocked()
	return id, nil
}

// Has reports whether id is a session this harness runs.
func (h *Harness) Has(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.sessions[id]
	return ok
}

// Info describes a session.
func (h *Harness) Info(id string) (Info, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[id]
	if !ok {
		return Info{}, ErrNotFound
	}
	return Info{ID: s.ID, Title: s.Spec.Title, Dir: s.Spec.Dir, Model: s.Spec.Model, State: s.state(),
		LastReply: s.LastReply, LastError: s.LastError, LimitedUntil: s.LimitedUntil, Queued: len(s.pending)}, nil
}

// Sessions describes every session, ordered by title.
func (h *Harness) Sessions() []Info {
	h.mu.Lock()
	ids := make([]string, 0, len(h.sessions))
	for id := range h.sessions {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	var out []Info
	for _, id := range ids {
		if info, err := h.Info(id); err == nil {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out
}

// LastReply is the session's latest reply - what it said at the end of its
// last turn.
func (h *Harness) LastReply(_ context.Context, id string) (string, error) {
	info, err := h.Info(id)
	return info.LastReply, err
}

// ActiveSessions lists the sessions mid-turn right now.
func (h *Harness) ActiveSessions(context.Context) (map[string]bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]bool{}
	for id, s := range h.sessions {
		if s.busy {
			out[id] = true
		}
	}
	return out, nil
}

// Usage is the context the session's latest turn used, against its model's
// context window.
func (h *Harness) Usage(id string) (tokens, window int, model string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, found := h.sessions[id]
	if !found || s.UsedTokens == 0 {
		return 0, 0, "", false
	}
	return s.UsedTokens, s.Context, s.UsedModel, true
}

// Limits is the latest usage report per provider.
func (h *Harness) Limits() []Limit {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Limit
	for _, l := range h.limits {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

// SetModel switches the session to model from its next turn on; a running
// process is restarted for it once the current turn ends.
func (h *Harness) SetModel(id, model string) error {
	return h.update(id, func(s *session) bool {
		if s.Spec.Model == model {
			return false
		}
		s.Spec.Model = model
		return true
	})
}

// SetAgent sets the session's --agent from its next process on.
func (h *Harness) SetAgent(id, agent string) error {
	return h.update(id, func(s *session) bool {
		changed := s.Spec.Agent != agent
		s.Spec.Agent = agent
		return changed
	})
}

// SetRole sets the session's appended system prompt from its next process on.
func (h *Harness) SetRole(id, role string) error {
	return h.update(id, func(s *session) bool {
		changed := s.Spec.Role != role
		s.Spec.Role = role
		return changed
	})
}

// SetArgs sets the session's permission and tool flags from its next
// process on.
func (h *Harness) SetArgs(id string, args []string) error {
	return h.update(id, func(s *session) bool {
		changed := strings.Join(s.Spec.Args, "\x00") != strings.Join(args, "\x00")
		s.Spec.Args = append([]string(nil), args...)
		return changed
	})
}

// update applies change; if it changed how the process must be started, an
// idle process is stopped so the next turn starts a fresh one.
func (h *Harness) update(id string, change func(*session) bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if change(s) && s.proc != nil && !s.busy {
		h.closeLocked(s)
	}
	h.saveLocked()
	return nil
}

// Delete stops the session and forgets it. Its transcript stays on disk.
func (h *Harness) Delete(id string) error {
	h.mu.Lock()
	s, ok := h.sessions[id]
	if !ok {
		h.mu.Unlock()
		return ErrNotFound
	}
	busy := s.busy
	h.mu.Unlock()
	if busy {
		h.Interrupt(id)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.proc != nil {
		h.closeLocked(s)
	}
	if s.limitTimer != nil {
		s.limitTimer.Stop()
	}
	delete(h.sessions, id)
	h.saveLocked()
	return nil
}

// --- turns ---

// Prompt sends text to the session. delivery "queue" (the default) delivers
// it after the current turn - at once if the session is idle; "steer" puts it
// into the running turn. A session a person is using, or one paused by a
// usage limit, holds it until it is free again.
func (h *Harness) Prompt(id, text, delivery string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[id]
	if !ok {
		return ErrNotFound
	}
	switch {
	case s.manual || s.LimitedUntil != nil:
		s.pending = append(s.pending, text)
		return nil
	case s.busy && delivery == "steer":
		if err := s.proc.send(userMessage(text)); err == nil {
			return nil
		}
		s.pending = append(s.pending, text) // the process went away; run it as the next turn
		return nil
	case s.busy:
		s.pending = append(s.pending, text)
		return nil
	}
	return h.startTurnLocked(s, text)
}

func userMessage(text string) map[string]any {
	return map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}}
}

func (h *Harness) startTurnLocked(s *session, text string) error {
	for attempt := 0; attempt < 2; attempt++ {
		if s.proc == nil {
			if err := h.spawnLocked(s); err != nil {
				s.LastError = err.Error()
				h.emitLocked(s.ID, "session.execution.failed", opencode.Warning, "could not start claude: "+err.Error(),
					map[string]any{"error": err.Error()})
				return err
			}
		}
		if err := s.proc.send(userMessage(text)); err != nil {
			h.closeLocked(s)
			continue
		}
		s.busy = true
		s.lastActive = h.now()
		s.errCode = ""
		h.emitLocked(s.ID, "session.execution.started", opencode.Info, "session.execution.started", nil)
		h.saveLocked()
		return nil
	}
	return fmt.Errorf("claude process for %s would not take input", s.ID)
}

func (h *Harness) spawnLocked(s *session) error {
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}
	if s.Spec.Model != "" {
		args = append(args, "--model", s.Spec.Model)
	}
	if s.Started {
		args = append(args, "--resume", s.ID)
	} else {
		args = append(args, "--session-id", s.ID)
	}
	if s.Spec.Title != "" {
		args = append(args, "-n", s.Spec.Title)
	}
	if s.Spec.Agent != "" {
		args = append(args, "--agent", s.Spec.Agent)
	}
	if s.Spec.Role != "" {
		// Render the role fresh on every request, so an edited instructions
		// file reaches a resumed session.
		args = append(args, "--append-system-prompt", s.Spec.Role, "--system-prompt-snapshot", "off")
	}
	args = append(args, s.Spec.Args...)

	cmd := exec.Command(h.opt.Binary, args...)
	cmd.Dir = s.Spec.Dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	p := &proc{cmd: cmd, stdin: stdin, done: make(chan struct{}), stderr: newTail(4096)}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	s.proc = p
	s.lastActive = h.now()
	h.ours[cmd.Process.Pid] = true
	h.opt.Logger.Printf("claude: started pid %d for %s (%s)", cmd.Process.Pid, s.ID, s.Spec.Title)
	go h.read(s, p, stdout)
	return nil
}

func (h *Harness) read(s *session, p *proc, stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		h.handleLine(s, p, sc.Bytes())
	}
	err := p.cmd.Wait()
	close(p.done)
	h.onExit(s, p, err)
}

type tokenUsage struct {
	Input       int `json:"input_tokens"`
	Output      int `json:"output_tokens"`
	CacheRead   int `json:"cache_read_input_tokens"`
	CacheCreate int `json:"cache_creation_input_tokens"`
}

func (u tokenUsage) total() int { return u.Input + u.Output + u.CacheRead + u.CacheCreate }

type streamLine struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Error     string          `json:"error"`
	IsError   bool            `json:"is_error"`
	Result    string          `json:"result"`
	Message   json.RawMessage `json:"message"`
	Usage     struct {
		tokenUsage
		// Iterations is the usage of the turn's last API call; the totals
		// above are summed over every call in the turn (verified live).
		Iterations []tokenUsage `json:"iterations"`
	} `json:"usage"`
	ModelUsage map[string]struct {
		ContextWindow int `json:"contextWindow"`
	} `json:"modelUsage"`
	RateLimit *struct {
		Status         string `json:"status"`
		ResetsAt       int64  `json:"resetsAt"`
		UnifiedWindows map[string]struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    int64   `json:"resetsAt"`
		} `json:"unifiedWindows"`
	} `json:"rate_limit_info"`
}

func (h *Harness) handleLine(s *session, p *proc, line []byte) {
	var l streamLine
	if json.Unmarshal(line, &l) != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.proc != p {
		return // a process we already let go of
	}
	s.lastActive = h.now()
	switch l.Type {
	case "user", "assistant":
		if !s.Started {
			s.Started = true
			h.saveLocked()
		}
		if l.Type == "assistant" && l.Error != "" {
			s.errCode = l.Error
		}
	case "rate_limit_event":
		if l.RateLimit != nil {
			h.recordLimitLocked(provider(s.Spec.Model), l)
		}
	case "result":
		if !s.Started {
			s.Started = true
		}
		h.onResultLocked(s, l)
	}
}

func (h *Harness) recordLimitLocked(prov string, l streamLine) {
	lim := Limit{Provider: prov, Status: l.RateLimit.Status, Seen: h.now()}
	if l.RateLimit.ResetsAt > 0 {
		t := time.Unix(l.RateLimit.ResetsAt, 0).UTC()
		lim.ResetsAt = &t
	}
	if len(l.RateLimit.UnifiedWindows) > 0 {
		lim.Windows = map[string]Window{}
		for name, w := range l.RateLimit.UnifiedWindows {
			lim.Windows[name] = Window{Utilization: w.Utilization, ResetsAt: time.Unix(w.ResetsAt, 0).UTC()}
		}
	} else if old, ok := h.limits[prov]; ok {
		lim.Windows = old.Windows // providers that report none keep the last known
	}
	h.limits[prov] = lim
}

func (h *Harness) onResultLocked(s *session, l streamLine) {
	// The context in use is what the turn's last API call sent and got back.
	// The result's own totals are summed over every call in the turn - a
	// turn of ten tool calls would read as ten times the context, and
	// rotated Senapati after a couple of messages.
	last := l.Usage.tokenUsage
	if n := len(l.Usage.Iterations); n > 0 {
		last = l.Usage.Iterations[n-1]
	}
	if tokens := last.total(); tokens > 0 {
		s.UsedTokens = tokens
		for model, mu := range l.ModelUsage {
			if mu.ContextWindow > 0 && (s.Context == 0 || model == s.Spec.Model || len(l.ModelUsage) == 1) {
				s.Context, s.UsedModel = mu.ContextWindow, model
			}
		}
	}
	errCode := s.errCode
	s.errCode = ""

	if ch := s.interrupting; ch != nil {
		// Our own interrupt: no event - the caller is about to say what next.
		s.interrupting = nil
		s.busy = false
		close(ch)
		h.afterTurnLocked(s)
		return
	}

	if l.IsError && errCode == "rate_limit" {
		h.onLimitLocked(s, l.Result)
		return
	}
	if l.IsError {
		s.LastError = l.Result
		s.LastReply = l.Result
		s.busy = false
		h.emitLocked(s.ID, "session.execution.failed", opencode.Warning, "turn failed: "+truncate(l.Result, 200),
			map[string]any{"error": errCode, "reply": l.Result})
		h.afterTurnLocked(s)
		return
	}

	s.LastReply = l.Result
	s.LastError = ""
	if len(s.pending) > 0 {
		// Queued work runs as the next turn of the same execution: one
		// completion notice covers it all, as on opencode.
		next := s.pending[0]
		s.pending = s.pending[1:]
		if err := s.proc.send(userMessage(next)); err == nil {
			h.saveLocked()
			return
		}
		s.pending = append([]string{next}, s.pending...)
	}
	s.busy = false
	h.emitLocked(s.ID, "session.execution.succeeded", opencode.Info, "session.execution.succeeded",
		map[string]any{"reply": l.Result})
	h.afterTurnLocked(s)
}

// afterTurnLocked restarts the process if its flags changed mid-turn and
// starts held work if there is any.
func (h *Harness) afterTurnLocked(s *session) {
	h.saveLocked()
	if s.manual || s.LimitedUntil != nil || len(s.pending) == 0 {
		return
	}
	next := s.pending[0]
	s.pending = s.pending[1:]
	h.startTurnLocked(s, next)
}

var resetsAt = regexp.MustCompile(`resets (\d{1,2})(?::(\d{2}))?\s*(am|pm)\s*\(UTC\)`)

// onLimitLocked handles a turn stopped by a usage limit: switch to the
// fallback model and continue at once, or pause until the limit resets and
// continue then.
func (h *Harness) onLimitLocked(s *session, text string) {
	s.busy = false
	s.LastError = text
	s.LastReply = text
	if fb := s.Spec.FallbackModel; fb != "" && fb != s.Spec.Model && provider(fb) != provider(s.Spec.Model) {
		was := s.Spec.Model
		s.Spec.Model = fb
		h.closeLocked(s)
		h.opt.Logger.Printf("claude: %s hit a usage limit on %s; switched to %s", s.ID, was, fb)
		h.emitLocked(s.ID, "session.model.fallback", opencode.Info,
			fmt.Sprintf("usage limit on %s (%s) - switched to %s and continued", was, text, fb),
			map[string]any{"from": was, "to": fb, "reply": text})
		s.pending = append([]string{ContinueAfterLimit}, s.pending...)
		h.afterTurnLocked(s)
		return
	}
	until := h.resetTimeLocked(provider(s.Spec.Model), text)
	s.LimitedUntil = &until
	h.armLimitTimerLocked(s)
	h.emitLocked(s.ID, "session.execution.failed", opencode.Warning,
		fmt.Sprintf("usage limit: %s - paused until %s", text, until.Format("15:04 MST")),
		map[string]any{"error": "rate_limit", "limitedUntil": until, "reply": text})
	h.saveLocked()
}

func (h *Harness) resetTimeLocked(prov, text string) time.Time {
	now := h.now().UTC()
	if l, ok := h.limits[prov]; ok && l.Status != "allowed" && l.ResetsAt != nil && l.ResetsAt.After(now) {
		return *l.ResetsAt
	}
	if m := resetsAt.FindStringSubmatch(text); m != nil {
		hour, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		hour %= 12
		if m[3] == "pm" {
			hour += 12
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, time.UTC)
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		return t
	}
	return now.Add(time.Hour)
}

func (h *Harness) armLimitTimerLocked(s *session) {
	if s.LimitedUntil == nil {
		return
	}
	if s.limitTimer != nil {
		s.limitTimer.Stop()
	}
	wait := s.LimitedUntil.Sub(h.now()) + 30*time.Second
	id := s.ID
	s.limitTimer = time.AfterFunc(wait, func() { h.ResumeLimited(id) })
}

// ResumeLimited ends a usage-limit pause: the session continues where it
// was cut off, then runs whatever was held for it.
func (h *Harness) ResumeLimited(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[id]
	if !ok || s.LimitedUntil == nil {
		return
	}
	s.LimitedUntil = nil
	s.pending = append([]string{ContinueAfterLimit}, s.pending...)
	h.afterTurnLocked(s)
}

// Interrupt stops the session's turn now - a running tool call included -
// keeping its context. It returns once the turn has ended.
func (h *Harness) Interrupt(id string) error {
	h.mu.Lock()
	s, ok := h.sessions[id]
	if !ok {
		h.mu.Unlock()
		return ErrNotFound
	}
	if !s.busy || s.proc == nil {
		h.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	s.interrupting = ch
	p := s.proc
	h.seq++
	reqID := fmt.Sprintf("interrupt-%d", h.seq)
	h.mu.Unlock()

	if err := p.send(map[string]any{"type": "control_request", "request_id": reqID,
		"request": map[string]any{"subtype": "interrupt"}}); err != nil {
		h.opt.Logger.Printf("claude: interrupt request to %s failed: %v", id, err)
	}
	select {
	case <-ch:
		return nil
	case <-p.done:
		return nil
	case <-time.After(10 * time.Second):
	}
	// The request was not honoured: stop the process, and anything it
	// started that outlives it.
	h.opt.Logger.Printf("claude: %s did not stop on request; sending SIGINT", id)
	kids := descendants(p.cmd.Process.Pid)
	p.cmd.Process.Signal(syscall.SIGINT)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		p.cmd.Process.Kill()
		<-p.done
	}
	for _, pid := range kids {
		syscall.Kill(pid, syscall.SIGTERM)
	}
	return nil
}

func (h *Harness) onExit(s *session, p *proc, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.proc != p {
		return
	}
	s.proc = nil
	if ch := s.interrupting; ch != nil {
		s.interrupting = nil
		s.busy = false
		close(ch)
		h.afterTurnLocked(s)
		return
	}
	if !s.busy {
		return // exited after its turn: idle exit, or a restart we asked for
	}
	s.busy = false
	msg := strings.TrimSpace(p.stderr.String())
	if strings.Contains(msg, "No conversation found") {
		s.Started = false // never got a transcript; the next process starts it
	}
	if msg == "" && err != nil {
		msg = err.Error()
	}
	s.LastError = "claude exited mid-turn: " + truncate(msg, 500)
	h.emitLocked(s.ID, "session.execution.failed", opencode.Warning, s.LastError, map[string]any{"error": "exited", "reply": s.LastError})
	h.afterTurnLocked(s)
}

// closeLocked lets go of the session's process: its stdin is closed, so an
// idle claude exits at once. Late output from it is ignored.
func (h *Harness) closeLocked(s *session) {
	p := s.proc
	if p == nil {
		return
	}
	s.proc = nil
	p.stdin.Close()
	go func() {
		select {
		case <-p.done:
		case <-time.After(15 * time.Second):
			p.cmd.Process.Kill()
		}
	}()
}

// --- background work ---

// Run notices people taking sessions over, ends idle processes, re-arms
// usage-limit timers and reports sessions a previous shutdown cut off.
func (h *Harness) Run(ctx context.Context) {
	h.mu.Lock()
	for _, s := range h.sessions {
		if s.BusyAtExit {
			s.BusyAtExit = false
			s.LastError = "stopped mid-turn when mahamantri shut down"
			h.emitLocked(s.ID, "session.execution.failed", opencode.Warning, s.LastError,
				map[string]any{"error": "shutdown", "reply": s.LastReply})
		}
		h.armLimitTimerLocked(s)
	}
	h.saveLocked()
	h.mu.Unlock()

	ticker := time.NewTicker(h.opt.PollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			h.Tick(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// Tick runs one round of takeover detection and idle exit.
func (h *Harness) Tick(ctx context.Context) {
	h.mu.Lock()
	n := len(h.sessions)
	h.mu.Unlock()
	if n > 0 {
		if open, err := h.openSessions(ctx); err != nil {
			h.opt.Logger.Printf("claude: listing sessions: %v", err)
		} else {
			h.reconcileManual(open)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sessions {
		if s.proc != nil && !s.busy && h.now().Sub(s.lastActive) > h.opt.IdleExit {
			h.opt.Logger.Printf("claude: %s idle for %s; letting its process exit", s.ID, h.opt.IdleExit)
			h.closeLocked(s)
		}
	}
}

// openSessions asks Claude Code which processes have which sessions open.
func (h *Harness) openSessions(ctx context.Context) (map[string][]int, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, h.opt.Binary, "agents", "--json").Output()
	if err != nil {
		return nil, err
	}
	var list []struct {
		SessionID string `json:"sessionId"`
		PID       int    `json:"pid"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	open := map[string][]int{}
	for _, a := range list {
		if a.PID > 0 {
			open[a.SessionID] = append(open[a.SessionID], a.PID)
		}
	}
	return open, nil
}

// reconcileManual marks sessions open in a process that is not ours as
// manual, and hands them back once that process is gone.
func (h *Harness) reconcileManual(open map[string][]int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, s := range h.sessions {
		other := 0
		for _, pid := range open[id] {
			if !h.ours[pid] {
				other = pid
			}
		}
		switch {
		case other != 0 && !s.manual:
			s.manual, s.manualPID = true, other
			if s.proc != nil && !s.busy {
				h.closeLocked(s) // one writer per session: theirs
			}
			h.emitLocked(id, "session.manual.started", opencode.Info,
				fmt.Sprintf("pid %d", other), map[string]any{"pid": other})
		case other == 0 && s.manual:
			s.manual, s.manualPID = false, 0
			s.Started = true // whatever they did is in its transcript now
			// Their last turn ran in their own process: its reply is only in
			// the transcript.
			if reply := h.transcriptReply(id); reply != "" {
				s.LastReply = reply
			}
			h.emitLocked(id, "session.manual.ended", opencode.Info, "the person closed it", map[string]any{"reply": s.LastReply})
			h.afterTurnLocked(s)
		}
	}
}

// TranscriptRoot is where Claude Code keeps session transcripts
// (<root>/<project>/<session id>.jsonl). Empty means ~/.claude/projects.
var TranscriptRoot = ""

// transcriptReply is the text of the newest assistant message in the
// session's transcript on disk, or "".
func (h *Harness) transcriptReply(id string) string {
	root := TranscriptRoot
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		root = filepath.Join(home, ".claude", "projects")
	}
	matches, _ := filepath.Glob(filepath.Join(root, "*", id+".jsonl"))
	if len(matches) == 0 {
		return ""
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var e struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(lines[i]), &e) != nil || e.Type != "assistant" {
			continue
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		json.Unmarshal(e.Message.Content, &parts)
		var texts []string
		for _, p := range parts {
			if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
				texts = append(texts, strings.TrimSpace(p.Text))
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n\n")
		}
	}
	return ""
}

// Shutdown interrupts running turns and lets every process go; sessions it
// cut off are reported as such on the next start.
func (h *Harness) Shutdown() {
	h.mu.Lock()
	var busy []string
	for id, s := range h.sessions {
		if s.busy {
			s.BusyAtExit = true
			busy = append(busy, id)
		}
	}
	h.saveLocked()
	h.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range busy {
		wg.Add(1)
		go func(id string) { defer wg.Done(); h.Interrupt(id) }(id)
	}
	wg.Wait()
	h.mu.Lock()
	for _, s := range h.sessions {
		if s.limitTimer != nil {
			s.limitTimer.Stop()
		}
		h.closeLocked(s)
	}
	h.saveLocked()
	h.mu.Unlock()
}

// --- helpers ---

// provider names whose subscription a model draws on.
func provider(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "gpt") || strings.Contains(m, "codex") || strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4"):
		return "openai"
	case m == "" || strings.HasPrefix(m, "claude") || m == "opus" || m == "sonnet" || m == "haiku" || m == "fable":
		return "anthropic"
	}
	if i := strings.IndexAny(m, "-/"); i > 0 {
		return m[:i]
	}
	return m
}

// descendants lists every process below pid.
func descendants(pid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	children := map[int][]int{}
	for _, e := range entries {
		child, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// pid (comm) state ppid ... - comm may contain spaces and parens.
		s := string(data)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		fields := strings.Fields(s[i+1:])
		if len(fields) < 2 {
			continue
		}
		parent, _ := strconv.Atoi(fields[1])
		children[parent] = append(children[parent], child)
	}
	var out []int
	var walk func(int)
	walk = func(p int) {
		for _, c := range children[p] {
			out = append(out, c)
			walk(c)
		}
	}
	walk(pid)
	return out
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// tail keeps the last n bytes written to it.
type tail struct {
	mu  sync.Mutex
	n   int
	buf []byte
}

func newTail(n int) *tail { return &tail{n: n} }

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.n {
		t.buf = t.buf[len(t.buf)-t.n:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
