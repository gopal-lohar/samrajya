// Package bridge is what a native Claude Code main session needs from
// mahamantri: a durable inbox of Linear pings, the tasks they become, a
// journal of the external actions taken for them, and an authenticated
// local API the Samrajya channel plugin reads and writes them through.
//
// Delivery is at least once. A ping stays pending until the main session
// accepts it into a task; until then every new channel connection is sent
// it again. Accepting is idempotent, so a redelivered ping is harmless.
package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gopal-lohar/samrajya/mahamantri/linear"
	bolt "go.etcd.io/bbolt"
)

const schemaVersion = "1"

// maxBody bounds a stored comment; a longer one is cut, with its link kept.
const maxBody = 16 << 10

var (
	bMeta    = []byte("meta")
	bEvents  = []byte("events")
	bTasks   = []byte("tasks")
	bThreads = []byte("threads")
	bActions = []byte("actions")
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

// Event is a ping in the inbox.
type Event struct {
	ID         string    `json:"id"`
	Seq        uint64    `json:"seq"`
	Source     string    `json:"source"`
	Kind       string    `json:"kind"`
	Issue      string    `json:"issue,omitempty"`
	IssueID    string    `json:"issueId,omitempty"`
	IssueTitle string    `json:"issueTitle,omitempty"`
	CommentID  string    `json:"commentId,omitempty"`
	ThreadID   string    `json:"threadId,omitempty"`
	Author     string    `json:"author,omitempty"`
	AuthorID   string    `json:"authorId,omitempty"`
	Trusted    bool      `json:"trusted"`
	Body       string    `json:"body"`
	URL        string    `json:"url,omitempty"`
	Delivery   string    `json:"delivery,omitempty"`
	ReceivedAt time.Time `json:"receivedAt"`

	AcceptedAt *time.Time `json:"acceptedAt,omitempty"`
	TaskID     string     `json:"taskId,omitempty"`
}

// Task is a piece of work the main session owns: what was asked, where it
// came from, where it stands and what happens next.
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
}

func (t Task) Open() bool { return t.Status != Done && t.Status != Cancelled }

// NewTask describes a task to create.
type NewTask struct {
	Title    string `json:"title"`
	Issue    string `json:"issue,omitempty"`
	ThreadID string `json:"threadId,omitempty"`
	Project  string `json:"project,omitempty"`
}

// TaskUpdate changes a task; empty fields are left as they are.
type TaskUpdate struct {
	Status     string `json:"status,omitempty"`
	NextAction string `json:"nextAction,omitempty"`
	Checkpoint string `json:"checkpoint,omitempty"`
	Result     string `json:"result,omitempty"`
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
	db      *bolt.DB
	trusted map[string]bool
	now     func() time.Time
}

// Open opens (creating if needed) the store at path. Events from authors in
// trusted (Linear user ids) are marked Trusted; with none listed, every
// author is.
func Open(path string, trusted []string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if errors.Is(err, bolt.ErrTimeout) {
		return nil, fmt.Errorf("%s is in use by another mahamantri", path)
	}
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bMeta, bEvents, bTasks, bThreads, bActions} {
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
		default:
			return fmt.Errorf("store schema %q is not supported by this mahamantri (want %s)", v, schemaVersion)
		}
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
	if len(trusted) > 0 {
		s.trusted = map[string]bool{}
		for _, id := range trusted {
			s.trusted[id] = true
		}
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Ingest keeps a ping. A comment already in the inbox - Linear redelivering
// it - is left as it is.
func (s *Store) Ingest(p linear.Ping, delivery string) error {
	id := "linear:" + p.CommentID
	if p.CommentID == "" {
		if delivery == "" {
			return errors.New("ping has neither a comment id nor a delivery id")
		}
		id = "linear-delivery:" + delivery
	}
	_, err := s.ingest(Event{
		ID: id, Source: "linear", Kind: p.Kind,
		Issue: p.Issue, IssueID: p.IssueID, IssueTitle: p.IssueTitle,
		CommentID: p.CommentID, ThreadID: p.ThreadID,
		Author: p.Author, AuthorID: p.AuthorID, Trusted: s.trusted == nil || s.trusted[p.AuthorID],
		Body: truncate(p.Body, maxBody), URL: p.URL, Delivery: delivery,
	})
	return err
}

func (s *Store) ingest(e Event) (bool, error) {
	created := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bEvents)
		if b.Get([]byte(e.ID)) != nil {
			return nil
		}
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		e.Seq, e.ReceivedAt, created = seq, s.now(), true
		return putJSON(b, e.ID, e)
	})
	return created, err
}

// Has reports whether Senapati has written in threadID (linear.ThreadIndex).
func (s *Store) Has(threadID string) bool {
	found := false
	s.db.View(func(tx *bolt.Tx) error {
		found = tx.Bucket(bThreads).Get([]byte(threadID)) != nil
		return nil
	})
	return found
}

// Add records that Senapati wrote in threadID.
func (s *Store) Add(threadID, issue string) error {
	if threadID == "" {
		return errors.New("empty thread id")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bThreads).Put([]byte(threadID), []byte(issue))
	})
}

// ImportThreads adds the threads in a managed-mode thread file (see
// linear.Threads) once; later calls do nothing. A missing file is nothing
// to import.
func (s *Store) ImportThreads(path string) (int, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var legacy map[string]struct {
		Issue string `json:"issue"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	n := 0
	err = s.db.Update(func(tx *bolt.Tx) error {
		meta, threads := tx.Bucket(bMeta), tx.Bucket(bThreads)
		marker := []byte("imported:" + path)
		if meta.Get(marker) != nil {
			return nil
		}
		for id, t := range legacy {
			if threads.Get([]byte(id)) == nil {
				if err := threads.Put([]byte(id), []byte(t.Issue)); err != nil {
					return err
				}
				n++
			}
		}
		return meta.Put(marker, []byte(s.now().Format(time.RFC3339)))
	})
	return n, err
}

// Pending lists the events not yet accepted with Seq above after, oldest
// first.
func (s *Store) Pending(after uint64) ([]Event, error) {
	var out []Event
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bEvents).ForEach(func(_, v []byte) error {
			var e Event
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			if e.AcceptedAt == nil && e.Seq > after {
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

// Accept attaches events to a task - an existing one (taskID) or one created
// for them (create) - and records what happens next, in one transaction.
// Events already accepted keep the task they were accepted into; accepting
// them again is not an error, so a redelivered ping cannot make a second
// task.
func (s *Store) Accept(eventIDs []string, taskID string, create *NewTask, next string) (Task, error) {
	if len(eventIDs) == 0 {
		return Task{}, errors.New("no events to accept")
	}
	if (taskID == "") == (create == nil) {
		return Task{}, errors.New("give either a task id or a new task, not both or neither")
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
		Status: Queued, CreatedAt: now, UpdatedAt: now}
	return t, putJSON(b, t.ID, t)
}

// UpdateTask changes a task's status, next action, checkpoint or result. A
// task is only done with a result saying what was achieved.
func (s *Store) UpdateTask(id string, u TaskUpdate) (Task, error) {
	if u.Status != "" && !taskStates[u.Status] {
		return Task{}, fmt.Errorf("status %q is not one of queued, working, waiting_approval, blocked, paused, done, cancelled", u.Status)
	}
	var t Task
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bTasks)
		if err := getJSON(b, id, &t); err != nil {
			return fmt.Errorf("task %s: %w", id, err)
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
func (s *Store) Intend(a Action) (Action, error) {
	if a.Kind == "" || a.Target == "" || a.Intent == "" {
		return Action{}, errors.New("an action needs a kind, a target and what it will do (intent)")
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		if a.TaskID != "" && tx.Bucket(bTasks).Get([]byte(a.TaskID)) == nil {
			return fmt.Errorf("task %s: %w", a.TaskID, ErrNotFound)
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

// Resolve records how an intended action turned out. When it posted a Linear
// comment, threadID (the thread it is in) is learned at once, so a reply
// there reaches the main session even before Linear's webhook for the
// comment itself arrives.
func (s *Store) Resolve(id, status, ref, note, threadID, issue string) (Action, error) {
	if status != Happened && status != Failed {
		return Action{}, fmt.Errorf("status %q must be %q or %q", status, Happened, Failed)
	}
	var a Action
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bActions)
		if err := getJSON(b, id, &a); err != nil {
			return fmt.Errorf("action %s: %w", id, err)
		}
		a.Status, a.Ref, a.Note, a.UpdatedAt = status, ref, note, s.now()
		if status == Happened && threadID != "" {
			if err := tx.Bucket(bThreads).Put([]byte(threadID), []byte(issue)); err != nil {
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
	return s[:cut] + "\n[... cut; read the rest on Linear]"
}
