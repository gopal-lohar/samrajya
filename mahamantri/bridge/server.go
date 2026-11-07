package bridge

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/brain"
)

const (
	maxRequest = 64 << 10
	// A channel connection that has not asked for events this long is gone;
	// another may attach without taking over.
	attachTTL = 90 * time.Second
	maxWait   = 30 * time.Second
)

// Server is the local API between mahamantri and the Samrajya channel plugin
// running in each agent's session. Every request carries the bearer token
// mahamantri wrote to its discovery file. A session attaches as its agent
// with the secret of the agent's current launch; a session that cannot is
// read-only, and a launch that was replaced can no longer act.
//
// Server is also where surfaces deliver: Receive stores a message and wakes
// the feeds.
type Server struct {
	Store  *Store
	Token  string
	Logger *log.Logger
	// Brain, if set, is served under /brain (see brain.Routes): any client
	// may read it, only an attached agent change it.
	Brain *brain.Store
	// Surfaces are where messages come from and replies go, by name (see
	// AddSurface).
	Surfaces map[string]Surface
	// Home is the conversation where Samrajya tells the person about
	// itself (a stuck main session); empty: only the log.
	Home string
	// FilesDir is where files fetched from surfaces for agents are saved.
	FilesDir string

	// Supervisor stops agents for the main (samrajya_stop_agent) and says
	// how long the queue for a free slot is. Nil when mahamantri launches
	// no sessions (mode opencode).
	Supervisor interface {
		StopAgent(ctx context.Context, id string) error
		Queued(agent string) (ahead int, queued bool)
	}

	mu       sync.Mutex
	atts     map[string]*attachment // by agent id
	seq      uint64                 // attachments made, for their ids
	wake     chan struct{}          // closed and replaced whenever an event is stored
	now      func() time.Time
	brainMux *http.ServeMux
}

// attachment is one plugin instance attached as an agent.
type attachment struct {
	agent    string
	holder   string // the attachment id the plugin sends back
	secret   string // the agent secret it attached with (agents mode)
	client   string
	gen      uint64
	lastSeen time.Time
	protocol int
}

func (a *attachment) live(now time.Time) bool {
	return a != nil && now.Sub(a.lastSeen) < attachTTL
}

func (s *Server) init() {
	if s.wake == nil {
		s.wake = make(chan struct{})
	}
	if s.atts == nil {
		s.atts = map[string]*attachment{}
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.Brain != nil && s.brainMux == nil {
		s.brainMux = http.NewServeMux()
		brain.Routes(s.brainMux, s.Brain)
	}
}

// Receive stores a message from a surface and wakes the feeds. A new
// message is marked received on its surface, so its author sees at once
// that it arrived even while nobody can answer; that is best effort and
// never fails the receipt.
func (s *Server) Receive(in Inbound) error {
	created, err := s.Store.Ingest(in)
	if err != nil {
		return err
	}
	s.Wake()
	if created {
		s.markReceived(in)
	}
	return nil
}

// Wake ends every waiting feed request, after an event is stored.
func (s *Server) Wake() {
	s.mu.Lock()
	s.init()
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
}

// AttachInfo describes who is attached as an agent, for status.
type AttachInfo struct {
	Agent      string    `json:"agent"`
	Client     string    `json:"client,omitempty"`
	Generation uint64    `json:"generation"`
	LastSeen   time.Time `json:"lastSeen,omitempty"`
	Live       bool      `json:"live"`
}

// attachmentOf describes agent's attachment; call with s.mu held.
func (s *Server) attachmentOf(agent string) AttachInfo {
	a := s.atts[agent]
	if a == nil {
		return AttachInfo{Agent: agent}
	}
	return AttachInfo{Agent: agent, Client: a.client, Generation: a.gen, LastSeen: a.lastSeen, Live: a.live(s.now())}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.init()
	s.mu.Unlock()
	if r.Header.Get("Origin") != "" {
		writeErr(w, http.StatusForbidden, "browser requests are not accepted")
		return
	}
	got := []byte(r.Header.Get("Authorization"))
	want := []byte("Bearer " + s.Token)
	if s.Token == "" || subtle.ConstantTimeCompare(got, want) != 1 {
		writeErr(w, http.StatusUnauthorized, "bad or missing token - reread the discovery file")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequest)

	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "/brain" || strings.HasPrefix(path, "/brain/") {
		switch {
		case s.brainMux == nil:
			writeErr(w, http.StatusNotFound, "no brain configured (brain: in mahamantri's config)")
		case r.Method == http.MethodGet:
			s.brainMux.ServeHTTP(w, r)
		default:
			s.withAgent(w, r, func(w http.ResponseWriter, r *http.Request, _ string) { s.brainMux.ServeHTTP(w, r) })
		}
		return
	}
	switch {
	case r.Method == http.MethodPost && path == "/v1/attach":
		s.attach(w, r)
	case r.Method == http.MethodPost && path == "/v1/detach":
		s.withAgent(w, r, s.detach)
	case r.Method == http.MethodGet && path == "/v1/status":
		s.status(w, r)
	case r.Method == http.MethodGet && path == "/v1/events":
		s.withAgent(w, r, s.events)
	case r.Method == http.MethodGet && path == "/v1/tasks":
		s.tasks(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/tasks/"):
		t, err := s.Store.Task(strings.TrimPrefix(path, "/v1/tasks/"))
		reply(w, t, err)
	case r.Method == http.MethodPost && path == "/v1/accept":
		s.withAgent(w, r, s.accept)
	case r.Method == http.MethodPost && path == "/v1/tasks":
		s.withAgent(w, r, mainOnly(s.createTask))
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/tasks/"):
		s.withAgent(w, r, func(w http.ResponseWriter, r *http.Request, agent string) {
			var u TaskUpdate
			if decode(w, r, &u) {
				t, err := s.Store.UpdateTaskAs(agent, strings.TrimPrefix(path, "/v1/tasks/"), u)
				reply(w, t, err)
			}
		})
	case r.Method == http.MethodPost && path == "/v1/limit":
		s.limit(w, r)
	case r.Method == http.MethodPost && path == "/v1/turn":
		s.turn(w, r)
	case r.Method == http.MethodPost && path == "/v1/reply":
		s.withAgent(w, r, s.reply)
	case r.Method == http.MethodPost && path == "/v1/fetch":
		s.withAgent(w, r, s.fetch)
	case r.Method == http.MethodGet && path == "/v1/agents":
		s.agents(w)
	case r.Method == http.MethodPost && path == "/v1/dispatch":
		s.withAgent(w, r, mainOnly(s.dispatch))
	case r.Method == http.MethodPost && path == "/v1/message":
		s.withAgent(w, r, s.message)
	case r.Method == http.MethodPost && path == "/v1/report":
		s.withAgent(w, r, s.report)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/agents/") && strings.HasSuffix(path, "/stop"):
		s.withAgent(w, r, mainOnly(func(w http.ResponseWriter, r *http.Request, _ string) {
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/agents/"), "/stop")
			if s.Supervisor == nil {
				writeErr(w, http.StatusNotImplemented, "mahamantri launches no sessions here (mode opencode)")
				return
			}
			reply(w, map[string]any{"stopped": id}, s.Supervisor.StopAgent(r.Context(), id))
		}))
	case r.Method == http.MethodPost && path == "/v1/actions":
		s.withAgent(w, r, func(w http.ResponseWriter, r *http.Request, agent string) {
			var a Action
			if decode(w, r, &a) {
				a, err := s.Store.IntendAs(agent, Action{TaskID: a.TaskID, Kind: a.Kind, Target: a.Target, Intent: a.Intent})
				reply(w, a, err)
			}
		})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/actions/"):
		s.withAgent(w, r, func(w http.ResponseWriter, r *http.Request, agent string) {
			var body struct {
				Status, Ref, Note, ThreadID, Issue string
			}
			if decode(w, r, &body) {
				a, err := s.Store.ResolveAs(agent, strings.TrimPrefix(path, "/v1/actions/"), body.Status, body.Ref, body.Note, body.ThreadID, body.Issue)
				reply(w, a, err)
			}
		})
	default:
		writeErr(w, http.StatusNotFound, "unknown endpoint")
	}
}

type agentHandler func(w http.ResponseWriter, r *http.Request, agent string)

func mainOnly(next agentHandler) agentHandler {
	return func(w http.ResponseWriter, r *http.Request, agent string) {
		if agent != MainAgent {
			writeErr(w, http.StatusForbidden, "only the main session may do this")
			return
		}
		next(w, r, agent)
	}
}

// ProtocolVersion is the plugin protocol this mahamantri serves.
const ProtocolVersion = 3

// attach attaches the caller as an agent. It names its agent and gives the
// secret of the agent's current launch; anything else is refused (and stays
// read-only). A launch's secret is replaced on the next launch, so a
// replaced session can no longer attach or act. The agent's earlier
// attachment, if any, is replaced: it is the same session reconnecting.
func (s *Server) attach(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Client   string `json:"client"`
		Agent    string `json:"agent"`
		Secret   string `json:"secret"`
		Protocol int    `json:"protocol"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Agent == "" || body.Secret == "" {
		writeErr(w, http.StatusConflict, "this session is not one of Samrajya's agents (no agent id), so it is read-only")
		return
	}
	if body.Protocol != ProtocolVersion {
		writeErr(w, http.StatusUpgradeRequired, fmt.Sprintf("plugin protocol %d; this mahamantri serves %d - update the plugin", body.Protocol, ProtocolVersion))
		return
	}
	a, err := s.Store.Agent(body.Agent)
	if err != nil || a.Secret == "" || subtle.ConstantTimeCompare([]byte(a.Secret), []byte(body.Secret)) != 1 {
		writeErr(w, http.StatusForbidden, "unknown agent or stale secret: this session has been replaced or stopped")
		return
	}
	agent, secret := a.ID, a.Secret
	s.mu.Lock()
	s.init()
	s.seq++
	att := &attachment{agent: agent, secret: secret, client: body.Client, gen: s.seq, lastSeen: s.now(), protocol: body.Protocol,
		holder: fmt.Sprintf("%s-%d-%d", agent, s.seq, s.now().UnixNano())}
	s.atts[agent] = att
	s.mu.Unlock()
	s.Store.UpdateAgent(agent, func(a *Agent) error {
		if a.Secret == secret {
			a.State, a.AttachedAt, a.Failures, a.Error = AgentLive, s.now(), 0, ""
		}
		return nil
	})
	s.Logger.Printf("bridge: %q attached as %s (attachment %d)", body.Client, agent, att.gen)
	writeJSON(w, http.StatusOK, map[string]any{"attachment": att.holder, "generation": att.gen, "agent": agent})
}

// detach releases the caller's attachment, so its successor attaches at
// once rather than after attachTTL. The plugin calls it when its session
// ends.
func (s *Server) detach(w http.ResponseWriter, r *http.Request, agent string) {
	s.mu.Lock()
	if a := s.atts[agent]; a != nil && a.holder == r.Header.Get("X-Samrajya-Attachment") {
		delete(s.atts, agent)
	}
	s.mu.Unlock()
	s.Logger.Printf("bridge: %s detached", agent)
	writeJSON(w, http.StatusOK, map[string]any{"detached": true})
}

// withAgent runs next for the agent the request's attachment belongs to,
// refusing a request without one, or one whose launch has since been
// replaced.
func (s *Server) withAgent(w http.ResponseWriter, r *http.Request, next agentHandler) {
	id := r.Header.Get("X-Samrajya-Attachment")
	s.mu.Lock()
	s.init()
	var att *attachment
	for _, a := range s.atts {
		if id != "" && a.holder == id {
			att = a
		}
	}
	if att != nil {
		att.lastSeen = s.now()
	}
	main := s.attachmentOf(MainAgent)
	s.mu.Unlock()
	if att != nil {
		if a, err := s.Store.Agent(att.agent); err != nil || a.Secret != att.secret {
			s.mu.Lock()
			if s.atts[att.agent] == att {
				delete(s.atts, att.agent)
			}
			s.mu.Unlock()
			att = nil
		}
	}
	if att == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "this session is not attached as one of Samrajya's agents", "attached": main})
		return
	}
	if r.Method == http.MethodPost && r.URL.Path != "/v1/detach" {
		// The session is calling tools, so turns run again. (A detach is
		// its session ending, which proves nothing.)
		s.clearLimitOnActivity()
	}
	next(w, r, att.agent)
}

// reply posts in a conversation through its surface, journaled: the action
// is recorded before posting and resolved with what was posted after, so
// the reply's thread is known at once and a crash in between leaves it
// uncertain rather than forgotten. The main may reply anywhere; a worker
// only in its task's conversations.
func (s *Server) reply(w http.ResponseWriter, r *http.Request, agent string) {
	var body struct {
		TaskID       string `json:"taskId"`
		Conversation string `json:"conversation"`
		ThreadID     string `json:"threadId"`
		Body         string `json:"body"`
	}
	if !decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Body) == "" {
		writeErr(w, http.StatusBadRequest, "body is required")
		return
	}
	conv, err := s.canonical(r.Context(), body.Conversation)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	conv, task, err := s.Store.MayReply(agent, conv)
	if err != nil {
		reply(w, nil, err)
		return
	}
	sf, err := s.surface(conv)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.TaskID == "" {
		body.TaskID = task.ID
	}
	if body.ThreadID == "" && agent != MainAgent && slices.Contains(task.Conversations, conv) {
		body.ThreadID = task.ThreadID
	}
	target := conv
	if body.ThreadID != "" {
		target += " thread " + body.ThreadID
	}
	a, err := s.Store.IntendAs(agent, Action{TaskID: body.TaskID, Kind: sf.Name() + "_reply", Target: target, Intent: truncate(body.Body, 200)})
	if err != nil {
		reply(w, nil, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	posted, err := sf.Deliver(ctx, conv, body.ThreadID, body.Body)
	if err != nil {
		// It may or may not have been posted (a timeout, say): leave the
		// action unresolved, to be checked before posting again.
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error() + " - check the conversation before posting again", "actionId": a.ID})
		return
	}
	if _, err := s.Store.Resolve(a.ID, Happened, posted.ID, "", posted.Thread, conv); err != nil {
		s.Logger.Printf("bridge: posted %s but could not record it: %v", posted.ID, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": posted.ID, "conversation": conv, "threadId": posted.Thread, "url": posted.URL, "actionId": a.ID})
}

// Status is the assistant's state as surfaces that can show one show it
// (StatusShower). It says something only when something is wrong: no main
// session, the usage limit, or a task waiting on the person. Otherwise it is
// empty.
type Status struct{ Emoji, Label string }

var offline = Status{"💤", "Offline - pings wait until I'm back"}

func (s *Server) currentStatus() Status {
	if !s.Live() {
		return offline
	}
	if until := s.limitedUntil(); !until.IsZero() {
		return Status{"⏳", "At my usage limit until " + untilText(until) + " - pings wait until then"}
	}
	if rec, err := s.Store.Recovery(); err == nil {
		for _, t := range rec.Open {
			if t.Status == Blocked || t.Status == WaitingApproval {
				if t.Title != "" {
					return Status{"⚠️", "Blocked on " + truncate(t.Title, 60) + " - needs you"}
				}
				return Status{"⚠️", "Blocked - needs you"}
			}
		}
	}
	return Status{}
}

// Live reports whether a main session is attached and has been heard from.
func (s *Server) Live() bool { return s.AgentLive(MainAgent) }

// AgentLive reports whether agent is attached and has been heard from.
func (s *Server) AgentLive(agent string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	return s.atts[agent].live(s.now())
}

// Watch runs until ctx ends, checking every interval: it wakes the main
// session once its usage limit has reset, shows the current Status on every
// surface that can show one, and tells the author of a message still
// unaccepted for its surface's NudgeAfter why it waits - on the message's own
// surface, once per thread and reason - so nothing sits silently while a
// session is offline, busy or at the usage limit. What fails is retried at
// the next check. On the way out it shows offline.
func (s *Server) Watch(ctx context.Context, every time.Duration) {
	shown := map[string]Status{}
	set := func(ctx context.Context, st Status) {
		for name, sf := range s.Surfaces {
			sh, ok := sf.(StatusShower)
			if !ok {
				continue
			}
			if cur, ok := shown[name]; ok && cur == st {
				continue
			}
			if err := sh.ShowStatus(ctx, st); err != nil {
				s.Logger.Printf("%s: could not show status %q: %v", name, st.Label, err)
				continue
			}
			shown[name] = st
		}
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.wakeAfterLimit()
		set(ctx, s.currentStatus())
		s.nudge(ctx)
		select {
		case <-ctx.Done():
			off, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			set(off, offline)
			cancel()
			return
		case <-t.C:
		}
	}
}

// nudgeQuiet is how long a thread that was told why its messages wait is
// not told the same again.
const nudgeQuiet = 6 * time.Hour

func (s *Server) nudge(ctx context.Context) {
	pending, err := s.Store.Pending(0)
	if err != nil {
		return
	}
	for _, e := range pending {
		after := s.Store.policy(SurfaceOf(e.Conversation)).NudgeAfter
		if e.NudgedAt != nil || e.Conversation == "" || !e.Trusted || after <= 0 || s.now().Sub(e.ReceivedAt) < after {
			continue
		}
		sf, err := s.surface(e.Conversation)
		if err != nil {
			continue
		}
		reason, why := s.waitReason(e)
		if reason == "" {
			continue
		}
		// One note per thread and reason: a second message waiting for the
		// same reason is covered by the first.
		key := "nudge:" + e.Conversation + "/" + e.ThreadID
		if last := strings.SplitN(s.Store.Meta(key), "|", 2); len(last) == 2 && last[1] == reason {
			if at, err := time.Parse(time.RFC3339, last[0]); err == nil && s.now().Sub(at) < nudgeQuiet {
				s.Store.MarkNudged(e.ID)
				continue
			}
		}
		if err := sf.Notify(ctx, e, "I've received this. "+why); err != nil {
			s.Logger.Printf("%s: could not tell %s about %s: %v", sf.Name(), e.Author, e.ID, err)
			continue
		}
		s.Store.SetMeta(key, s.now().Format(time.RFC3339)+"|"+reason)
		if err := s.Store.MarkNudged(e.ID); err != nil {
			s.Logger.Printf("bridge: nudged about %s but could not record it: %v", e.ID, err)
		}
		s.Logger.Printf("%s: told %s that %s waits (%s, %s)", sf.Name(), e.Author, e.ID, s.now().Sub(e.ReceivedAt).Round(time.Minute), reason)
	}
}

// TellPerson says something about Samrajya itself in the Home
// conversation, or only logs it when there is none.
func (s *Server) TellPerson(ctx context.Context, text string) {
	s.Logger.Printf("bridge: for the person: %s", text)
	if s.Home == "" {
		return
	}
	sf, err := s.surface(s.Home)
	if err == nil {
		_, err = sf.Deliver(ctx, s.Home, "", text)
	}
	if err != nil {
		s.Logger.Printf("bridge: could not tell the person in %s: %v", s.Home, err)
	}
}

// waitReason is why a ping is still waiting, for the note that tells the
// person: a short key (one note per thread and key) and the sentence. An
// empty key means say nothing (the person was told otherwise).
func (s *Server) waitReason(e Event) (string, string) {
	if until := s.limitedUntil(); !until.IsZero() {
		return "limit " + until.Format(time.RFC3339), "I'm at my usage limit until " + untilText(until) + ". It's queued, and I'll pick it up then."
	}
	owner := e.Owner()
	if owner == MainAgent {
		if !s.Live() {
			return "offline", "I'm offline right now. It's queued, and I'll pick it up as soon as I'm back."
		}
		return "busy", "I haven't started on it yet because I'm busy with other work. It's queued, and I'll pick it up as soon as I can."
	}
	a, err := s.Store.Agent(owner)
	if err != nil {
		return "", ""
	}
	switch a.State {
	case AgentFailed:
		return "", "" // the supervisor said it could not start
	case AgentIdle:
		if s.Supervisor != nil {
			if ahead, queued := s.Supervisor.Queued(owner); queued {
				if ahead == 0 {
					return "queued", "Other work is using all my free slots right now. This is next in line, and I'll start on it as soon as one frees up."
				}
				return "queued", fmt.Sprintf("Other work is using all my free slots right now. This is queued with %d ahead of it.", ahead)
			}
		}
		return "starting", "I'm about to start on it."
	case AgentStarting:
		return "starting", "I'm starting a session for it; it'll pick this up in a moment."
	default:
		return "busy", "I'm in the middle of a step on this work, and I'll read this as soon as that step ends."
	}
}

// status is what an agent needs after a restart or compaction: its own
// pending events, open tasks and unresolved actions (everyone's for the
// main and for a caller that is not attached), and where it can reply.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.init()
	var me string
	for _, a := range s.atts {
		if h := r.Header.Get("X-Samrajya-Attachment"); h != "" && a.holder == h {
			me = a.agent
		}
	}
	main := s.attachmentOf(MainAgent)
	s.mu.Unlock()
	var rec Recovery
	var err error
	if me != "" && me != MainAgent {
		rec, err = s.Store.AgentRecovery(me)
	} else {
		rec, err = s.Store.Recovery()
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	names := s.surfaceNames()
	sort.Strings(names)
	out := map[string]any{"main": main, "you": me, "surfaces": names, "recovery": rec}
	if s.Brain != nil {
		out["brainIndex"] = s.Brain.Index()
	}
	writeJSON(w, http.StatusOK, out)
}

// WaitPending returns the pending events after seq, waiting up to wait for
// one to arrive. The wake channel is taken before the store is read, so an
// event stored in between still ends the wait.
func (s *Server) WaitPending(ctx context.Context, after uint64, wait time.Duration) ([]Event, error) {
	return s.WaitPendingFor(ctx, "", after, wait)
}

// WaitPendingFor is WaitPending for one agent's events ("" for everyone's).
func (s *Server) WaitPendingFor(ctx context.Context, agent string, after uint64, wait time.Duration) ([]Event, error) {
	s.mu.Lock()
	s.init()
	s.mu.Unlock()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		wake := s.wake
		s.mu.Unlock()
		pending, err := s.Store.PendingFor(agent, after)
		if err != nil || len(pending) > 0 || wait <= 0 {
			return pending, err
		}
		select {
		case <-wake:
		case <-deadline.C:
			wait = 0
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// events long-polls for pending events after a sequence number.
func (s *Server) events(w http.ResponseWriter, r *http.Request, agent string) {
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	wait, _ := time.ParseDuration(r.URL.Query().Get("wait") + "s")
	// Events the launch brief carried are not sent again.
	if a, err := s.Store.Agent(agent); err == nil {
		after = max(after, a.BriefSeq)
	}
	pending, err := s.WaitPendingFor(r.Context(), agent, after, min(wait, maxWait))
	switch {
	case r.Context().Err() != nil:
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"events": nonNil(pending)})
	}
}

func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	t, err := s.Store.Tasks(r.URL.Query().Get("all") == "1")
	reply(w, map[string]any{"tasks": nonNil(t)}, err)
}

func (s *Server) accept(w http.ResponseWriter, r *http.Request, agent string) {
	var body struct {
		Events     []string `json:"events"`
		TaskID     string   `json:"taskId"`
		NewTask    *NewTask `json:"newTask"`
		NextAction string   `json:"nextAction"`
	}
	if decode(w, r, &body) {
		t, err := s.Store.AcceptAs(agent, body.Events, body.TaskID, body.NewTask, body.NextAction)
		reply(w, t, err)
	}
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request, _ string) {
	var n NewTask
	if decode(w, r, &n) {
		t, err := s.Store.CreateTask(n)
		reply(w, t, err)
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrForbidden):
		writeErr(w, http.StatusForbidden, err.Error())
	case err != nil:
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusOK, v)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
