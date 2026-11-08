// Package bridge is Samrajya's core: a durable inbox of messages from every
// surface (Linear, chat, ...), the tasks they become, the agents that own
// them, a journal of the external actions taken for them, and an
// authenticated local API the Samrajya channel plugin reads and writes them
// through. It knows conversations, not surfaces: adapters (package linear,
// package telegram) bring messages in and take replies out.
//
// Delivery is at least once. An event stays pending until its owner accepts
// it; until then every new channel connection is sent it again. Accepting is
// idempotent, so a redelivered event is harmless.
package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	bolt "go.etcd.io/bbolt"
)

const schemaVersion = "2"

// maxBody bounds a stored comment; a longer one is cut, with its link kept.
const maxBody = 16 << 10

var (
	bMeta    = []byte("meta")
	bEvents  = []byte("events")
	bTasks   = []byte("tasks")
	bThreads = []byte("threads")
	bActions = []byte("actions")
	bAgents  = []byte("agents")
	// bConvs maps a conversation key (e.g. linear:<issue uuid>) to the task
	// its messages are routed to.
	bConvs = []byte("conversations")
	// bPosted maps a message an agent posted (<conversation>/<ref>) to its
	// task, so a reply to it goes to that task.
	bPosted = []byte("posted")
)

// Task states. A task is open until done or cancelled.
const (
	Queued          = "queued"
	Working         = "working"
	WaitingApproval = "waiting_approval"
	Blocked         = "blocked"
	Paused          = "paused"
	Done            = "done"
	Cancelled       = "cancelled"
)

var taskStates = map[string]bool{Queued: true, Working: true, WaitingApproval: true, Blocked: true, Paused: true, Done: true, Cancelled: true}

// Action outcomes. An action still Intended after its session is gone is
// uncertain: it may or may not have happened, and must be checked before it
// is tried again.
const (
	Intended = "intended"
	Happened = "done"
	Failed   = "failed"
)

// ErrNotFound is returned for an unknown event, task or action id.
var ErrNotFound = errors.New("not found")

// ErrForbidden is returned when an agent touches what another agent owns.
var ErrForbidden = errors.New("not yours")

// mayTouch allows the main everything and a worker what it owns.
func mayTouch(caller, owner string) error {
	if caller == MainAgent || caller == owner {
		return nil
	}
	return fmt.Errorf("%w: it belongs to agent %s", ErrForbidden, owner)
}

// Event is something in the inbox: a message from a surface, or a notice
// or message from Samrajya itself (Source "samrajya").
type Event struct {
	ID     string `json:"id"`
	Seq    uint64 `json:"seq"`
	Source string `json:"source"`
	Kind   string `json:"kind"`
	// Title says what the conversation is, for people (an issue's
	// identifier and title, a chat's name).
	Title string `json:"title,omitempty"`
	// Issue and IssueID: a Linear issue's identifier and UUID.
	Issue      string `json:"issue,omitempty"`
	IssueID    string `json:"issueId,omitempty"`
	IssueTitle string `json:"issueTitle,omitempty"` // events stored before Title
	// Ref is the surface's id for the message (CommentID on events stored
	// before Ref).
	Ref       string `json:"ref,omitempty"`
	CommentID string `json:"commentId,omitempty"`
	// ThreadID is where a reply goes within the conversation.
	ThreadID  string `json:"threadId,omitempty"`
	InReplyTo string `json:"inReplyTo,omitempty"`
	// Links are references (an issue, a URL); Attachments are files the
	// message carried, kept by its surface.
	Links       []string     `json:"links,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Author      string       `json:"author,omitempty"`
	AuthorID    string       `json:"authorId,omitempty"`
	Trusted     bool         `json:"trusted"`
	Body        string       `json:"body"`
	URL         string       `json:"url,omitempty"`
	Delivery    string       `json:"delivery,omitempty"`
	ReceivedAt  time.Time    `json:"receivedAt"`

	AcceptedAt *time.Time `json:"acceptedAt,omitempty"`
	TaskID     string     `json:"taskId,omitempty"`
	// NudgedAt is when the person was told the ping is still waiting.
	NudgedAt *time.Time `json:"nudgedAt,omitempty"`
	// Conversation is where the event came from and a reply goes, e.g.
	// linear:<issue uuid> or telegram:<chat id>.
	Conversation string `json:"conversation,omitempty"`
	// Agent owns the event: it alone is sent it and may accept it. Empty
	// (events stored before agents existed) means the main.
	Agent string `json:"agent,omitempty"`
}

// MessageRef is the surface's id for the message.
func (e Event) MessageRef() string {
	if e.Ref != "" {
		return e.Ref
	}
	return e.CommentID
}

// Heading says what the conversation is: its title, or failing that its
// issue or key.
func (e Event) Heading() string {
	switch {
	case e.Title != "":
		return e.Title
	case e.Issue != "" && e.IssueTitle != "":
		return e.Issue + ": " + e.IssueTitle
	case e.Issue != "":
		return e.Issue
	}
	return e.Conversation
}

// Owner is the agent that owns the event.
func (e Event) Owner() string {
	if e.Agent == "" {
		return MainAgent
	}
	return e.Agent
}

// Task is a piece of work: what was asked, where it came from, where it
// stands and what happens next. It need not be a ticket anywhere.
type Task struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Issue      string    `json:"issue,omitempty"`
	ThreadID   string    `json:"threadId,omitempty"`
	Project    string    `json:"project,omitempty"`
	Status     string    `json:"status"`
	NextAction string    `json:"nextAction,omitempty"`
	Checkpoint string    `json:"checkpoint,omitempty"`
	Result     string    `json:"result,omitempty"`
	Events     []string  `json:"events,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	// Agent owns the task; empty means the main.
	Agent string `json:"agent,omitempty"`
	// Conversations routed to the task: their new messages go to its
	// agent, and its agent may reply in them.
	Conversations []string `json:"conversations,omitempty"`
	// ReportTo are conversations its agent may reply in without taking
	// their messages (a worker reporting in the person's chat).
	ReportTo []string `json:"reportTo,omitempty"`
	// Links are references for people and agents: an issue, a PR, a doc.
	Links    []string `json:"links,omitempty"`
	Worktree string   `json:"worktree,omitempty"`
}

// Owner is the agent that owns the task.
func (t Task) Owner() string {
	if t.Agent == "" {
		return MainAgent
	}
	return t.Agent
}

func (t Task) Open() bool { return t.Status != Done && t.Status != Cancelled }

// NewTask describes a task to create.
type NewTask struct {
	Title    string   `json:"title"`
	Issue    string   `json:"issue,omitempty"`
	ThreadID string   `json:"threadId,omitempty"`
	Project  string   `json:"project,omitempty"`
	Links    []string `json:"links,omitempty"`
}

// TaskUpdate changes a task; empty fields are left as they are.
type TaskUpdate struct {
	Status     string `json:"status,omitempty"`
	NextAction string `json:"nextAction,omitempty"`
	Checkpoint string `json:"checkpoint,omitempty"`
	Result     string `json:"result,omitempty"`
	Worktree   string `json:"worktree,omitempty"`
}

// Action is a significant external effect - a Linear reply, a status change
// - recorded before it is attempted and resolved after.
type Action struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"taskId,omitempty"`
	Kind      string    `json:"kind"`
	Target    string    `json:"target"`
	Intent    string    `json:"intent"`
	Status    string    `json:"status"`
	Ref       string    `json:"ref,omitempty"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Recovery is what a main session needs after a restart or compaction to
// pick up where things stand.
type Recovery struct {
	Pending   []Event  `json:"pending"`
	Open      []Task   `json:"openTasks"`
	Uncertain []Action `json:"unresolvedActions"`
}

// Store is the bbolt-backed state. bbolt holds an exclusive lock on the
// file, so a second mahamantri on the same store fails to open it rather
// than both consuming the inbox.
type Store struct {
	db  *bolt.DB
	now func() time.Time
	// policies say how each surface's conversations are routed (see
	// route); a surface without one goes to the main.
	mu       sync.Mutex
	policies map[string]Policy
}

// SetPolicy sets how a surface's conversations are routed.
func (s *Store) SetPolicy(surface string, p Policy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.policies == nil {
		s.policies = map[string]Policy{}
	}
	s.policies[surface] = p
}

func (s *Store) policy(surface string) Policy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policies[surface]
}

func (s *Store) startsWork(surface string) bool { return s.policy(surface).StartsWork }

// Open opens (creating if needed) the store at path.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if errors.Is(err, bolt.ErrTimeout) {
		return nil, fmt.Errorf("%s is in use by another mahamantri", path)
	}
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bMeta, bEvents, bTasks, bThreads, bActions, bAgents, bConvs, bPosted} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		meta := tx.Bucket(bMeta)
		switch v := string(meta.Get([]byte("schema"))); v {
		case "":
			return meta.Put([]byte("schema"), []byte(schemaVersion))
		case schemaVersion:
			return nil
		case "1":
			// Schema 2 only adds buckets and fields; keep a copy of the
			// schema 1 store first.
			if err := tx.CopyFile(path+".schema1.bak", 0o600); err != nil {
				return fmt.Errorf("backing up the schema 1 store: %w", err)
			}
			return meta.Put([]byte("schema"), []byte(schemaVersion))
		default:
			return fmt.Errorf("store schema %q is not supported by this mahamantri (want %s)", v, schemaVersion)
		}
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Ingest keeps a message from a surface and reports whether it is new. A
// message already in the inbox (a surface redelivering it) is left as it
// is.
func (s *Store) Ingest(in Inbound) (bool, error) {
	switch {
	case in.ID == "":
		return false, errors.New("a message needs an id")
	case in.Conversation == "" || !strings.Contains(in.Conversation, ":"):
		return false, fmt.Errorf("message %s: conversation %q is not <surface>:<id>", in.ID, in.Conversation)
	}
	created := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		created, err = s.ingestTx(tx, inboundEvent(in))
		return err
	})
	return created, err
}

// IngestBatch keeps several messages and sets a meta value (a surface's
// read position) in one transaction, so the position never moves past a
// message that was not kept, and no message is kept twice.
func (s *Store) IngestBatch(ins []Inbound, metaKey, metaValue string) ([]bool, error) {
	created := make([]bool, len(ins))
	err := s.db.Update(func(tx *bolt.Tx) error {
		for i, in := range ins {
			if in.ID == "" || !strings.Contains(in.Conversation, ":") {
				return fmt.Errorf("message %q: conversation %q is not <surface>:<id>", in.ID, in.Conversation)
			}
			var err error
			if created[i], err = s.ingestTx(tx, inboundEvent(in)); err != nil {
				return err
			}
		}
		if metaKey == "" {
			return nil
		}
		return tx.Bucket(bMeta).Put([]byte("x:"+metaKey), []byte(metaValue))
	})
	return created, err
}

func inboundEvent(in Inbound) Event {
	return Event{
		ID: in.ID, Source: SurfaceOf(in.Conversation), Kind: in.Kind, Conversation: in.Conversation,
		Title: in.Title, Ref: in.Ref, ThreadID: in.Thread, InReplyTo: in.InReplyTo,
		Links: in.Links, Attachments: in.Attachments,
		Author: in.Author, AuthorID: in.AuthorID, Trusted: in.Trusted,
		Body: truncate(in.Body, maxBody), URL: in.URL, Delivery: in.Delivery,
	}
}

func (s *Store) ingest(e Event) (bool, error) {
	created := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		created, err = s.ingestTx(tx, e)
		return err
	})
	return created, err
}

func (s *Store) ingestTx(tx *bolt.Tx, e Event) (bool, error) {
	b := tx.Bucket(bEvents)
	if b.Get([]byte(e.ID)) != nil {
		return false, nil
	}
	seq, err := b.NextSequence()
	if err != nil {
		return false, err
	}
	e.Seq, e.ReceivedAt = seq, s.now()
	if e.Agent == "" {
		if e.Agent, err = s.route(tx, &e); err != nil {
			return false, err
		}
	}
	return true, putJSON(b, e.ID, e)
}

// route picks the agent that owns a new event, in the transaction that
// stores it, so an event is never stored without an owner and two messages
// for a new conversation cannot make two tasks:
//  1. a reply to a message an agent posted goes to that agent's task;
//  2. a conversation routed to a task goes to that task's agent. A
//     finished task is reopened (Policy.Reopen: an issue stays one piece of
//     work), or else the conversation goes back to the main (a chat);
//  3. a trusted message from a surface that starts work (Policy.StartsWork)
//     makes a task and a worker for it;
//  4. anything else goes to the main, which answers it or dispatches.
//
// Only trusted messages reach a worker: an untrusted one is the main's to
// judge.
func (s *Store) route(tx *bolt.Tx, e *Event) (string, error) {
	if e.Conversation == "" {
		return MainAgent, nil
	}
	tasks := tx.Bucket(bTasks)
	pol := s.policy(SurfaceOf(e.Conversation))
	if e.InReplyTo != "" && e.Trusted {
		if id := tx.Bucket(bPosted).Get([]byte(e.Conversation + "/" + e.InReplyTo)); id != nil {
			var t Task
			if err := getJSON(tasks, string(id), &t); err == nil {
				if !t.Open() {
					t.Status, t.UpdatedAt = Queued, s.now()
					if err := putJSON(tasks, t.ID, t); err != nil {
						return "", err
					}
				}
				return t.Owner(), nil
			}
		}
	}
	if id := tx.Bucket(bConvs).Get([]byte(e.Conversation)); id != nil {
		var t Task
		if err := getJSON(tasks, string(id), &t); err != nil {
			return "", fmt.Errorf("conversation %s: task %s: %w", e.Conversation, id, err)
		}
		switch {
		case t.Open():
			return t.Owner(), nil
		case pol.Reopen:
			t.Status, t.UpdatedAt = Queued, s.now()
			if err := putJSON(tasks, t.ID, t); err != nil {
				return "", err
			}
			return t.Owner(), nil
		default:
			// The work is finished: the conversation is the main's again.
			if err := tx.Bucket(bConvs).Delete([]byte(e.Conversation)); err != nil {
				return "", err
			}
			t.Conversations = slices.DeleteFunc(t.Conversations, func(c string) bool { return c == e.Conversation })
			if !slices.Contains(t.ReportTo, e.Conversation) {
				t.ReportTo = append(t.ReportTo, e.Conversation)
			}
			if err := putJSON(tasks, t.ID, t); err != nil {
				return "", err
			}
			return MainAgent, nil
		}
	}
	if !e.Trusted || !pol.StartsWork {
		return MainAgent, nil
	}
	n := NewTask{Title: e.Heading(), ThreadID: e.ThreadID, Links: e.Links}
	t, err := newTask(tasks, n, s.now())
	if err != nil {
		return "", err
	}
	if err := s.assignWorker(tx, &t, e.Conversation, true); err != nil {
		return "", err
	}
	return t.Agent, nil
}

// assignWorker gives task t a worker agent of its own, and routes conv
// (if any) to it, or only lets it report there.
func (s *Store) assignWorker(tx *bolt.Tx, t *Task, conv string, route bool) error {
	a := Agent{ID: "w-" + t.ID, Role: RoleWorker, TaskID: t.ID, State: AgentIdle, CreatedAt: s.now()}
	if err := putJSON(tx.Bucket(bAgents), a.ID, a); err != nil {
		return err
	}
	t.Agent = a.ID
	if conv != "" {
		if err := s.linkConversation(tx, t, conv, route); err != nil {
			return err
		}
	}
	return putJSON(tx.Bucket(bTasks), t.ID, *t)
}

// linkConversation routes conv to task t, or only lets t's agent report
// there. A conversation is routed to at most one task.
func (s *Store) linkConversation(tx *bolt.Tx, t *Task, conv string, route bool) error {
	if !route {
		if !slices.Contains(t.ReportTo, conv) {
			t.ReportTo = append(t.ReportTo, conv)
		}
		return nil
	}
	if id := tx.Bucket(bConvs).Get([]byte(conv)); id != nil && string(id) != t.ID {
		return fmt.Errorf("conversation %s is already routed to task %s", conv, id)
	}
	if err := tx.Bucket(bConvs).Put([]byte(conv), []byte(t.ID)); err != nil {
		return err
	}
	if !slices.Contains(t.Conversations, conv) {
		t.Conversations = append(t.Conversations, conv)
	}
	return nil
}

// LinkOpenIssues routes each open task that has no conversation yet to the
// Linear issues of its events, so follow-ups on work started before
// conversations existed go to that work, not to a new worker. It is
// idempotent and returns how many tasks it linked.
func (s *Store) LinkOpenIssues() (int, error) {
	n := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		tasks, events, convs := tx.Bucket(bTasks), tx.Bucket(bEvents), tx.Bucket(bConvs)
		var open []Task
		err := tasks.ForEach(func(_, v []byte) error {
			var t Task
			if err := json.Unmarshal(v, &t); err != nil {
				return err
			}
			if t.Open() && len(t.Conversations) == 0 {
				open = append(open, t)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, t := range open {
			linked := false
			for _, id := range t.Events {
				var e Event
				if getJSON(events, id, &e) != nil || e.Source != "linear" {
					continue
				}
				conv := e.Conversation
				if conv == "" && e.IssueID != "" {
					conv = "linear:" + e.IssueID // stored before conversations
				}
				if conv == "" || slices.Contains(t.Conversations, conv) {
					continue
				}
				if convs.Get([]byte(conv)) != nil {
					continue
				}
				if err := convs.Put([]byte(conv), []byte(t.ID)); err != nil {
					return err
				}
				t.Conversations, linked = append(t.Conversations, conv), true
			}
			if linked {
				n++
				if err := putJSON(tasks, t.ID, t); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return n, err
}

// Has reports whether Samrajya has written in threadID (linear.ThreadIndex).
func (s *Store) Has(threadID string) bool {
	found := false
	s.db.View(func(tx *bolt.Tx) error {
		found = tx.Bucket(bThreads).Get([]byte(threadID)) != nil
		return nil
	})
	return found
}

// Add records that Samrajya wrote in threadID.
func (s *Store) Add(threadID, issue string) error {
	if threadID == "" {
		return errors.New("empty thread id")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bThreads).Put([]byte(threadID), []byte(issue))
	})
}

// Meta returns a stored setting, "" if unset.
func (s *Store) Meta(key string) string {
	var v string
	s.db.View(func(tx *bolt.Tx) error {
		v = string(tx.Bucket(bMeta).Get([]byte("x:" + key)))
		return nil
	})
	return v
}

// SetMeta stores a setting, e.g. which opencode session is the main one.
func (s *Store) SetMeta(key, value string) error {
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bMeta).Put([]byte("x:"+key), []byte(value)) })
}

// Pending lists the events not yet accepted with Seq above after, oldest
// first, for every agent.
func (s *Store) Pending(after uint64) ([]Event, error) { return s.PendingFor("", after) }

// PendingFor lists the events agent owns that are not yet accepted with Seq
// above after, oldest first; agent "" lists everyone's.
func (s *Store) PendingFor(agent string, after uint64) ([]Event, error) {
	var out []Event
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bEvents).ForEach(func(_, v []byte) error {
			var e Event
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			if e.AcceptedAt == nil && e.Seq > after && (agent == "" || e.Owner() == agent) {
				out = append(out, e)
			}
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, err
}

// Event returns one event.
func (s *Store) Event(id string) (Event, error) {
	var e Event
	err := s.db.View(func(tx *bolt.Tx) error { return getJSON(tx.Bucket(bEvents), id, &e) })
	return e, err
}

// MarkNudged records that the person was told event id is still waiting.
func (s *Store) MarkNudged(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bEvents)
		var e Event
		if err := getJSON(b, id, &e); err != nil {
			return err
		}
		now := s.now()
		e.NudgedAt = &now
		return putJSON(b, id, e)
	})
}

// Notice stores a notice from Samrajya itself, such as "your usage limit
// has reset", for the main session; it is delivered like a ping and
// acknowledged without a task.
func (s *Store) Notice(id, kind, body string) error { return s.NoticeFor(MainAgent, id, kind, body) }

// NoticeFor stores a notice for one agent.
func (s *Store) NoticeFor(agent, id, kind, body string) error {
	_, err := s.ingest(Event{ID: "samrajya:" + id, Source: "samrajya", Kind: kind, Trusted: true, Body: body, Agent: agent})
	return err
}

// acknowledge marks events handled without a task: a notice seen, a
// message answered on the spot. Work that spans time goes into a task.
func (s *Store) acknowledge(caller string, ids []string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bEvents)
		now := s.now()
		for _, id := range ids {
			var e Event
			if err := getJSON(b, id, &e); err != nil {
				return fmt.Errorf("event %s: %w", id, err)
			}
			if err := mayTouch(caller, e.Owner()); err != nil {
				return fmt.Errorf("event %s: %w", id, err)
			}
			if e.AcceptedAt == nil {
				e.AcceptedAt = &now
				if err := putJSON(b, id, e); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// Accept attaches events to a task - an existing one (taskID) or one created
// for them (create) - and records what happens next, in one transaction.
// Events already accepted keep the task they were accepted into; accepting
// them again is not an error, so a redelivered ping cannot make a second
// task.
func (s *Store) Accept(eventIDs []string, taskID string, create *NewTask, next string) (Task, error) {
	return s.AcceptAs(MainAgent, eventIDs, taskID, create, next)
}

// AcceptAs is Accept by an agent, which may accept only events it owns,
// into a task it owns; only the main creates tasks this way.
func (s *Store) AcceptAs(caller string, eventIDs []string, taskID string, create *NewTask, next string) (Task, error) {
	if len(eventIDs) == 0 {
		return Task{}, errors.New("no events to accept")
	}
	if taskID == "" && create == nil {
		return Task{}, s.acknowledge(caller, eventIDs)
	}
	if create != nil && caller != MainAgent {
		return Task{}, fmt.Errorf("%w: a worker accepts into its own task (task_id), it does not start new ones", ErrForbidden)
	}
	if taskID != "" && create != nil {
		return Task{}, errors.New("give either a task id or a new task, not both")
	}
	if strings.TrimSpace(next) == "" {
		return Task{}, errors.New("say what happens next (nextAction)")
	}
	var task Task
	err := s.db.Update(func(tx *bolt.Tx) error {
		events, tasks := tx.Bucket(bEvents), tx.Bucket(bTasks)
		var fresh []Event
		for _, id := range eventIDs {
			var e Event
			if err := getJSON(events, id, &e); err != nil {
				return fmt.Errorf("event %s: %w", id, err)
			}
			if err := mayTouch(caller, e.Owner()); err != nil {
				return fmt.Errorf("event %s: %w", id, err)
			}
			if e.AcceptedAt == nil {
				fresh = append(fresh, e)
			}
		}
		if len(fresh) == 0 {
			// All accepted before: answer with where the first one went.
			var e Event
			getJSON(events, eventIDs[0], &e)
			return getJSON(tasks, e.TaskID, &task)
		}
		now := s.now()
		if create != nil {
			t, err := newTask(tasks, *create, now)
			if err != nil {
				return err
			}
			task = t
		} else if err := getJSON(tasks, taskID, &task); err != nil {
			return fmt.Errorf("task %s: %w", taskID, err)
		} else if err := mayTouch(caller, task.Owner()); err != nil {
			return fmt.Errorf("task %s: %w", taskID, err)
		}
		for _, e := range fresh {
			e.AcceptedAt, e.TaskID = &now, task.ID
			if err := putJSON(events, e.ID, e); err != nil {
				return err
			}
			task.Events = append(task.Events, e.ID)
			if task.Issue == "" {
				task.Issue = e.Issue
			}
			for _, l := range e.Links {
				if !slices.Contains(task.Links, l) {
					task.Links = append(task.Links, l)
				}
			}
			if task.ThreadID == "" {
				task.ThreadID = e.ThreadID
			}
		}
		if !task.Open() {
			task.Status = Queued // new input on a finished task reopens it
		}
		task.NextAction, task.UpdatedAt = next, now
		return putJSON(tasks, task.ID, task)
	})
	return task, err
}

// CreateTask creates a task not started by a ping, e.g. from a conversation.
func (s *Store) CreateTask(n NewTask) (Task, error) {
	var t Task
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		t, err = newTask(tx.Bucket(bTasks), n, s.now())
		return err
	})
	return t, err
}

func newTask(b *bolt.Bucket, n NewTask, now time.Time) (Task, error) {
	if strings.TrimSpace(n.Title) == "" {
		return Task{}, errors.New("a task needs a title")
	}
	seq, err := b.NextSequence()
	if err != nil {
		return Task{}, err
	}
	t := Task{ID: fmt.Sprintf("T%d", seq), Title: n.Title, Issue: n.Issue, ThreadID: n.ThreadID, Project: n.Project,
		Links: n.Links, Status: Queued, CreatedAt: now, UpdatedAt: now}
	return t, putJSON(b, t.ID, t)
}

// UpdateTask changes a task's status, next action, checkpoint or result. A
// task is only done with a result saying what was achieved.
func (s *Store) UpdateTask(id string, u TaskUpdate) (Task, error) {
	return s.UpdateTaskAs(MainAgent, id, u)
}

// UpdateTaskAs is UpdateTask by an agent, which may update only its own task.
func (s *Store) UpdateTaskAs(caller, id string, u TaskUpdate) (Task, error) {
	if u.Status != "" && !taskStates[u.Status] {
		return Task{}, fmt.Errorf("status %q is not one of queued, working, waiting_approval, blocked, paused, done, cancelled", u.Status)
	}
	var t Task
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bTasks)
		if err := getJSON(b, id, &t); err != nil {
			return fmt.Errorf("task %s: %w", id, err)
		}
		if err := mayTouch(caller, t.Owner()); err != nil {
			return fmt.Errorf("task %s: %w", id, err)
		}
		if u.Worktree != "" {
			t.Worktree = u.Worktree
		}
		if u.Status != "" {
			t.Status = u.Status
		}
		if u.NextAction != "" {
			t.NextAction = u.NextAction
		}
		if u.Checkpoint != "" {
			t.Checkpoint = u.Checkpoint
		}
		if u.Result != "" {
			t.Result = u.Result
		}
		if t.Status == Done && strings.TrimSpace(t.Result) == "" {
			return errors.New("a task is done only with a result: what was achieved and how it was verified")
		}
		t.UpdatedAt = s.now()
		return putJSON(b, t.ID, t)
	})
	return t, err
}

// Task returns one task.
func (s *Store) Task(id string) (Task, error) {
	var t Task
	err := s.db.View(func(tx *bolt.Tx) error { return getJSON(tx.Bucket(bTasks), id, &t) })
	return t, err
}

// Tasks lists tasks, most recently updated first; only open ones unless all.
func (s *Store) Tasks(all bool) ([]Task, error) {
	var out []Task
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bTasks).ForEach(func(_, v []byte) error {
			var t Task
			if err := json.Unmarshal(v, &t); err != nil {
				return err
			}
			if all || t.Open() {
				out = append(out, t)
			}
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, err
}

// Intend records an external action before it is attempted.
func (s *Store) Intend(a Action) (Action, error) { return s.IntendAs(MainAgent, a) }

// IntendAs is Intend by an agent: a worker records actions for its own task.
func (s *Store) IntendAs(caller string, a Action) (Action, error) {
	if a.Kind == "" || a.Target == "" || a.Intent == "" {
		return Action{}, errors.New("an action needs a kind, a target and what it will do (intent)")
	}
	if caller != MainAgent && a.TaskID == "" {
		return Action{}, fmt.Errorf("%w: give the task the action is for", ErrForbidden)
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		if a.TaskID != "" {
			var t Task
			if err := getJSON(tx.Bucket(bTasks), a.TaskID, &t); err != nil {
				return fmt.Errorf("task %s: %w", a.TaskID, err)
			}
			if err := mayTouch(caller, t.Owner()); err != nil {
				return fmt.Errorf("task %s: %w", a.TaskID, err)
			}
		}
		b := tx.Bucket(bActions)
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		now := s.now()
		a.ID, a.Status, a.CreatedAt, a.UpdatedAt = fmt.Sprintf("A%d", seq), Intended, now, now
		return putJSON(b, a.ID, a)
	})
	return a, err
}

// Resolve records how an intended action turned out. When it posted a
// reply, threadID (the thread it is in) is learned at once, so a reply
// there reaches Samrajya even before the surface reports the reply itself.
func (s *Store) Resolve(id, status, ref, note, threadID, issue string) (Action, error) {
	return s.ResolveAs(MainAgent, id, status, ref, note, threadID, issue)
}

// ResolveAs is Resolve by an agent, for an action of its own task.
func (s *Store) ResolveAs(caller, id, status, ref, note, threadID, issue string) (Action, error) {
	if status != Happened && status != Failed {
		return Action{}, fmt.Errorf("status %q must be %q or %q", status, Happened, Failed)
	}
	var a Action
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bActions)
		if err := getJSON(b, id, &a); err != nil {
			return fmt.Errorf("action %s: %w", id, err)
		}
		if caller != MainAgent {
			var t Task
			if err := getJSON(tx.Bucket(bTasks), a.TaskID, &t); err != nil || t.Owner() != caller {
				return fmt.Errorf("action %s: %w", id, ErrForbidden)
			}
		}
		a.Status, a.Ref, a.Note, a.UpdatedAt = status, ref, note, s.now()
		if status == Happened && threadID != "" {
			if err := tx.Bucket(bThreads).Put([]byte(threadID), []byte(issue)); err != nil {
				return err
			}
		}
		if status == Happened && ref != "" && a.TaskID != "" && strings.Contains(issue, ":") {
			// issue is the conversation for replies posted through a
			// surface: a reply to this message goes to the task.
			if err := tx.Bucket(bPosted).Put([]byte(issue+"/"+ref), []byte(a.TaskID)); err != nil {
				return err
			}
		}
		return putJSON(b, a.ID, a)
	})
	return a, err
}

// Recovery returns pending events, open tasks and actions never resolved.
func (s *Store) Recovery() (Recovery, error) {
	var r Recovery
	var err error
	if r.Pending, err = s.Pending(0); err != nil {
		return r, err
	}
	if r.Open, err = s.Tasks(false); err != nil {
		return r, err
	}
	err = s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bActions).ForEach(func(_, v []byte) error {
			var a Action
			if err := json.Unmarshal(v, &a); err != nil {
				return err
			}
			if a.Status == Intended {
				r.Uncertain = append(r.Uncertain, a)
			}
			return nil
		})
	})
	r.Pending, r.Open, r.Uncertain = nonNil(r.Pending), nonNil(r.Open), nonNil(r.Uncertain)
	return r, err
}

// Said is what has been posted, or tried, in a task's conversations for
// it, oldest first and at most the last limit: replies recorded against the
// task, and replies in its thread (the main's acknowledgement may come
// before the task exists). A session starting on the task reads it, so it
// does not say again what was already said.
func (s *Store) Said(t Task, limit int) ([]Action, error) {
	threads := map[string]bool{}
	if t.ThreadID != "" {
		for _, c := range t.Conversations {
			threads[c+" thread "+t.ThreadID] = true
		}
	}
	var out []Action
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bActions).ForEach(func(_, v []byte) error {
			var a Action
			if err := json.Unmarshal(v, &a); err != nil {
				return err
			}
			if strings.HasSuffix(a.Kind, "_reply") && a.Status != Failed && (a.TaskID == t.ID || threads[a.Target]) {
				out = append(out, a)
			}
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, err
}

func putJSON(b *bolt.Bucket, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return b.Put([]byte(key), data)
}

func getJSON(b *bolt.Bucket, key string, v any) error {
	data := b.Get([]byte(key))
	if data == nil {
		return ErrNotFound
	}
	return json.Unmarshal(data, v)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n[... cut; read the rest at the source]"
}
