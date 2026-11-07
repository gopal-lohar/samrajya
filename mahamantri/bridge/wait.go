package bridge

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Delivery. A session learns of new events through its waiter: a hook the
// plugin starts in the background whenever the session ends a turn
// (Claude Code's asyncRewake). The waiter long-polls /v1/wait; when events
// arrive it is answered with them, exits, and Claude Code wakes the
// session with them as input. The session's next turn ends with a new
// waiter. So an idle session hears of new work at once, and a busy one when
// its turn ends - without Claude Code's channels, which an account's
// policy may switch off.
//
// Each event is handed over once (Agent.DeliveredSeq); it stays pending
// until the session accepts it, and a session that has no waiter while
// events wait for it is relaunched by the supervisor with them in its brief.

// maxWaitPoll bounds one /v1/wait request; the waiter asks again.
const maxWaitPoll = 5 * time.Minute

type waiter struct {
	id   uint64
	stop chan struct{}
}

// wait answers an agent's waiter with the events not yet handed to its
// session, waiting up to the requested time for some. A newer waiter for
// the same agent replaces an older one, which is answered superseded.
func (s *Server) wait(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Agent   string `json:"agent"`
		Secret  string `json:"secret"`
		Seconds int    `json:"seconds"`
	}
	if !decode(w, r, &body) {
		return
	}
	agent, ok := s.hookAgent(body.Agent, body.Secret)
	if !ok {
		writeErr(w, http.StatusForbidden, "unknown agent or stale secret: this session has been replaced or stopped")
		return
	}
	d := min(time.Duration(body.Seconds)*time.Second, maxWaitPoll)
	me := s.addWaiter(agent)
	defer s.dropWaiter(agent, me)
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		wake := s.wake
		s.mu.Unlock()
		text, n, err := s.handOver(agent, body.Secret)
		if err != nil {
			reply(w, nil, err)
			return
		}
		if n > 0 {
			writeJSON(w, http.StatusOK, map[string]any{"events": n, "text": text})
			return
		}
		select {
		case <-wake:
		case <-me.stop:
			writeJSON(w, http.StatusOK, map[string]any{"superseded": true})
			return
		case <-deadline.C:
			writeJSON(w, http.StatusOK, map[string]any{"events": 0})
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) addWaiter(agent string) *waiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	if old := s.waiters[agent]; old != nil {
		close(old.stop)
	}
	s.seq++
	me := &waiter{id: s.seq, stop: make(chan struct{})}
	s.waiters[agent] = me
	return me
}

func (s *Server) dropWaiter(agent string, me *waiter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waiters[agent] == me {
		delete(s.waiters, agent)
	}
	s.lastWaiter[agent] = s.now()
}

// Listening reports whether agent's session has a waiter, or had one in the
// last few seconds (between two polls).
func (s *Server) Listening(agent string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	return s.waiters[agent] != nil || s.now().Sub(s.lastWaiter[agent]) < 15*time.Second
}

// handOver takes the agent's events not yet handed to its session (under
// the launch whose secret the waiter has), marks them handed over and the
// session in a turn, and renders them.
func (s *Server) handOver(agent, secret string) (string, int, error) {
	a, err := s.Store.Agent(agent)
	if err != nil {
		return "", 0, err
	}
	pending, err := s.Store.PendingFor(agent, max(a.DeliveredSeq, a.BriefSeq))
	if err != nil || len(pending) == 0 {
		return "", 0, err
	}
	last := pending[len(pending)-1].Seq
	_, err = s.Store.UpdateAgent(agent, func(x *Agent) error {
		if x.Secret != secret {
			return fmt.Errorf("%w: this session has been replaced", ErrForbidden)
		}
		if x.DeliveredSeq >= last {
			return errHanded
		}
		x.DeliveredSeq, x.InTurn, x.TurnAt = last, true, s.now()
		return nil
	})
	if err == errHanded {
		return "", 0, nil // another waiter took them
	}
	if err != nil {
		return "", 0, err
	}
	var parts []string
	for _, e := range pending {
		parts = append(parts, RenderForAgent(e))
	}
	return strings.Join(parts, "\n\n"), len(pending), nil
}

var errHanded = fmt.Errorf("already handed over")

// RenderForAgent is an event as a session reads it when it is handed over:
// what it is, and how to accept and answer it.
func RenderForAgent(e Event) string {
	text := RenderEvent(e)
	if e.Source == "samrajya" {
		return text + fmt.Sprintf("\nAcknowledge it with samrajya_accept (event_id %s); it needs no task.", e.ID)
	}
	thread := e.ThreadID
	if thread == "" {
		thread = "(none)"
	}
	return text + fmt.Sprintf("\nAccept it with samrajya_accept (event_id %s) before working on it; answer with samrajya_reply in conversation %s, thread %s.",
		e.ID, e.Conversation, thread)
}

var _ = context.Background
