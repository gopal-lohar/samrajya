package bridge

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Agents are the sessions Mahamantri runs (docs/design/agents.md): one main,
// which talks to the person, and a worker per dispatched task. The store records which
// agent owns every event and task; the supervisor makes the agents exist.

// MainAgent is the main session's agent id.
const MainAgent = "main"

// Agent roles.
const (
	RoleMain   = "main"
	RoleWorker = "worker"
)

// Agent states.
const (
	// AgentIdle: not running; started when it has work.
	AgentIdle = "idle"
	// AgentStarting: launched under its current generation, not yet attached.
	AgentStarting = "starting"
	// AgentLive: attached under its current generation.
	AgentLive = "live"
	// AgentFailed: could not be started; its task is blocked until a person
	// or the main intervenes.
	AgentFailed = "failed"
)

// Report kinds an agent sends the main.
const (
	ReportProgress = "progress"
	ReportDone     = "done"
	ReportBlocked  = "blocked"
	ReportDecision = "needs_decision"
)

// Agent is a session Mahamantri runs.
type Agent struct {
	ID     string `json:"id"`
	Role   string `json:"role"`
	TaskID string `json:"taskId,omitempty"`
	State  string `json:"state"`
	// Gen counts launches. A session may act only under the agent's current
	// generation, so a replaced session is fenced out before its successor
	// starts.
	Gen uint64 `json:"gen"`
	// Secret is what the agent's session attaches with (from a 0600 file
	// the launcher names).
	Secret string `json:"secret,omitempty"`
	// Session is the runtime's id for the current session, Name its name.
	Session string `json:"session,omitempty"`
	Name    string `json:"name,omitempty"`
	// BriefSeq: events up to this sequence were given in the launch brief,
	// so the feed does not send them again.
	BriefSeq uint64 `json:"briefSeq,omitempty"`

	CreatedAt  time.Time `json:"createdAt"`
	LaunchedAt time.Time `json:"launchedAt,omitempty"`
	AttachedAt time.Time `json:"attachedAt,omitempty"`
	StoppedAt  time.Time `json:"stoppedAt,omitempty"`

	// InTurn is whether its session is in a turn (from its hooks), since
	// TurnAt; when not, TurnAt is when the last turn ended.
	InTurn bool      `json:"inTurn,omitempty"`
	TurnAt time.Time `json:"turnAt,omitempty"`

	Failures int       `json:"failures,omitempty"`
	RetryAt  time.Time `json:"retryAt,omitempty"`
	Error    string    `json:"error,omitempty"`

	Report   string    `json:"report,omitempty"`
	ReportAt time.Time `json:"reportAt,omitempty"`
	// StuckNoted is the turn start already reported as stuck.
	StuckNoted time.Time `json:"stuckNoted,omitempty"`
	// ResumedAt is when it was last told to resume after a usage limit.
	ResumedAt time.Time `json:"resumedAt,omitempty"`
}

// EnsureMain records the main agent if it is not yet.
func (s *Store) EnsureMain() (Agent, error) {
	var a Agent
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bAgents)
		if err := getJSON(b, MainAgent, &a); err == nil {
			return nil
		}
		a = Agent{ID: MainAgent, Role: RoleMain, State: AgentIdle, CreatedAt: s.now()}
		return putJSON(b, a.ID, a)
	})
	return a, err
}

// Agent returns one agent.
func (s *Store) Agent(id string) (Agent, error) {
	var a Agent
	err := s.db.View(func(tx *bolt.Tx) error { return getJSON(tx.Bucket(bAgents), id, &a) })
	return a, err
}

// Agents lists every agent, main first, then by id.
func (s *Store) Agents() ([]Agent, error) {
	var out []Agent
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bAgents).ForEach(func(_, v []byte) error {
			var a Agent
			if err := json.Unmarshal(v, &a); err != nil {
				return err
			}
			out = append(out, a)
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool {
		if (out[i].ID == MainAgent) != (out[j].ID == MainAgent) {
			return out[i].ID == MainAgent
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt) || out[i].CreatedAt.Equal(out[j].CreatedAt) && out[i].ID < out[j].ID
	})
	return out, err
}

// UpdateAgent changes an agent in one transaction.
func (s *Store) UpdateAgent(id string, f func(*Agent) error) (Agent, error) {
	var a Agent
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bAgents)
		if err := getJSON(b, id, &a); err != nil {
			return fmt.Errorf("agent %s: %w", id, err)
		}
		if err := f(&a); err != nil {
			return err
		}
		return putJSON(b, a.ID, a)
	})
	return a, err
}

// Launching fences out the agent's current session and records a new launch:
// a new generation and secret, state starting, and in BriefSeq the last of
// its pending events, which its launch brief carries. It returns the agent
// as it must be launched, named <prefix>-<id>-g<gen>.
func (s *Store) Launching(id, prefix string) (Agent, error) {
	secret, err := newSecret()
	if err != nil {
		return Agent{}, err
	}
	var a Agent
	err = s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bAgents)
		if err := getJSON(b, id, &a); err != nil {
			return fmt.Errorf("agent %s: %w", id, err)
		}
		a.BriefSeq = 0
		err := tx.Bucket(bEvents).ForEach(func(_, v []byte) error {
			var e Event
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			if e.AcceptedAt == nil && e.Owner() == id && e.Seq > a.BriefSeq {
				a.BriefSeq = e.Seq
			}
			return nil
		})
		if err != nil {
			return err
		}
		a.Gen++
		a.Secret, a.State, a.Session, a.Error = secret, AgentStarting, "", ""
		a.Name = fmt.Sprintf("%s-%s-g%d", prefix, a.ID, a.Gen)
		a.LaunchedAt, a.AttachedAt, a.InTurn, a.TurnAt = s.now(), time.Time{}, false, time.Time{}
		return putJSON(b, a.ID, a)
	})
	return a, err
}

func newSecret() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// TaskForConversation returns the task a conversation is linked to.
func (s *Store) TaskForConversation(conv string) (Task, error) {
	var t Task
	err := s.db.View(func(tx *bolt.Tx) error {
		id := tx.Bucket(bConvs).Get([]byte(conv))
		if id == nil {
			return ErrNotFound
		}
		return getJSON(tx.Bucket(bTasks), string(id), &t)
	})
	return t, err
}

// Dispatch gives work to a worker of its own: a new task, with the main's
// brief as its first event. With conv, the worker may reply there; with
// route too, conv's new messages go to the worker, and when conv is already
// routed to a task the work goes to that task (and its worker) instead.
func (s *Store) Dispatch(n NewTask, conv string, route bool, brief string) (Task, error) {
	if strings.TrimSpace(brief) == "" {
		return Task{}, errors.New("say what the worker is to do (brief)")
	}
	if conv != "" && !strings.Contains(conv, ":") {
		return Task{}, fmt.Errorf("conversation %q is not <surface>:<id>", conv)
	}
	if route && conv == "" {
		return Task{}, errors.New("route needs a conversation")
	}
	var t Task
	err := s.db.Update(func(tx *bolt.Tx) error {
		tasks := tx.Bucket(bTasks)
		found := false
		if route {
			if id := tx.Bucket(bConvs).Get([]byte(conv)); id != nil {
				if err := getJSON(tasks, string(id), &t); err != nil {
					return err
				}
				found = true
			}
		}
		if !found {
			if strings.TrimSpace(n.Title) == "" {
				n.Title = firstLine(brief, 80)
			}
			var err error
			if t, err = newTask(tasks, n, s.now()); err != nil {
				return err
			}
		}
		if t.Agent == "" || t.Agent == MainAgent {
			if err := s.assignWorker(tx, &t, conv, route); err != nil {
				return err
			}
		} else if conv != "" && !found {
			if err := s.linkConversation(tx, &t, conv, route); err != nil {
				return err
			}
		}
		if !t.Open() {
			t.Status = Queued
		}
		t.UpdatedAt = s.now()
		if err := putJSON(tasks, t.ID, t); err != nil {
			return err
		}
		where := ""
		if conv != "" {
			where = "\nReport in conversation " + conv
			if n.ThreadID != "" {
				where += ", thread " + n.ThreadID
			}
			where += " (samrajya_reply)."
		}
		return s.putNotice(tx, t.Agent, fmt.Sprintf("dispatch-%s-%d", t.ID, s.now().UnixNano()), "dispatch",
			"From the main session, for task "+t.ID+" ("+t.Title+"):\n"+brief+where)
	})
	return t, err
}

func firstLine(s string, max int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > max {
		s = strings.TrimSpace(s[:max]) + "..."
	}
	return s
}

// Message leaves text from one agent for another, delivered on its feed.
func (s *Store) Message(from, to, body string) (Event, error) {
	if strings.TrimSpace(body) == "" {
		return Event{}, errors.New("empty message")
	}
	var e Event
	err := s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(bAgents).Get([]byte(to)) == nil {
			return fmt.Errorf("agent %s: %w", to, ErrNotFound)
		}
		e = Event{ID: fmt.Sprintf("samrajya:msg-%s-%d", from, s.now().UnixNano()), Source: "samrajya", Kind: "message",
			Author: from, Trusted: true, Body: body, Agent: to}
		return s.putEvent(tx, &e)
	})
	return e, err
}

// Report records what a worker says about its task. Blocked and
// needs-decision reports reach the main as events; the rest are state the
// main reads with Agents.
func (s *Store) Report(agent, kind, body string) error {
	switch kind {
	case ReportProgress, ReportDone, ReportBlocked, ReportDecision:
	default:
		return fmt.Errorf("report kind %q is not one of progress, done, blocked, needs_decision", kind)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bAgents)
		var a Agent
		if err := getJSON(b, agent, &a); err != nil {
			return fmt.Errorf("agent %s: %w", agent, err)
		}
		a.Report, a.ReportAt = kind+": "+truncate(body, 2000), s.now()
		if err := putJSON(b, a.ID, a); err != nil {
			return err
		}
		if kind == ReportBlocked || kind == ReportDecision {
			what := map[string]string{ReportBlocked: "is blocked", ReportDecision: "needs a decision"}[kind]
			return s.putNotice(tx, MainAgent, fmt.Sprintf("report-%s-%d", agent, s.now().UnixNano()), "report",
				fmt.Sprintf("Worker %s (task %s) %s:\n%s", agent, a.TaskID, what, body))
		}
		return nil
	})
}

func (s *Store) putNotice(tx *bolt.Tx, agent, id, kind, body string) error {
	e := Event{ID: "samrajya:" + id, Source: "samrajya", Kind: kind, Trusted: true, Body: body, Agent: agent}
	return s.putEvent(tx, &e)
}

func (s *Store) putEvent(tx *bolt.Tx, e *Event) error {
	b := tx.Bucket(bEvents)
	if b.Get([]byte(e.ID)) != nil {
		return nil
	}
	seq, err := b.NextSequence()
	if err != nil {
		return err
	}
	e.Seq, e.ReceivedAt = seq, s.now()
	return putJSON(b, e.ID, *e)
}

// MayReply decides where agent may reply: the main in any conversation,
// a worker only in its task's conversations (routed or reportTo). An empty
// conv is the worker's own first conversation. It returns the conversation
// and the worker's task.
func (s *Store) MayReply(agent, conv string) (string, Task, error) {
	if agent == MainAgent {
		if conv == "" {
			return "", Task{}, errors.New("say which conversation to reply in")
		}
		return conv, Task{}, nil
	}
	a, err := s.Agent(agent)
	if err != nil {
		return "", Task{}, err
	}
	t, err := s.Task(a.TaskID)
	if err != nil {
		return "", Task{}, err
	}
	mine := append(append([]string(nil), t.Conversations...), t.ReportTo...)
	if conv == "" {
		if len(mine) == 0 {
			return "", t, fmt.Errorf("%w: task %s has no conversation to reply in; tell the main session (samrajya_report)", ErrForbidden, t.ID)
		}
		return mine[0], t, nil
	}
	if slices.Contains(mine, conv) {
		return conv, t, nil
	}
	return "", t, fmt.Errorf("%w: worker %s replies only in its task's conversations (%s)", ErrForbidden, agent, strings.Join(mine, ", "))
}

// FixMainSecret gives the main agent a secret that does not change between
// launches, for a main session mahamantri does not launch itself
// (opencode), and returns it.
func (s *Store) FixMainSecret() (string, error) {
	if _, err := s.EnsureMain(); err != nil {
		return "", err
	}
	a, err := s.UpdateAgent(MainAgent, func(a *Agent) error {
		if a.Secret == "" {
			secret, err := newSecret()
			if err != nil {
				return err
			}
			a.Secret, a.Gen = secret, a.Gen+1
		}
		return nil
	})
	return a.Secret, err
}

// AgentRecovery is Recovery for one agent: its pending events, its open
// tasks, and its unresolved actions.
func (s *Store) AgentRecovery(agent string) (Recovery, error) {
	r, err := s.Recovery()
	if err != nil || agent == MainAgent {
		return r, err
	}
	var out Recovery
	for _, e := range r.Pending {
		if e.Owner() == agent {
			out.Pending = append(out.Pending, e)
		}
	}
	mine := map[string]bool{}
	for _, t := range r.Open {
		if t.Owner() == agent {
			out.Open = append(out.Open, t)
			mine[t.ID] = true
		}
	}
	for _, a := range r.Uncertain {
		if mine[a.TaskID] {
			out.Uncertain = append(out.Uncertain, a)
		}
	}
	out.Pending, out.Open, out.Uncertain = nonNil(out.Pending), nonNil(out.Open), nonNil(out.Uncertain)
	return out, nil
}
