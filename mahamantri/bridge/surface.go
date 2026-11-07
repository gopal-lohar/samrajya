package bridge

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A surface is a place messages come from and replies go to: Linear, a
// chat app, later a call or a meeting. The core knows conversations, not
// surfaces; each surface's adapter turns its messages into Inbound events
// (Server.Receive) and implements Surface for what goes back.
//
// A conversation key is "<surface>:<id>", e.g. linear:<issue uuid> or
// telegram:<chat id>. Within a conversation a thread (a Linear comment
// thread, a chat message to reply to) says where exactly a reply goes.
type Surface interface {
	// Name is the surface's prefix in conversation keys.
	Name() string
	// Deliver posts body in conversation conv, in thread if given.
	Deliver(ctx context.Context, conv, thread, body string) (Posted, error)
	// Received marks an incoming event as seen, so its author knows it
	// arrived even while nobody can answer yet. Best effort.
	Received(ctx context.Context, e Event) error
	// Notify tells an event's author something about it (that it is still
	// waiting, that it could not be started), where they will see it.
	Notify(ctx context.Context, e Event, text string) error
}

// StatusShower is a surface that can show the assistant's state (Linear's
// user status). The server calls it with the current Status whenever it
// changes; an empty Status means all is well.
type StatusShower interface {
	ShowStatus(ctx context.Context, st Status) error
}

// Posted is what a surface created for a reply.
type Posted struct {
	ID     string `json:"id"`
	Thread string `json:"thread,omitempty"`
	URL    string `json:"url,omitempty"`
}

// Inbound is a message a surface adapter receives.
type Inbound struct {
	// ID identifies the message for good, e.g. linear:<comment id>; a
	// redelivery with the same ID is dropped.
	ID           string
	Conversation string // <surface>:<id>
	Title        string // what the conversation is, for people
	Thread       string // where a reply goes within the conversation
	Kind         string // e.g. mention, reply, message
	Author       string
	AuthorID     string
	// Trusted: the author may ask for work. Others' messages are
	// information, not instructions.
	Trusted bool
	Body    string
	URL     string
	// Delivery is the surface's id for this delivery, for the log.
	Delivery string
	// Ref is the surface's own id for the message (a comment id, a chat
	// message id), for Received and Notify.
	Ref string
	// InReplyTo is the surface's id of the message this one answers, when
	// the surface has replies (a chat reply). A reply to something an
	// agent posted goes to that agent's task.
	InReplyTo string
	// Links are references for people and agents: the issue, a URL.
	Links []string
	// Attachments are files that came with the message (a voice note, a
	// photo), described for the agent; the surface keeps the files.
	Attachments []Attachment
}

// Attachment is a file a message carried.
type Attachment struct {
	Kind    string `json:"kind"` // voice, audio, photo, video, document
	Ref     string `json:"ref"`  // the surface's id for the file
	Name    string `json:"name,omitempty"`
	Seconds int    `json:"seconds,omitempty"`
}

// Policy is how the core treats a surface's conversations.
type Policy struct {
	// StartsWork: a trusted message in a conversation no task has starts a
	// task and a worker of its own (an issue is a piece of work). Off, it
	// goes to the main (a chat with the person).
	StartsWork bool
	// Reopen: a message in a conversation routed to a finished task
	// reopens it (an issue stays one piece of work). Off, the conversation
	// goes back to the main once its task is finished.
	Reopen bool
	// NudgeAfter: how long a message may wait unaccepted before its author
	// is told why; 0: never.
	NudgeAfter time.Duration
}

// SurfaceOf is the surface a conversation key belongs to.
func SurfaceOf(conv string) string {
	name, _, _ := strings.Cut(conv, ":")
	return name
}

// surface finds the adapter for a conversation.
func (s *Server) surface(conv string) (Surface, error) {
	if sf := s.Surfaces[SurfaceOf(conv)]; sf != nil && conv != "" {
		return sf, nil
	}
	return nil, fmt.Errorf("no surface for conversation %q (known: %s)", conv, strings.Join(s.surfaceNames(), ", "))
}

func (s *Server) surfaceNames() []string {
	var out []string
	for n := range s.Surfaces {
		out = append(out, n)
	}
	return out
}

// Canonicalizer is a surface with more than one way to name a
// conversation (Linear: an issue's key or UUID); Canonical returns the one
// messages arrive under.
type Canonicalizer interface {
	Canonical(ctx context.Context, conv string) (string, error)
}

// AddSurface registers an adapter and its policy.
func (s *Server) AddSurface(sf Surface, p Policy) {
	if s.Surfaces == nil {
		s.Surfaces = map[string]Surface{}
	}
	s.Surfaces[sf.Name()] = sf
	s.Store.SetPolicy(sf.Name(), p)
}

// canonical names a conversation the way its messages arrive.
func (s *Server) canonical(ctx context.Context, conv string) (string, error) {
	if conv == "" {
		return "", nil
	}
	sf, err := s.surface(conv)
	if err != nil {
		return "", err
	}
	if c, ok := sf.(Canonicalizer); ok {
		return c.Canonical(ctx, conv)
	}
	return conv, nil
}

// ReceiveBatch stores messages and a surface's read position together (see
// Store.IngestBatch), wakes the feeds and marks the new messages received.
func (s *Server) ReceiveBatch(ins []Inbound, metaKey, metaValue string) error {
	created, err := s.Store.IngestBatch(ins, metaKey, metaValue)
	if err != nil {
		return err
	}
	s.Wake()
	for i, in := range ins {
		if created[i] {
			s.markReceived(in)
		}
	}
	return nil
}

// Meta reads a stored value (a surface's read position).
func (s *Server) Meta(key string) string { return s.Store.Meta(key) }

func (s *Server) markReceived(in Inbound) {
	sf, err := s.surface(in.Conversation)
	if err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		e, err := s.Store.Event(in.ID)
		if err == nil {
			err = sf.Received(ctx, e)
		}
		if err != nil {
			s.Logger.Printf("%s: could not mark %s received: %v", sf.Name(), in.ID, err)
		}
	}()
}

// Fetcher is a surface that keeps the files messages carry (Telegram) and
// can hand one to an agent.
type Fetcher interface {
	Fetch(ctx context.Context, ref string) (name string, data []byte, err error)
}

// fetch saves a message's attachment under FilesDir and answers with its
// path, for the agent to read.
func (s *Server) fetch(w http.ResponseWriter, r *http.Request, _ string) {
	var body struct {
		EventID string `json:"eventId"`
		Ref     string `json:"ref"`
	}
	if !decode(w, r, &body) {
		return
	}
	e, err := s.Store.Event(body.EventID)
	if err != nil {
		reply(w, nil, err)
		return
	}
	found := false
	for _, a := range e.Attachments {
		found = found || a.Ref == body.Ref
	}
	if !found {
		writeErr(w, http.StatusNotFound, "that event has no attachment "+body.Ref)
		return
	}
	sf, err := s.surface(e.Conversation)
	f, ok := sf.(Fetcher)
	if err != nil || !ok || s.FilesDir == "" {
		writeErr(w, http.StatusNotImplemented, "files from this surface cannot be fetched here")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	name, data, err := f.Fetch(ctx, body.Ref)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	dir := filepath.Join(s.FilesDir, safeName(e.ID))
	path := filepath.Join(dir, safeName(name))
	if err := os.MkdirAll(dir, 0o700); err == nil {
		err = os.WriteFile(path, data, 0o600)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "bytes": len(data)})
}

func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
	if s == "" || s == "." || s == ".." {
		s = "file"
	}
	return s
}
