package bridge

import (
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

	mu       sync.Mutex
	holder   string // attachment id
	client   string
	gen      uint64
	lastSeen time.Time
	wake     chan struct{} // closed and replaced whenever an event is stored
	now      func() time.Time
}

func (s *Server) init() {
	if s.wake == nil {
		s.wake = make(chan struct{})
	}
	if s.now == nil {
		s.now = time.Now
	}
}

// Ingest stores a ping and wakes any waiting feed request.
func (s *Server) Ingest(p linear.Ping, delivery string) error {
	if err := s.Store.Ingest(p, delivery); err != nil {
		return err
	}
	s.mu.Lock()
	s.init()
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
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

func (s *Server) status(w http.ResponseWriter) {
	rec, err := s.Store.Recovery()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mu.Lock()
	a := s.attachment()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"attached": a, "recovery": rec})
}

// events long-polls for pending events after a sequence number. The wake
// channel is taken before the store is read, so an event stored in between
// still wakes this request.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	wait, _ := time.ParseDuration(r.URL.Query().Get("wait") + "s")
	if wait > maxWait {
		wait = maxWait
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		wake := s.wake
		s.mu.Unlock()
		pending, err := s.Store.Pending(after)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(pending) > 0 || wait <= 0 {
			writeJSON(w, http.StatusOK, map[string]any{"events": nonNil(pending)})
			return
		}
		select {
		case <-wake:
		case <-deadline.C:
			wait = 0
		case <-r.Context().Done():
			return
		}
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
