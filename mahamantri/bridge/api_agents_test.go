package bridge

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func agentsServer(t *testing.T) (*Server, string, *fakeLinear) {
	t.Helper()
	s, fake, c := linearServer(t)
	s.Agents = true
	s.Store.RouteToWorkers(true)
	s.Store.trusted = map[string]bool{"u-asha": true}
	if _, err := s.Store.EnsureMain(); err != nil {
		t.Fatal(err)
	}
	return s, c.url, fake
}

// attachAs launches agent id in the store and attaches a client with its
// secret, as the plugin does.
func attachAs(t *testing.T, s *Server, url, id string) *client {
	t.Helper()
	a, err := s.Store.Launching(id, "samrajya")
	if err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, url: url, token: "tok"}
	code, out := c.do("POST", "/v1/attach", map[string]any{"client": "t", "agent": id, "secret": a.Secret, "protocol": ProtocolVersion})
	if code != http.StatusOK {
		t.Fatalf("attach %s: %d %v", id, code, out)
	}
	c.attachment = out["attachment"].(string)
	return c
}

func events(t *testing.T, c *client) []string {
	t.Helper()
	code, out := c.do("GET", "/v1/events?after=0&wait=0", nil)
	if code != http.StatusOK {
		t.Fatalf("events: %d %v", code, out)
	}
	var ids []string
	for _, e := range out["events"].([]any) {
		ids = append(ids, e.(map[string]any)["id"].(string))
	}
	return ids
}

// Acceptance 12: in agents mode a session attaches only as an agent, with
// its launch's secret.
func TestAgentsModeAttachesOnlyWithTheAgentsSecret(t *testing.T) {
	s, url, _ := agentsServer(t)
	anon := &client{t: t, url: url, token: "tok"}
	if code := anon.attach(true); code != http.StatusConflict {
		t.Errorf("anonymous takeover: %d", code)
	}
	if code, _ := anon.do("POST", "/v1/attach", map[string]any{"agent": "main", "secret": "guess", "protocol": ProtocolVersion}); code != http.StatusForbidden {
		t.Errorf("forged secret: %d", code)
	}
	a, _ := s.Store.Launching(MainAgent, "samrajya")
	if code, _ := anon.do("POST", "/v1/attach", map[string]any{"agent": "main", "secret": a.Secret}); code != http.StatusUpgradeRequired {
		t.Errorf("old plugin protocol: %d", code)
	}
	// Status is readable without attaching.
	if code, _ := anon.do("GET", "/v1/status", nil); code != http.StatusOK {
		t.Errorf("status: %d", code)
	}
	main := attachAs(t, s, url, MainAgent)
	if !s.Live() {
		t.Error("main not live after attaching")
	}
	if a, _ := s.Store.Agent(MainAgent); a.State != AgentLive {
		t.Errorf("state %s", a.State)
	}
	_ = main
}

// Acceptance 7: relaunching an agent fences out its earlier session.
func TestARelaunchFencesOutTheOldSession(t *testing.T) {
	s, url, _ := agentsServer(t)
	s.Ingest(issuePing("ENG-1", "uuid-1", "c1", "c1"), "")
	e, _ := s.Store.Event("linear:c1")
	old := attachAs(t, s, url, e.Owner())
	oldSecret, _ := s.Store.Agent(e.Owner())
	if _, err := s.Store.Launching(e.Owner(), "samrajya"); err != nil {
		t.Fatal(err)
	}
	if code, _ := old.do("GET", "/v1/events?after=0&wait=0", nil); code != http.StatusConflict {
		t.Errorf("fenced session read its feed: %d", code)
	}
	if code, _ := old.do("POST", "/v1/report", map[string]string{"kind": "progress", "body": "x"}); code != http.StatusConflict {
		t.Errorf("fenced session acted: %d", code)
	}
	if code, _ := old.do("POST", "/v1/attach", map[string]any{"agent": e.Owner(), "secret": oldSecret.Secret, "protocol": ProtocolVersion}); code != http.StatusForbidden {
		t.Errorf("fenced session reattached: %d", code)
	}
}

// Acceptances 1, 6 and 11 over the API: a worker's feed holds only its own
// events (less those its launch brief carried); it cannot dispatch, accept
// or reply for another task; the main and workers can message each other.
func TestWorkersSeeAndTouchOnlyTheirOwnWork(t *testing.T) {
	s, url, fake := agentsServer(t)
	main := attachAs(t, s, url, MainAgent)
	s.Ingest(issuePing("ENG-1", "uuid-1", "c1", "c1"), "")
	s.Ingest(issuePing("ENG-2", "uuid-2", "c2", "c2"), "")
	e1, _ := s.Store.Event("linear:c1")
	e2, _ := s.Store.Event("linear:c2")
	w1 := attachAs(t, s, url, e1.Owner()) // its brief carried c1

	if got := events(t, w1); len(got) != 0 {
		t.Errorf("worker feed repeats its brief: %v", got)
	}
	s.Ingest(issuePing("ENG-1", "uuid-1", "c1b", "c1"), "")
	if got := events(t, w1); len(got) != 1 || got[0] != "linear:c1b" {
		t.Errorf("worker feed %v", got)
	}
	if got := events(t, main); len(got) != 0 {
		t.Errorf("main got workers' events: %v", got)
	}

	if code, _ := w1.do("POST", "/v1/accept", map[string]any{"events": []string{e2.ID}, "taskId": e1.TaskID, "nextAction": "x"}); code != http.StatusForbidden {
		t.Errorf("accepting another worker's event: %d", code)
	}
	if code, _ := w1.do("POST", "/v1/reply", map[string]any{"issue": "ENG-2", "body": "hi"}); code != http.StatusForbidden {
		t.Errorf("replying on another issue: %d", code)
	}
	if code, out := w1.do("POST", "/v1/reply", map[string]any{"threadId": "c1", "body": "on it"}); code != http.StatusOK {
		t.Errorf("replying on its own issue: %d %v", code, out)
	}
	if _, _, comments := fake.got(); len(comments) != 1 || !strings.HasPrefix(comments[0], "ENG-1 c1 ") {
		t.Errorf("posted %v", comments)
	}
	if code, _ := w1.do("POST", "/v1/dispatch", map[string]any{"brief": "x"}); code != http.StatusForbidden {
		t.Errorf("worker dispatching: %d", code)
	}
	if code, _ := w1.do("POST", "/v1/tasks", map[string]any{"title": "x"}); code != http.StatusForbidden {
		t.Errorf("worker creating a task: %d", code)
	}

	if code, _ := main.do("POST", "/v1/message", map[string]any{"to": e1.Owner(), "body": "check mobile too"}); code != http.StatusOK {
		t.Errorf("main messaging a worker: %d", code)
	}
	if got := events(t, w1); len(got) != 2 {
		t.Errorf("worker feed after message %v", got)
	}
	if code, _ := w1.do("POST", "/v1/report", map[string]any{"kind": "needs_decision", "body": "A or B?"}); code != http.StatusOK {
		t.Errorf("report: %d", code)
	}
	if got := events(t, main); len(got) != 1 {
		t.Errorf("main feed after a decision report %v", got)
	}
	code, out := main.do("POST", "/v1/dispatch", map[string]any{"title": "Write the release notes", "brief": "for v2"})
	if code != http.StatusOK || out["agent"] == "" {
		t.Errorf("dispatch: %d %v", code, out)
	}
}

func TestHooksActAsTheirAgent(t *testing.T) {
	s, url, _ := agentsServer(t)
	s.Ingest(issuePing("ENG-1", "uuid-1", "c1", "c1"), "")
	e, _ := s.Store.Event("linear:c1")
	a, _ := s.Store.Launching(e.Owner(), "samrajya")
	hook := &client{t: t, url: url, token: "tok"}
	if code, _ := hook.do("POST", "/v1/turn", map[string]any{"agent": a.ID, "secret": "wrong", "state": "start"}); code != http.StatusForbidden {
		t.Errorf("forged turn: %d", code)
	}
	if code, _ := hook.do("POST", "/v1/turn", map[string]any{"agent": a.ID, "secret": a.Secret, "state": "start"}); code != http.StatusOK {
		t.Errorf("turn: %d", code)
	}
	if got, _ := s.Store.Agent(a.ID); !got.InTurn {
		t.Error("turn start not recorded")
	}
	if code, _ := hook.do("POST", "/v1/limit", map[string]any{"agent": a.ID, "secret": a.Secret, "message": "limit · resets 7pm (UTC)"}); code != http.StatusOK {
		t.Errorf("limit: %d", code)
	}
	if s.limitedUntil().IsZero() {
		t.Error("limit not recorded")
	}
}

// Found in the sandbox: a session ending (its plugin detaching) during a
// usage limit ended the limit, and the main was relaunched into it.
func TestADetachDoesNotEndTheLimit(t *testing.T) {
	s, url, _ := agentsServer(t)
	main := attachAs(t, s, url, MainAgent)
	s.setLimited(time.Now().Add(time.Hour))
	s.Store.SetMeta(limitSetKey, time.Now().Add(-time.Hour).Format(time.RFC3339))
	main.do("POST", "/v1/detach", nil)
	if s.limitedUntil().IsZero() {
		t.Error("a detach ended the limit")
	}
}
