package bridge

import (
	"os"

	"errors"
	"fmt"
	bolt "go.etcd.io/bbolt"
	"sync"
	"testing"

	"github.com/gopal-lohar/samrajya/mahamantri/linear"
)

func issuePing(issue, issueID, comment, thread string) linear.Ping {
	return linear.Ping{Kind: "mention", Issue: issue, IssueID: issueID, IssueTitle: "Work on " + issue,
		Author: "Asha", AuthorID: "u-asha", Body: "please", CommentID: comment, ThreadID: thread}
}

func workerStore(t *testing.T) *Store {
	t.Helper()
	s, _ := openStore(t, "u-asha")
	s.RouteToWorkers(true)
	if _, err := s.EnsureMain(); err != nil {
		t.Fatal(err)
	}
	return s
}

func owner(t *testing.T, s *Store, comment string) string {
	t.Helper()
	e, err := s.Event("linear:" + comment)
	if err != nil {
		t.Fatal(err)
	}
	return e.Owner()
}

// Acceptance 1 and 2: each issue gets its own worker, and follow-ups on an
// issue, in any thread, reach the same one.
func TestEachIssueGetsItsOwnWorkerAndKeepsIt(t *testing.T) {
	s := workerStore(t)
	for i, issue := range []string{"ENG-1", "ENG-2", "ENG-3"} {
		c := fmt.Sprintf("c%d", i)
		if err := s.Ingest(issuePing(issue, "uuid-"+issue, c, c), ""); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for i := range 3 {
		o := owner(t, s, fmt.Sprintf("c%d", i))
		if o == MainAgent || seen[o] {
			t.Fatalf("event %d owned by %q (seen %v)", i, o, seen)
		}
		seen[o] = true
	}
	// A reply in the first thread and a new thread on the same issue.
	s.Ingest(issuePing("ENG-1", "uuid-ENG-1", "c0b", "c0"), "")
	s.Ingest(issuePing("ENG-1", "uuid-ENG-1", "c0c", "c0c"), "")
	for _, c := range []string{"c0b", "c0c"} {
		if got, want := owner(t, s, c), owner(t, s, "c0"); got != want {
			t.Errorf("%s went to %s, want %s", c, got, want)
		}
	}
	tasks, _ := s.Tasks(true)
	if len(tasks) != 3 {
		t.Errorf("%d tasks, want 3", len(tasks))
	}
	agents, _ := s.Agents()
	if len(agents) != 4 || agents[0].ID != MainAgent {
		t.Errorf("agents %+v", agents)
	}
	// Each worker's feed holds only its own events.
	w := owner(t, s, "c1")
	mine, _ := s.PendingFor(w, 0)
	if len(mine) != 1 || mine[0].ID != "linear:c1" {
		t.Errorf("worker feed %+v", mine)
	}
}

// Acceptance 3: concurrent pings for one new issue, and a ping racing a
// dispatch, make one task.
func TestConcurrentPingsForANewIssueMakeOneTask(t *testing.T) {
	s := workerStore(t)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := fmt.Sprintf("c%d", i)
			if err := s.Ingest(issuePing("ENG-9", "uuid-9", c, c), ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := s.Dispatch(NewTask{Title: "ENG-9", Issue: "ENG-9"}, "linear:uuid-9", "look at it"); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	tasks, _ := s.Tasks(true)
	if len(tasks) != 1 {
		t.Fatalf("%d tasks, want 1: %+v", len(tasks), tasks)
	}
	agents, _ := s.Agents()
	if len(agents) != 2 {
		t.Errorf("%d agents, want main and one worker", len(agents))
	}
}

// Acceptance 4: routing is by the issue's UUID, so a changed identifier
// (the issue moved team) keeps its task.
func TestIdentifierChangeKeepsTheTask(t *testing.T) {
	s := workerStore(t)
	s.Ingest(issuePing("ENG-1", "uuid-1", "c1", "c1"), "")
	s.Ingest(issuePing("OPS-7", "uuid-1", "c2", "c2"), "")
	if owner(t, s, "c1") != owner(t, s, "c2") {
		t.Error("moved issue got a second worker")
	}
}

// Acceptance 5: an untrusted author's ping on a new issue starts nothing.
func TestUntrustedPingOnANewIssueGoesToTheMain(t *testing.T) {
	s := workerStore(t)
	p := issuePing("ENG-1", "uuid-1", "c1", "c1")
	p.AuthorID = "u-stranger"
	s.Ingest(p, "")
	if o := owner(t, s, "c1"); o != MainAgent {
		t.Errorf("owner %s", o)
	}
	if agents, _ := s.Agents(); len(agents) != 1 {
		t.Errorf("agents %+v", agents)
	}
}

// Acceptance 15: workers off routes everything to the main and creates no
// tasks, as before agents.
func TestWorkersOffRoutesEverythingToTheMain(t *testing.T) {
	s, _ := openStore(t, "u-asha")
	s.Ingest(issuePing("ENG-1", "uuid-1", "c1", "c1"), "")
	if o := owner(t, s, "c1"); o != MainAgent {
		t.Errorf("owner %s", o)
	}
	if tasks, _ := s.Tasks(true); len(tasks) != 0 {
		t.Errorf("tasks %+v", tasks)
	}
}

// Acceptance 11: a worker acts only on what it owns.
func TestAWorkerActsOnlyOnItsOwnTask(t *testing.T) {
	s := workerStore(t)
	s.Ingest(issuePing("ENG-1", "uuid-1", "c1", "c1"), "")
	s.Ingest(issuePing("ENG-2", "uuid-2", "c2", "c2"), "")
	w1, w2 := owner(t, s, "c1"), owner(t, s, "c2")
	a1, _ := s.Agent(w1)
	a2, _ := s.Agent(w2)

	if _, err := s.AcceptAs(w1, []string{"linear:c2"}, a1.TaskID, nil, "go"); !errors.Is(err, ErrForbidden) {
		t.Errorf("accepting another's event: %v", err)
	}
	if _, err := s.AcceptAs(w1, []string{"linear:c1"}, a2.TaskID, nil, "go"); !errors.Is(err, ErrForbidden) {
		t.Errorf("accepting into another's task: %v", err)
	}
	if _, err := s.AcceptAs(w1, []string{"linear:c1"}, "", &NewTask{Title: "x"}, "go"); !errors.Is(err, ErrForbidden) {
		t.Errorf("worker creating a task: %v", err)
	}
	if _, err := s.AcceptAs(w1, []string{"linear:c1"}, a1.TaskID, nil, "go"); err != nil {
		t.Errorf("accepting its own: %v", err)
	}
	if _, err := s.UpdateTaskAs(w1, a2.TaskID, TaskUpdate{Checkpoint: "x"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("updating another's task: %v", err)
	}
	if _, err := s.IntendAs(w1, Action{TaskID: a2.TaskID, Kind: "k", Target: "t", Intent: "i"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("intending for another's task: %v", err)
	}
	if _, err := s.MayReply(w1, "ENG-2"); !errors.Is(err, ErrForbidden) {
		t.Errorf("replying on another's issue: %v", err)
	}
	if _, err := s.MayReply(w1, "uuid-1"); err != nil {
		t.Errorf("replying on its own issue by uuid: %v", err)
	}
	if _, err := s.MayReply(MainAgent, "ENG-2"); err != nil {
		t.Errorf("main replying: %v", err)
	}
	// The main may act for anyone.
	if _, err := s.AcceptAs(MainAgent, []string{"linear:c2"}, a2.TaskID, nil, "go"); err != nil {
		t.Errorf("main accepting: %v", err)
	}
}

func TestAFinishedTaskIsReopenedNotDuplicated(t *testing.T) {
	s := workerStore(t)
	s.Ingest(issuePing("ENG-1", "uuid-1", "c1", "c1"), "")
	w := owner(t, s, "c1")
	a, _ := s.Agent(w)
	s.AcceptAs(w, []string{"linear:c1"}, a.TaskID, nil, "go")
	if _, err := s.UpdateTaskAs(w, a.TaskID, TaskUpdate{Status: Done, Result: "fixed, tested"}); err != nil {
		t.Fatal(err)
	}
	s.Ingest(issuePing("ENG-1", "uuid-1", "c2", "c1"), "")
	if owner(t, s, "c2") != w {
		t.Error("follow-up on a finished task went elsewhere")
	}
	task, _ := s.Task(a.TaskID)
	if task.Status != Queued {
		t.Errorf("status %s, want reopened", task.Status)
	}
	if tasks, _ := s.Tasks(true); len(tasks) != 1 {
		t.Errorf("%d tasks", len(tasks))
	}
}

func TestLaunchingFencesAndBriefsTheAgent(t *testing.T) {
	s := workerStore(t)
	s.Ingest(issuePing("ENG-1", "uuid-1", "c1", "c1"), "")
	w := owner(t, s, "c1")
	a1, err := s.Launching(w)
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := s.Launching(w)
	if a2.Gen != a1.Gen+1 || a2.Secret == a1.Secret || a2.Name == a1.Name || a2.State != AgentStarting {
		t.Errorf("relaunch %+v after %+v", a2, a1)
	}
	e, _ := s.Event("linear:c1")
	if a2.BriefSeq != e.Seq {
		t.Errorf("brief seq %d, want %d", a2.BriefSeq, e.Seq)
	}
}

func TestReportsReachTheMainOnlyWhenTheyNeedIt(t *testing.T) {
	s := workerStore(t)
	s.Ingest(issuePing("ENG-1", "uuid-1", "c1", "c1"), "")
	w := owner(t, s, "c1")
	s.Report(w, ReportProgress, "halfway")
	s.Report(w, ReportDone, "PR open")
	if p, _ := s.PendingFor(MainAgent, 0); len(p) != 0 {
		t.Errorf("main got %+v", p)
	}
	s.Report(w, ReportBlocked, "need a login")
	if p, _ := s.PendingFor(MainAgent, 0); len(p) != 1 || p[0].Kind != "report" {
		t.Errorf("main got %+v", p)
	}
	if _, err := s.Message(MainAgent, w, "also check mobile"); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.PendingFor(w, 0); len(p) != 2 {
		t.Errorf("worker feed %+v", p)
	}
}

func TestSchemaOneIsUpgradedInPlaceWithABackup(t *testing.T) {
	s, path := openStore(t)
	s.Ingest(issuePing("ENG-1", "uuid-1", "c1", "c1"), "")
	s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bMeta).Put([]byte("schema"), []byte("1")) })
	s.Close()
	s2, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := os.Stat(path + ".schema1.bak"); err != nil {
		t.Errorf("no backup: %v", err)
	}
	if e, err := s2.Event("linear:c1"); err != nil || e.Owner() != MainAgent {
		t.Errorf("old event %+v %v", e, err)
	}
}
