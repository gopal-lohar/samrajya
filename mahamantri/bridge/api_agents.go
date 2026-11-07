package bridge

import (
	"crypto/subtle"
	"net/http"
	"time"
)

// hookAgent identifies the agent behind a hook request (turn start/end, a
// usage limit): hooks run outside the plugin, so they carry the agent id and
// secret from the session's environment rather than an attachment.
func (s *Server) hookAgent(agent, secret string) (string, bool) {
	if agent == "" || secret == "" {
		return "", false
	}
	a, err := s.Store.Agent(agent)
	if err != nil || a.Secret == "" || subtle.ConstantTimeCompare([]byte(a.Secret), []byte(secret)) != 1 {
		return "", false
	}
	return a.ID, true
}

// turn records that an agent's session started or ended a turn (its
// UserPromptSubmit and Stop hooks), so the supervisor can tell a session
// working on a long step from a stuck or idle one.
func (s *Server) turn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Agent  string `json:"agent"`
		Secret string `json:"secret"`
		State  string `json:"state"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.State != "start" && body.State != "end" {
		writeErr(w, http.StatusBadRequest, `state must be "start" or "end"`)
		return
	}
	agent, ok := s.hookAgent(body.Agent, body.Secret)
	if !ok {
		writeErr(w, http.StatusForbidden, "unknown agent or stale secret")
		return
	}
	_, err := s.Store.UpdateAgent(agent, func(a *Agent) error {
		a.InTurn, a.TurnAt = body.State == "start", s.now()
		return nil
	})
	reply(w, map[string]any{"ok": true}, err)
}

// agents lists the agents and how each is doing, for the main's overview.
func (s *Server) agents(w http.ResponseWriter) {
	list, err := s.Store.Agents()
	if err != nil {
		reply(w, nil, err)
		return
	}
	type view struct {
		Agent
		Attached bool  `json:"attached"`
		Task     *Task `json:"task,omitempty"`
	}
	out := make([]view, 0, len(list))
	for _, a := range list {
		a.Secret = ""
		v := view{Agent: a, Attached: s.AgentLive(a.ID)}
		if a.TaskID != "" {
			if t, err := s.Store.Task(a.TaskID); err == nil {
				v.Task = &t
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
}

// dispatch gives work to a worker of its own (the main only).
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, _ string) {
	if s.Supervisor == nil {
		writeErr(w, http.StatusNotImplemented, "this mahamantri launches no workers (mode opencode): do the work in this session")
		return
	}
	var body struct {
		Title        string   `json:"title"`
		Brief        string   `json:"brief"`
		Project      string   `json:"project"`
		Conversation string   `json:"conversation"`
		ThreadID     string   `json:"threadId"`
		Route        *bool    `json:"route"`
		Links        []string `json:"links"`
	}
	if !decode(w, r, &body) {
		return
	}
	var err error
	if body.Conversation, err = s.canonical(r.Context(), body.Conversation); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// By default a conversation is routed to the worker when its surface
	// starts work by itself (an issue is a piece of work), and only
	// reported in otherwise (the person's chat stays with the main).
	route := body.Conversation != "" && s.Store.startsWork(SurfaceOf(body.Conversation))
	if body.Route != nil {
		route = *body.Route
	}
	t, err := s.Store.Dispatch(NewTask{Title: body.Title, ThreadID: body.ThreadID, Project: body.Project, Links: body.Links},
		body.Conversation, route, body.Brief)
	if err == nil {
		s.Wake()
	}
	reply(w, t, err)
}

// message leaves text for another agent: from the main to any agent, from
// a worker to the main.
func (s *Server) message(w http.ResponseWriter, r *http.Request, agent string) {
	var body struct {
		To   string `json:"to"`
		Body string `json:"body"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.To == "" {
		body.To = MainAgent
	}
	if agent != MainAgent && body.To != MainAgent {
		writeErr(w, http.StatusForbidden, "a worker messages only the main")
		return
	}
	e, err := s.Store.Message(agent, body.To, body.Body)
	if err == nil {
		s.Wake()
	}
	reply(w, map[string]any{"eventId": e.ID, "to": body.To}, err)
}

// report records a worker's report on its task (see Store.Report).
func (s *Server) report(w http.ResponseWriter, r *http.Request, agent string) {
	var body struct {
		Kind string `json:"kind"`
		Body string `json:"body"`
	}
	if !decode(w, r, &body) {
		return
	}
	err := s.Store.Report(agent, body.Kind, body.Body)
	if err == nil {
		s.Wake()
	}
	reply(w, map[string]any{"reported": body.Kind}, err)
}

// limitProofDelay: a tool call this long after a usage limit was reported
// shows turns run again. A call sooner may come from a turn that started
// before the limit, so it proves nothing.
const limitProofDelay = time.Minute

const limitSetKey = "limit.set"

// clearLimitOnActivity ends a recorded usage limit when an agent calls a
// tool well after it was reported.
func (s *Server) clearLimitOnActivity() {
	if s.limitedUntil().IsZero() {
		return
	}
	set, err := time.Parse(time.RFC3339, s.Store.Meta(limitSetKey))
	if err == nil && s.now().Sub(set) < limitProofDelay {
		return
	}
	s.setLimited(time.Time{})
}
