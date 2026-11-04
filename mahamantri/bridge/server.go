package bridge

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/brain"
	"github.com/gopal-lohar/samrajya/mahamantri/linear"
)

const (
	maxRequest = 64 << 10
	// A channel connection that has not asked for events this long is gone;
	// another may attach without taking over.
	attachTTL = 90 * time.Second
	maxWait   = 30 * time.Second
)

// Server is the local API between mahamantri and the Samrajya channel plugin
// running inside the main session. Every request carries the bearer token
// mahamantri wrote to its discovery file. One plugin instance at a time is
// attached - the main session's - and only it may read the event feed or
// change state; another (a second session with the plugin loaded) is
// refused until the first goes quiet or is explicitly taken over.
//
// Server is also the webhook's linear.Sink: it stores a ping and wakes the
// feed.
type Server struct {
	Store  *Store
	Token  string
	Logger *log.Logger
	// Brain, if set, is served under /brain (see brain.Routes): any client
	// may read it, only the attached one change it.
	Brain *brain.Store
	// Linear, if set, acts on Linear as Senapati: it reacts to each new ping
	// with Received once it is stored, and Watch keeps Senapati's status
	// and nudges about pings left waiting.
	Linear   Linear
	Received string

	// Agents turns on agents mode (docs/design/agents.md): each session
	// attaches as its agent, with the secret its launcher gave it, and
	// nothing attaches by being first. Off, one session attaches as the main
	// one, as before agents.
	Agents bool
	// Supervisor, in agents mode, stops agents for the main
	// (samrajya_stop_agent).
	Supervisor interface {
		StopAgent(ctx context.Context, id string) error
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

// Linear is what the server does on Linear itself (linear.API).
type Linear interface {
	React(ctx context.Context, commentID, emoji string) error
	SetStatus(ctx context.Context, emoji, label string) error
	Comment(ctx context.Context, issue, parentID, body string) (linear.Posted, error)
	Handle(ctx context.Context, userID string) (string, error)
}

// Ingest stores a ping and wakes any waiting feed request. A new ping gets
// the Received reaction, so the person sees at once that it arrived even
// while the main session is busy or offline; that is best effort and never
// fails the ingest.
func (s *Server) Ingest(p linear.Ping, delivery string) error {
	created, err := s.Store.IngestNew(p, delivery)
	if err != nil {
		return err
	}
	s.Wake()
	if created && s.Linear != nil && s.Received != "" && p.CommentID != "" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := s.Linear.React(ctx, p.CommentID, s.Received); err != nil {
				s.Logger.Printf("linear: could not react to comment %s: %v", p.CommentID, err)
			}
		}()
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

// Attachment describes who is attached as an agent, for status.
type Attachment struct {
	Agent      string    `json:"agent"`
	Client     string    `json:"client,omitempty"`
	Generation uint64    `json:"generation"`
	LastSeen   time.Time `json:"lastSeen,omitempty"`
	Live       bool      `json:"live"`
}

// attachmentOf describes agent's attachment; call with s.mu held.
func (s *Server) attachmentOf(agent string) Attachment {
	a := s.atts[agent]
	if a == nil {
		return Attachment{Agent: agent}
	}
	return Attachment{Agent: agent, Client: a.client, Generation: a.gen, LastSeen: a.lastSeen, Live: a.live(s.now())}
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
				writeErr(w, http.StatusNotImplemented, "agents mode is off")
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

// ProtocolVersion is the plugin protocol this mahamantri serves. A plugin
// that sends none is from before agents and is served in legacy mode only.
const ProtocolVersion = 2

// attach attaches the caller as an agent.
//
// Agents mode: the caller names its agent and gives the secret of the
// agent's current launch; anything else is refused (and stays read-only). A
// launch's secret is replaced on the next launch, so a replaced session can
// no longer attach or act. The agent's earlier attachment, if any, is
// replaced: it is the same session reconnecting.
//
// Legacy mode: the caller becomes the main session when nobody is attached,
// when the holder has gone quiet, or with takeover - which a session does
// only when the person says this one is now the main.
func (s *Server) attach(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Client   string `json:"client"`
		Takeover bool   `json:"takeover"`
		Agent    string `json:"agent"`
		Secret   string `json:"secret"`
		Protocol int    `json:"protocol"`
	}
	if !decode(w, r, &body) {
		return
	}
	agent, secret := MainAgent, ""
	if s.Agents {
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
		agent, secret = a.ID, a.Secret
	} else if body.Agent != "" && body.Agent != MainAgent {
		writeErr(w, http.StatusConflict, "agents mode is off: only the main session attaches")
		return
	}
	s.mu.Lock()
	s.init()
	if cur := s.atts[agent]; !s.Agents && cur.live(s.now()) && !body.Takeover {
		a := s.attachmentOf(agent)
		s.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":    "another session is attached as the main session; take over only if the person says this one is now the main",
			"attached": a,
		})
		return
	}
	s.seq++
	att := &attachment{agent: agent, secret: secret, client: body.Client, gen: s.seq, lastSeen: s.now(), protocol: body.Protocol,
		holder: fmt.Sprintf("%s-%d-%d", agent, s.seq, s.now().UnixNano())}
	s.atts[agent] = att
	s.mu.Unlock()
	if s.Agents {
		s.Store.UpdateAgent(agent, func(a *Agent) error {
			if a.Secret == secret {
				a.State, a.AttachedAt, a.Failures, a.Error = AgentLive, s.now(), 0, ""
			}
			return nil
		})
	}
	s.Logger.Printf("bridge: %q attached as %s (attachment %d, takeover %v)", body.Client, agent, att.gen, body.Takeover)
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
// refusing a request without one, and in agents mode one whose launch has
// since been replaced.
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
	if att != nil && s.Agents {
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
	if r.Method == http.MethodPost {
		// The session is calling tools, so turns run again.
		s.clearLimitOnActivity()
	}
	next(w, r, att.agent)
}

// reply posts a comment as Senapati for the main session, journaled: the
// action is recorded before posting and resolved with the comment after, so
// the comment's thread is known at once and a crash in between leaves it
// uncertain rather than forgotten. A comment posted this way, rather than
// through the session's Linear connector, is Senapati's whoever the
// connector belongs to.
func (s *Server) reply(w http.ResponseWriter, r *http.Request, agent string) {
	var body struct {
		TaskID   string `json:"taskId"`
		Issue    string `json:"issue"`
		ThreadID string `json:"threadId"`
		Body     string `json:"body"`
	}
	if !decode(w, r, &body) {
		return
	}
	switch {
	case s.Linear == nil:
		writeErr(w, http.StatusNotImplemented, "mahamantri has no Linear API key (linear.apiKey), so it cannot post")
		return
	}
	task, err := s.Store.MayReply(agent, body.Issue)
	if err != nil {
		reply(w, nil, err)
		return
	}
	if body.Issue == "" {
		body.Issue = task.Issue // a worker's own issue
	}
	if body.TaskID == "" {
		body.TaskID = task.ID
	}
	if body.Issue == "" || strings.TrimSpace(body.Body) == "" {
		writeErr(w, http.StatusBadRequest, "issue and body are required")
		return
	}
	target := body.Issue
	if body.ThreadID != "" {
		target += " thread " + body.ThreadID
	}
	a, err := s.Store.IntendAs(agent, Action{TaskID: body.TaskID, Kind: "linear_comment", Target: target, Intent: truncate(body.Body, 200)})
	if err != nil {
		reply(w, nil, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	posted, err := s.Linear.Comment(ctx, body.Issue, body.ThreadID, body.Body)
	if err != nil {
		// The comment may or may not exist (a timeout, say): leave the
		// action unresolved, to be checked on Linear before retrying.
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error() + " - check the thread on Linear before posting again", "actionId": a.ID})
		return
	}
	if _, err := s.Store.Resolve(a.ID, Happened, posted.ID, "", posted.ThreadID, posted.Issue); err != nil {
		s.Logger.Printf("bridge: posted comment %s but could not record it: %v", posted.ID, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"commentId": posted.ID, "threadId": posted.ThreadID, "issue": posted.Issue, "url": posted.URL, "actionId": a.ID})
}

// Status is Senapati's Linear status. It says something only when
// something is wrong: no main session, or a task waiting on the person.
// Otherwise it is clear.
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
				if t.Issue != "" {
					return Status{"⚠️", "Blocked on " + t.Issue + " - needs you"}
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
// session once its usage limit has reset, and with a Linear key keeps
// Senapati's Linear status at currentStatus and tells the person about a
// ping still unaccepted nudgeAfter after it arrived - mentioning its author,
// saying why it waits, once per thread and reason - so a ping never sits
// silently while the session is offline, busy or at its usage limit. What
// fails is retried at the next check. On the way out it leaves the status
// offline.
func (s *Server) Watch(ctx context.Context, every, nudgeAfter time.Duration) {
	shown := Status{"?", "?"} // whatever an earlier run left: set it once
	set := func(ctx context.Context, st Status) {
		if st == shown || s.Linear == nil {
			return
		}
		if err := s.Linear.SetStatus(ctx, st.Emoji, st.Label); err != nil {
			s.Logger.Printf("linear: could not set status %q: %v", st.Label, err)
			return
		}
		shown = st
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.wakeAfterLimit()
		set(ctx, s.currentStatus())
		if nudgeAfter > 0 && s.Linear != nil {
			s.nudge(ctx, nudgeAfter)
		}
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

// nudgeQuiet is how long a thread that was told why its pings wait is not
// told the same again.
const nudgeQuiet = 6 * time.Hour

func (s *Server) nudge(ctx context.Context, after time.Duration) {
	pending, err := s.Store.Pending(0)
	if err != nil {
		return
	}
	for _, e := range pending {
		if e.NudgedAt != nil || e.IssueID == "" || s.now().Sub(e.ReceivedAt) < after {
			continue
		}
		reason, why := "offline", "I'm offline right now. It's queued, and I'll pick it up as soon as I'm back."
		if until := s.limitedUntil(); !until.IsZero() {
			reason = "limit " + until.Format(time.RFC3339)
			why = "I'm at my usage limit until " + untilText(until) + ". It's queued, and I'll pick it up then."
		} else if s.Live() {
			reason, why = "busy", "I haven't started on it yet because I'm busy with other work. It's queued, and I'll pick it up as soon as I can."
		}
		// One comment per thread and reason: a second ping waiting for the
		// same reason is covered by the first nudge.
		key := "nudge:" + e.ThreadID
		if last := strings.SplitN(s.Store.Meta(key), "|", 2); len(last) == 2 && last[1] == reason {
			if at, err := time.Parse(time.RFC3339, last[0]); err == nil && s.now().Sub(at) < nudgeQuiet {
				s.Store.MarkNudged(e.ID)
				continue
			}
		}
		mention := ""
		if e.AuthorID != "" {
			if h, err := s.Linear.Handle(ctx, e.AuthorID); err == nil && h != "" {
				mention = "@" + h + " "
			}
		}
		if _, err := s.Linear.Comment(ctx, e.IssueID, e.ThreadID, mention+"I've received this. "+why); err != nil {
			s.Logger.Printf("linear: could not nudge about %s: %v", e.ID, err)
			continue
		}
		s.Store.SetMeta(key, s.now().Format(time.RFC3339)+"|"+reason)
		if err := s.Store.MarkNudged(e.ID); err != nil {
			s.Logger.Printf("bridge: nudged about %s but could not record it: %v", e.ID, err)
		}
		s.Logger.Printf("linear: nudged %s about %s waiting %s (%s)", e.Author, e.ID, s.now().Sub(e.ReceivedAt).Round(time.Minute), reason)
	}
}

// status is what an agent needs after a restart or compaction: its own
// pending events, open tasks and unresolved actions (everyone's for the
// main and for a caller that is not attached), and who is attached.
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
	out := map[string]any{"attached": main, "you": me, "agentsMode": s.Agents, "recovery": rec}
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
	if s.Agents {
		// Events the launch brief carried are not sent again.
		if a, err := s.Store.Agent(agent); err == nil {
			after = max(after, a.BriefSeq)
		}
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
