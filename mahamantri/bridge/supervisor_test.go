package bridge

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeLauncher struct {
	mu       sync.Mutex
	running  map[string]Session // by name
	launches []string           // names
	briefs   map[string]string  // by name
	stops    []string
	fail     error
	listErr  error
	n        int
}

func (f *fakeLauncher) Launch(_ context.Context, a Agent, brief string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return "", f.fail
	}
	f.n++
	id := fmt.Sprintf("s%d", f.n)
	f.running[a.Name] = Session{ID: id, Name: a.Name}
	f.launches = append(f.launches, a.Name)
	f.briefs[a.Name] = brief
	return id, nil
}

func (f *fakeLauncher) Stop(_ context.Context, s Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, s.Name)
	delete(f.running, s.Name)
	return nil
}

func (f *fakeLauncher) Running(context.Context) ([]Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []Session
	for _, s := range f.running {
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeLauncher) launched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.launches...)
}

type rig struct {
	t     *testing.T
	s     *Server
	sv    *Supervisor
	l     *fakeLauncher
	fake  *fakeLinear
	url   string
	now   time.Time
	cli   map[string]*client
	clock sync.Mutex
}

func newRig(t *testing.T) *rig {
	t.Helper()
	s, url, fake := agentsServer(t)
	r := &rig{t: t, s: s, url: url, fake: fake, now: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC), cli: map[string]*client{}}
	clock := func() time.Time { r.clock.Lock(); defer r.clock.Unlock(); return r.now }
	s.now, s.Store.now = clock, clock
	r.l = &fakeLauncher{running: map[string]Session{}, briefs: map[string]string{}}
	r.sv = &Supervisor{Store: s.Store, Server: s, Launcher: r.l, Logger: log.New(io.Discard, "", 0), MaxWorkers: 2, now: clock}
	s.Supervisor = r.sv
	return r
}

func (r *rig) advance(d time.Duration) {
	r.clock.Lock()
	r.now = r.now.Add(d)
	r.clock.Unlock()
}

func (r *rig) tick() { r.sv.Tick(context.Background()) }

// attach attaches agent id's current launch, as its plugin would.
func (r *rig) attach(id string) *client {
	r.t.Helper()
	a, _ := r.s.Store.Agent(id)
	c := &client{t: r.t, url: r.url, token: "tok"}
	code, out := c.do("POST", "/v1/attach", map[string]any{"agent": id, "secret": a.Secret, "protocol": ProtocolVersion})
	if code != http.StatusOK {
		r.t.Fatalf("attach %s: %d %v", id, code, out)
	}
	c.attachment = out["attachment"].(string)
	r.cli[id] = c
	return c
}

// keepAlive refreshes every attachment, as polling plugins do.
func (r *rig) keepAlive() {
	for _, c := range r.cli {
		c.do("POST", "/v1/turn", nil) // any request through withAgent would do; this one is cheap
		c.do("GET", "/v1/events?after=999999&wait=0", nil)
	}
}

func (r *rig) ping(issue, comment string) string {
	r.s.Ingest(issuePing(issue, "uuid-"+issue, comment, comment), "")
	e, _ := r.s.Store.Event("linear:" + comment)
	return e.Owner()
}

// Acceptances 1 and 6: the main and up to MaxWorkers workers start, with
// briefs carrying their work; the rest wait and start as slots free.
func TestSupervisorLaunchesUpToTheCapAndQueuesTheRest(t *testing.T) {
	r := newRig(t)
	w1, w2, w3 := r.ping("ENG-1", "c1"), r.ping("ENG-2", "c2"), r.ping("ENG-3", "c3")
	r.tick()
	got := r.l.launched()
	if len(got) != 3 || got[0] != "samrajya-main-g1" || got[1] != "samrajya-"+w1+"-g1" || got[2] != "samrajya-"+w2+"-g1" {
		t.Fatalf("launched %v", got)
	}
	if b := r.l.briefs["samrajya-"+w1+"-g1"]; !strings.Contains(b, "linear:c1") || !strings.Contains(b, "ENG-1") {
		t.Errorf("brief %q", b)
	}
	if n, q := r.sv.Queued(w3); !q || n != 0 {
		t.Errorf("queued %d %v", n, q)
	}
	r.tick()
	if len(r.l.launched()) != 3 {
		t.Error("launched past the cap or twice")
	}
	// w1 attaches, finishes and goes idle: it is stopped and w3 starts.
	r.attach(MainAgent)
	c1 := r.attach(w1)
	r.attach(w2)
	a1, _ := r.s.Store.Agent(w1)
	c1.do("POST", "/v1/accept", map[string]any{"events": []string{"linear:c1"}, "taskId": a1.TaskID, "nextAction": "fix"})
	if code, out := c1.do("POST", "/v1/tasks/"+a1.TaskID, map[string]any{"status": "done", "result": "fixed; tested"}); code != http.StatusOK {
		t.Fatalf("done: %d %v", code, out)
	}
	r.advance(31 * time.Minute)
	r.keepAlive()
	r.tick()
	got = r.l.launched()
	if len(got) != 4 || got[3] != "samrajya-"+w3+"-g1" {
		t.Errorf("after w1 went idle: launched %v, stopped %v", got, r.l.stops)
	}
	if a, _ := r.s.Store.Agent(w1); a.State != AgentIdle {
		t.Errorf("w1 state %s", a.State)
	}
	// A new ping on w1's issue starts it again, from its task.
	r.ping("ENG-1", "c1b")
	r.cli[w3] = r.attach(w3)
	r.s.Store.AcceptAs(w3, []string{"linear:c3"}, mustAgent(t, r, w3).TaskID, nil, "x")
	r.s.Store.UpdateTaskAs(MainAgent, mustAgent(t, r, w3).TaskID, TaskUpdate{Status: Done, Result: "x"})
	r.advance(31 * time.Minute)
	r.keepAlive()
	r.tick()
	if got := r.l.launched(); got[len(got)-1] != "samrajya-"+w1+"-g2" {
		t.Errorf("w1 not relaunched for its follow-up: %v", got)
	}
}

func mustAgent(t *testing.T, r *rig, id string) Agent {
	t.Helper()
	a, err := r.s.Store.Agent(id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Acceptance 9: a launch that never attaches is retried with backoff and
// given up on after three tries, blocking the task and telling the person.
func TestSupervisorGivesUpOnALaunchThatNeverAttaches(t *testing.T) {
	r := newRig(t)
	w := r.ping("ENG-1", "c1")
	r.tick()
	r.attach(MainAgent)
	for try := 1; try <= 3; try++ {
		r.advance(6 * time.Minute)
		r.keepAlive()
		r.tick() // times out; retried after backoff
		r.advance(time.Duration(1<<try) * time.Minute)
		r.keepAlive()
		r.tick()
	}
	launches := 0
	for _, n := range r.l.launched() {
		if strings.Contains(n, w) {
			launches++
		}
	}
	if launches != 3 {
		t.Errorf("%d launches of %s: %v", launches, w, r.l.launched())
	}
	a := mustAgent(t, r, w)
	if a.State != AgentFailed {
		t.Errorf("state %s", a.State)
	}
	if task, _ := r.s.Store.Task(a.TaskID); task.Status != Blocked {
		t.Errorf("task %s", task.Status)
	}
	if _, _, cm := r.fake.got(); len(cm) != 1 || !strings.Contains(cm[0], "couldn't start") {
		t.Errorf("person told %v", cm)
	}
	if len(r.l.running) > 1 { // only the main
		t.Errorf("left running %v", r.l.running)
	}
	// A new comment makes it try again.
	r.advance(time.Minute)
	r.ping("ENG-1", "c1b")
	r.keepAlive()
	r.tick()
	r.tick()
	if a := mustAgent(t, r, w); a.State != AgentStarting {
		t.Errorf("after a new comment: %s", a.State)
	}
}

// Acceptance 8: a session launched just before Mahamantri restarted is
// adopted, not duplicated; one from an older launch is stopped.
func TestSupervisorAdoptsAndStopsWhatItFindsRunning(t *testing.T) {
	r := newRig(t)
	w := r.ping("ENG-1", "c1")
	a, _ := r.s.Store.Launching(w, "samrajya") // recorded, then "crashed" before Launch returned
	r.l.running[a.Name] = Session{ID: "orphan", Name: a.Name}
	r.l.running["samrajya-"+w+"-g0"] = Session{ID: "old", Name: "samrajya-" + w + "-g0"}
	r.l.running["someone-else"] = Session{ID: "x", Name: "someone-else"}
	r.tick()
	for _, n := range r.l.launched() {
		if strings.Contains(n, w) {
			t.Errorf("relaunched %s", n)
		}
	}
	if got := mustAgent(t, r, w); got.Session != "orphan" {
		t.Errorf("not adopted: %+v", got)
	}
	if len(r.l.stops) != 1 || r.l.stops[0] != "samrajya-"+w+"-g0" {
		t.Errorf("stopped %v", r.l.stops)
	}
	r.l.listErr = fmt.Errorf("daemon down")
	before := len(r.l.launched())
	r.s.Store.UpdateAgent(MainAgent, func(a *Agent) error { a.State = AgentIdle; return nil })
	r.tick()
	if len(r.l.launched()) != before {
		t.Error("launched without knowing what runs")
	}
}

// Acceptances 4, 7 and 10: a long turn is not "gone" and is reported once
// as stuck; a session whose plugin stops answering is fenced out and
// stopped, then replaced while it has work.
func TestSupervisorTellsLongTurnsFromLostSessions(t *testing.T) {
	r := newRig(t)
	w := r.ping("ENG-1", "c1")
	r.tick()
	r.attach(MainAgent)
	c := r.attach(w)
	a := mustAgent(t, r, w)
	c.do("POST", "/v1/turn", map[string]any{"agent": w, "secret": a.Secret, "state": "start"})
	for range 4 { // two hours in one turn, plugin polling throughout
		r.advance(30 * time.Minute)
		r.keepAlive()
		r.tick()
	}
	if got := mustAgent(t, r, w); got.State != AgentLive || got.Gen != 1 {
		t.Fatalf("long turn treated as gone: %+v", got)
	}
	main, _ := r.s.Store.PendingFor(MainAgent, 0)
	stuck := 0
	for _, e := range main {
		if e.Kind == "stuck" {
			stuck++
		}
	}
	if stuck != 1 {
		t.Errorf("%d stuck reports", stuck)
	}
	// Its plugin stops answering.
	delete(r.cli, w)
	r.advance(2 * time.Minute)
	r.keepAlive()
	r.tick()
	if got := mustAgent(t, r, w); got.State != AgentStarting || got.Gen != 2 {
		t.Errorf("not replaced: %+v", got)
	}
	if !contains(r.l.stops, "samrajya-"+w+"-g1") {
		t.Errorf("old session not stopped: %v", r.l.stops)
	}
	if code, _ := c.do("GET", "/v1/events?after=0&wait=0", nil); code != http.StatusConflict {
		t.Errorf("old session still acts: %d", code)
	}
}

// After a restart, running sessions get time to reattach before they count
// as lost.
func TestSupervisorWaitsForSessionsToReattachAfterARestart(t *testing.T) {
	r := newRig(t)
	w := r.ping("ENG-1", "c1")
	r.tick()
	r.attach(MainAgent)
	r.attach(w)
	// A new server and supervisor over the same store: no attachments.
	s2 := &Server{Store: r.s.Store, Token: "tok", Logger: log.New(io.Discard, "", 0), Agents: true, now: r.s.now}
	r.sv = &Supervisor{Store: r.s.Store, Server: s2, Launcher: r.l, Logger: log.New(io.Discard, "", 0), MaxWorkers: 2, now: r.s.now}
	r.tick()
	r.advance(time.Minute)
	r.tick()
	if got := mustAgent(t, r, w); got.State != AgentLive || len(r.l.stops) != 0 {
		t.Errorf("lost before it could reattach: %+v %v", got, r.l.stops)
	}
}

// Acceptance 13: nothing launches at the usage limit; just after it, one
// agent per tick, and running workers are told to resume one at a time.
func TestSupervisorWaitsOutTheLimitAndResumesSlowly(t *testing.T) {
	r := newRig(t)
	r.s.setLimited(r.now.Add(time.Hour))
	r.ping("ENG-1", "c1")
	r.ping("ENG-2", "c2")
	r.tick()
	if got := r.l.launched(); len(got) != 0 {
		t.Fatalf("launched at the limit: %v", got)
	}
	r.advance(2 * time.Hour)
	r.s.wakeAfterLimit()
	r.tick()
	if got := r.l.launched(); len(got) != 1 || got[0] != "samrajya-main-g1" {
		t.Errorf("first tick after the limit: %v", got)
	}
	r.tick()
	r.tick()
	if got := r.l.launched(); len(got) != 3 {
		t.Errorf("later ticks: %v", got)
	}
}

func TestStopAgentFencesAndPauses(t *testing.T) {
	r := newRig(t)
	w := r.ping("ENG-1", "c1")
	r.tick()
	main := r.attach(MainAgent)
	r.attach(w)
	if code, out := main.do("POST", "/v1/agents/"+w+"/stop", nil); code != http.StatusOK {
		t.Fatalf("stop: %d %v", code, out)
	}
	a := mustAgent(t, r, w)
	if a.State != AgentIdle || a.Secret != "" {
		t.Errorf("agent %+v", a)
	}
	if task, _ := r.s.Store.Task(a.TaskID); task.Status != Paused {
		t.Errorf("task %s", task.Status)
	}
	// Paused with its event accepted: not restarted.
	r.s.Store.AcceptAs(MainAgent, []string{"linear:c1"}, a.TaskID, nil, "paused")
	r.s.Store.UpdateTaskAs(MainAgent, a.TaskID, TaskUpdate{Status: Paused})
	r.keepAlive()
	r.tick()
	if n := len(r.l.launched()); n != 2 {
		t.Errorf("relaunched a paused worker: %v", r.l.launched())
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Acceptance 6: the author of a ping waiting for a free slot is told so.
func TestAPingWaitingForASlotSaysSo(t *testing.T) {
	r := newRig(t)
	w1, w2 := r.ping("ENG-1", "c1"), r.ping("ENG-2", "c2")
	r.ping("ENG-3", "c3")
	r.tick()
	r.attach(MainAgent)
	r.attach(w1)
	r.attach(w2)
	r.s.Store.AcceptAs(w1, []string{"linear:c1"}, mustAgent(t, r, w1).TaskID, nil, "x")
	r.s.Store.AcceptAs(w2, []string{"linear:c2"}, mustAgent(t, r, w2).TaskID, nil, "x")
	r.advance(16 * time.Minute)
	r.keepAlive()
	r.s.nudge(context.Background(), 15*time.Minute)
	_, _, cm := r.fake.got()
	if len(cm) != 1 || !strings.HasPrefix(cm[0], "uuid-ENG-3 c3 ") || !strings.Contains(cm[0], "next in line") {
		t.Errorf("nudges %v", cm)
	}
}

// A queued task does not wait for an idle worker's idle timeout: a worker
// with nothing to do gives up its slot a minute after its last turn.
func TestAnIdleWorkerGivesUpItsSlotToQueuedWork(t *testing.T) {
	r := newRig(t)
	w1, w2 := r.ping("ENG-1", "c1"), r.ping("ENG-2", "c2")
	w3 := r.ping("ENG-3", "c3")
	r.tick()
	r.attach(MainAgent)
	for _, w := range []string{w1, w2} {
		c := r.attach(w)
		a := mustAgent(t, r, w)
		r.s.Store.AcceptAs(w, []string{"linear:c" + w[len(w)-1:]}, a.TaskID, nil, "x")
		c.do("POST", "/v1/turn", map[string]any{"agent": w, "secret": a.Secret, "state": "end"})
	}
	r.s.Store.UpdateTaskAs(MainAgent, mustAgent(t, r, w1).TaskID, TaskUpdate{Status: Done, Result: "done"})
	r.advance(30 * time.Second)
	r.keepAlive()
	r.tick()
	if contains(r.l.launched(), "samrajya-"+w3+"-g1") {
		t.Fatal("took the slot before the grace")
	}
	r.advance(time.Minute)
	r.keepAlive()
	r.tick()
	if !contains(r.l.launched(), "samrajya-"+w3+"-g1") || !contains(r.l.stops, "samrajya-"+w1+"-g1") {
		t.Errorf("launched %v, stopped %v", r.l.launched(), r.l.stops)
	}
	if a := mustAgent(t, r, w2); a.State != AgentLive {
		t.Errorf("stopped the worker with work: %s", a.State)
	}
}
