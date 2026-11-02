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

	mu       sync.Mutex
	holder   string // attachment id
	client   string
	gen      uint64
	lastSeen time.Time
	wake     chan struct{} // closed and replaced whenever an event is stored
	now      func() time.Time
	brainMux *http.ServeMux
}

func (s *Server) init() {
	if s.wake == nil {
		s.wake = make(chan struct{})
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
	Comment(ctx context.Context, issueID, parentID, body string) error
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
	s.mu.Lock()
	s.init()
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
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

// Attachment describes who is attached, for status.
type Attachment struct {
	Client     string    `json:"client,omitempty"`
	Generation uint64    `json:"generation"`
	LastSeen   time.Time `json:"lastSeen,omitempty"`
	Live       bool      `json:"live"`
}

func (s *Server) attachment() Attachment {
	return Attachment{Client: s.client, Generation: s.gen, LastSeen: s.lastSeen,
		Live: s.holder != "" && s.now().Sub(s.lastSeen) < attachTTL}
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
			s.withHolder(w, r, s.brainMux.ServeHTTP)
		}
		return
	}
	switch {
	case r.Method == http.MethodPost && path == "/v1/attach":
		s.attach(w, r)
	case r.Method == http.MethodPost && path == "/v1/detach":
		s.withHolder(w, r, s.detach)
	case r.Method == http.MethodGet && path == "/v1/status":
		s.status(w)
	case r.Method == http.MethodGet && path == "/v1/events":
		s.withHolder(w, r, s.events)
	case r.Method == http.MethodGet && path == "/v1/tasks":
		s.tasks(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/tasks/"):
		t, err := s.Store.Task(strings.TrimPrefix(path, "/v1/tasks/"))
		reply(w, t, err)
	case r.Method == http.MethodPost && path == "/v1/accept":
		s.withHolder(w, r, s.accept)
	case r.Method == http.MethodPost && path == "/v1/tasks":
		s.withHolder(w, r, s.createTask)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/tasks/"):
		s.withHolder(w, r, func(w http.ResponseWriter, r *http.Request) {
			var u TaskUpdate
			if decode(w, r, &u) {
				t, err := s.Store.UpdateTask(strings.TrimPrefix(path, "/v1/tasks/"), u)
				reply(w, t, err)
			}
		})
	case r.Method == http.MethodPost && path == "/v1/actions":
		s.withHolder(w, r, func(w http.ResponseWriter, r *http.Request) {
			var a Action
			if decode(w, r, &a) {
				a, err := s.Store.Intend(Action{TaskID: a.TaskID, Kind: a.Kind, Target: a.Target, Intent: a.Intent})
				reply(w, a, err)
			}
		})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/actions/"):
		s.withHolder(w, r, func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Status, Ref, Note, ThreadID, Issue string
			}
			if decode(w, r, &body) {
				a, err := s.Store.Resolve(strings.TrimPrefix(path, "/v1/actions/"), body.Status, body.Ref, body.Note, body.ThreadID, body.Issue)
				reply(w, a, err)
			}
		})
	default:
		writeErr(w, http.StatusNotFound, "unknown endpoint")
	}
}

// attach makes the caller the attached plugin instance: when nobody is, when
// the holder has gone quiet, or with takeover - which a main session does
// only when the person says this session is now the main one.
func (s *Server) attach(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Client   string `json:"client"`
		Takeover bool   `json:"takeover"`
	}
	if !decode(w, r, &body) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.attachment(); a.Live && !body.Takeover {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":    "another session is attached as the main session; take over only if the person says this one is now the main",
			"attached": a,
		})
		return
	}
	s.gen++
	s.holder = fmt.Sprintf("%d-%d", s.gen, s.now().UnixNano())
	s.client = body.Client
	s.lastSeen = s.now()
	s.Logger.Printf("bridge: %q attached (generation %d, takeover %v)", body.Client, s.gen, body.Takeover)
	writeJSON(w, http.StatusOK, map[string]any{"attachment": s.holder, "generation": s.gen})
}

// detach releases the attachment, so the next session attaches at once
// rather than after attachTTL. The plugin calls it when its session ends.
func (s *Server) detach(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Logger.Printf("bridge: %q detached (generation %d)", s.client, s.gen)
	s.holder, s.client = "", ""
	writeJSON(w, http.StatusOK, map[string]any{"detached": true})
}

func (s *Server) withHolder(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	id := r.Header.Get("X-Samrajya-Attachment")
	s.mu.Lock()
	ok := id != "" && id == s.holder
	if ok {
		s.lastSeen = s.now()
	}
	a := s.attachment()
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "this session is not attached as the main session", "attached": a})
		return
	}
	next(w, r)
}

// Status is Senapati's Linear status. It says something only when
// something is wrong: no main session, or a task waiting on the person.
// Otherwise it is clear.
type Status struct{ Emoji, Label string }

var offline = Status{"💤", "Offline - pings wait until I'm back"}

func (s *Server) currentStatus() Status {
	if !s.live() {
		return offline
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

func (s *Server) live() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	return s.attachment().Live
}

// Watch runs until ctx ends, checking every interval: it keeps Senapati's
// Linear status at currentStatus, and tells the person about any ping still
// unaccepted nudgeAfter after it arrived - once per ping, mentioning its
// author - so a ping never sits silently while the session is offline,
// busy or at its usage limit. What fails is retried at the next check. On
// the way out it leaves the status offline.
func (s *Server) Watch(ctx context.Context, every, nudgeAfter time.Duration) {
	if s.Linear == nil {
		return
	}
	shown := Status{"?", "?"} // whatever an earlier run left: set it once
	set := func(ctx context.Context, st Status) {
		if st == shown {
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
		set(ctx, s.currentStatus())
		if nudgeAfter > 0 {
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

func (s *Server) nudge(ctx context.Context, after time.Duration) {
	pending, err := s.Store.Pending(0)
	if err != nil {
		return
	}
	for _, e := range pending {
		if e.NudgedAt != nil || e.IssueID == "" || s.now().Sub(e.ReceivedAt) < after {
			continue
		}
		mention := ""
		if e.AuthorID != "" {
			if h, err := s.Linear.Handle(ctx, e.AuthorID); err == nil && h != "" {
				mention = "@" + h + " "
			}
		}
		why := "I'm offline right now. It's queued, and I'll pick it up as soon as I'm back."
		if s.live() {
			why = "I haven't started on it yet: I'm busy with something else or at my usage limit. It's queued, and I'll pick it up as soon as I can."
		}
		if err := s.Linear.Comment(ctx, e.IssueID, e.ThreadID, mention+"I've received this. "+why); err != nil {
			s.Logger.Printf("linear: could not nudge about %s: %v", e.ID, err)
			continue
		}
		if err := s.Store.MarkNudged(e.ID); err != nil {
			s.Logger.Printf("bridge: nudged about %s but could not record it: %v", e.ID, err)
		}
		s.Logger.Printf("linear: nudged %s about %s waiting %s", e.Author, e.ID, s.now().Sub(e.ReceivedAt).Round(time.Minute))
	}
}

func (s *Server) status(w http.ResponseWriter) {
	rec, err := s.Store.Recovery()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mu.Lock()
	a := s.attachment()
	s.mu.Unlock()
	out := map[string]any{"attached": a, "recovery": rec}
	if s.Brain != nil {
		out["brainIndex"] = s.Brain.Index()
	}
	writeJSON(w, http.StatusOK, out)
}

// WaitPending returns the pending events after seq, waiting up to wait for
// one to arrive. The wake channel is taken before the store is read, so an
// event stored in between still ends the wait.
func (s *Server) WaitPending(ctx context.Context, after uint64, wait time.Duration) ([]Event, error) {
	s.mu.Lock()
	s.init()
	s.mu.Unlock()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		wake := s.wake
		s.mu.Unlock()
		pending, err := s.Store.Pending(after)
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
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	wait, _ := time.ParseDuration(r.URL.Query().Get("wait") + "s")
	pending, err := s.WaitPending(r.Context(), after, min(wait, maxWait))
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

func (s *Server) accept(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Events     []string `json:"events"`
		TaskID     string   `json:"taskId"`
		NewTask    *NewTask `json:"newTask"`
		NextAction string   `json:"nextAction"`
	}
	if decode(w, r, &body) {
		t, err := s.Store.Accept(body.Events, body.TaskID, body.NewTask, body.NextAction)
		reply(w, t, err)
	}
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
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
