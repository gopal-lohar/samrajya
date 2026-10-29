package bridge

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopal-lohar/samrajya/mahamantri/linear"
	bolt "go.etcd.io/bbolt"
)

func openStore(t *testing.T, trusted ...string) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "native.db")
	s, err := Open(path, trusted)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func ping(comment, thread string) linear.Ping {
	return linear.Ping{Kind: "mention", Issue: "ENG-1", IssueID: "iss-1", IssueTitle: "Fix login",
		Author: "Asha", AuthorID: "u-asha", Body: "please fix", CommentID: comment, ThreadID: thread}
}

func TestIngestKeepsEachCommentOnce(t *testing.T) {
	s, _ := openStore(t)
	for _, d := range []string{"d1", "d2"} { // Linear redelivering under a new delivery id
		if err := s.Ingest(ping("c1", "c1"), d); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Ingest(ping("c2", "c1"), "d3"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Pending(0)
	if len(got) != 2 || got[0].ID != "linear:c1" || got[1].ID != "linear:c2" || got[0].Delivery != "d1" {
		t.Fatalf("pending = %+v", got)
	}
	if later, _ := s.Pending(got[0].Seq); len(later) != 1 || later[0].ID != "linear:c2" {
		t.Errorf("pending after first = %+v", later)
	}
}

func TestIngestMarksUntrustedAuthors(t *testing.T) {
	s, _ := openStore(t, "u-owner")
	s.Ingest(ping("c1", "c1"), "d1")
	p := ping("c2", "c2")
	p.AuthorID = "u-owner"
	s.Ingest(p, "d2")
	got, _ := s.Pending(0)
	if got[0].Trusted || !got[1].Trusted {
		t.Errorf("trusted = %v %v", got[0].Trusted, got[1].Trusted)
	}
	open, _ := openStore(t)
	open.Ingest(ping("c1", "c1"), "d1")
	if e, _ := open.Event("linear:c1"); !e.Trusted {
		t.Error("with no trusted list, everyone is trusted")
	}
}

func TestIngestBoundsTheBody(t *testing.T) {
	s, _ := openStore(t)
	p := ping("c1", "c1")
	p.Body = strings.Repeat("é", maxBody)
	s.Ingest(p, "d1")
	e, _ := s.Event("linear:c1")
	if len(e.Body) > maxBody+100 || !strings.Contains(e.Body, "read the rest on Linear") || !strings.HasPrefix(e.Body, "é") {
		t.Errorf("body length %d", len(e.Body))
	}
}

func TestAcceptIsAtomicAndIdempotent(t *testing.T) {
	s, _ := openStore(t)
	s.Ingest(ping("c1", "c1"), "d1")

	if _, err := s.Accept([]string{"linear:c1", "linear:missing"}, "", &NewTask{Title: "Fix login"}, "investigate"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if tasks, _ := s.Tasks(true); len(tasks) != 0 {
		t.Fatalf("a failed accept left a task behind: %+v", tasks)
	}
	if p, _ := s.Pending(0); len(p) != 1 {
		t.Fatal("a failed accept must leave the event pending")
	}

	task, err := s.Accept([]string{"linear:c1"}, "", &NewTask{Title: "Fix login"}, "investigate")
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "T1" || task.Issue != "ENG-1" || task.ThreadID != "c1" || task.Status != Queued || task.NextAction != "investigate" {
		t.Errorf("task = %+v", task)
	}
	again, err := s.Accept([]string{"linear:c1"}, "", &NewTask{Title: "Duplicate"}, "again")
	if err != nil || again.ID != "T1" {
		t.Errorf("re-accepting made %+v, %v", again, err)
	}
	if tasks, _ := s.Tasks(true); len(tasks) != 1 {
		t.Errorf("tasks = %+v", tasks)
	}
	if p, _ := s.Pending(0); len(p) != 0 {
		t.Errorf("pending = %+v", p)
	}
}

func TestAcceptFollowUpIntoExistingTaskReopensIt(t *testing.T) {
	s, _ := openStore(t)
	s.Ingest(ping("c1", "c1"), "d1")
	task, _ := s.Accept([]string{"linear:c1"}, "", &NewTask{Title: "Fix"}, "fix it")
	if _, err := s.UpdateTask(task.ID, TaskUpdate{Status: Done}); err == nil {
		t.Fatal("done without a result must be refused")
	}
	s.UpdateTask(task.ID, TaskUpdate{Status: Done, Result: "fixed, tests pass"})
	s.Ingest(ping("c2", "c1"), "d2")
	task, err := s.Accept([]string{"linear:c2"}, task.ID, nil, "answer the follow-up")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != Queued || len(task.Events) != 2 || task.Result != "fixed, tests pass" {
		t.Errorf("task = %+v", task)
	}
}

func TestAcceptValidatesItsArguments(t *testing.T) {
	s, _ := openStore(t)
	s.Ingest(ping("c1", "c1"), "d1")
	for name, call := range map[string]func() error{
		"no events":   func() error { _, err := s.Accept(nil, "", &NewTask{Title: "x"}, "n"); return err },
		"both":        func() error { _, err := s.Accept([]string{"linear:c1"}, "T9", &NewTask{Title: "x"}, "n"); return err },
		"neither":     func() error { _, err := s.Accept([]string{"linear:c1"}, "", nil, "n"); return err },
		"no next":     func() error { _, err := s.Accept([]string{"linear:c1"}, "", &NewTask{Title: "x"}, " "); return err },
		"no title":    func() error { _, err := s.Accept([]string{"linear:c1"}, "", &NewTask{}, "n"); return err },
		"bad task id": func() error { _, err := s.Accept([]string{"linear:c1"}, "T9", nil, "n"); return err },
	} {
		if call() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := s.UpdateTask("T1", TaskUpdate{Status: "finished"}); err == nil {
		t.Error("unknown status accepted")
	}
}

func TestActionsAreJournaledAndLearnTheirThread(t *testing.T) {
	s, _ := openStore(t)
	task, _ := s.CreateTask(NewTask{Title: "Reply"})
	if _, err := s.Intend(Action{Kind: "linear_comment", Target: "ENG-1"}); err == nil {
		t.Error("an action without intent was recorded")
	}
	if _, err := s.Intend(Action{TaskID: "T99", Kind: "k", Target: "t", Intent: "i"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown task: %v", err)
	}
	a, err := s.Intend(Action{TaskID: task.ID, Kind: "linear_comment", Target: "ENG-1 thread c9", Intent: "post the plan"})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := s.Recovery()
	if len(r.Uncertain) != 1 || r.Uncertain[0].ID != a.ID {
		t.Fatalf("an unresolved action must show as uncertain: %+v", r.Uncertain)
	}
	if _, err := s.Resolve(a.ID, "maybe", "", "", "", ""); err == nil {
		t.Error("bad outcome accepted")
	}
	if _, err := s.Resolve(a.ID, Happened, "comment-77", "", "c9", "ENG-1"); err != nil {
		t.Fatal(err)
	}
	if !s.Has("c9") {
		t.Error("the thread of a posted comment must be learned at once")
	}
	if r, _ := s.Recovery(); len(r.Uncertain) != 0 || len(r.Open) != 1 {
		t.Errorf("recovery = %+v", r)
	}
}

func TestStatePersistsAndTheStoreIsExclusive(t *testing.T) {
	s, path := openStore(t)
	s.Ingest(ping("c1", "c1"), "d1")
	s.Add("thread-1", "ENG-1")
	if _, err := Open(path, nil); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("second open: %v", err)
	}
	s.Close()
	again, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if p, _ := again.Pending(0); len(p) != 1 || !again.Has("thread-1") {
		t.Errorf("after reopen: pending %+v", p)
	}
}

func TestUnknownSchemaIsRefusedNotReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native.db")
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	db.Update(func(tx *bolt.Tx) error {
		b, _ := tx.CreateBucketIfNotExists(bMeta)
		return b.Put([]byte("schema"), []byte("99"))
	})
	db.Close()
	if _, err := Open(path, nil); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Errorf("err = %v", err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("store mode = %v", info.Mode())
	}
}

func TestImportThreadsOnce(t *testing.T) {
	s, _ := openStore(t)
	legacy := filepath.Join(t.TempDir(), "threads.json")
	if n, err := s.ImportThreads(legacy); n != 0 || err != nil {
		t.Errorf("missing file: %d %v", n, err)
	}
	os.WriteFile(legacy, []byte(`{"t1":{"issue":"ENG-1","seen":"2026-01-01T00:00:00Z"},"t2":{"issue":"ENG-2"}}`), 0o600)
	if n, err := s.ImportThreads(legacy); n != 2 || err != nil || !s.Has("t1") || !s.Has("t2") {
		t.Fatalf("import: %d %v", n, err)
	}
	os.WriteFile(legacy, []byte(`{"t3":{"issue":"ENG-3"}}`), 0o600)
	if n, _ := s.ImportThreads(legacy); n != 0 || s.Has("t3") {
		t.Error("a second import must not run")
	}
}
