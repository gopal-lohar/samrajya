package bridge

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLinear records what the server does on Linear.
type fakeLinear struct {
	mu        sync.Mutex
	reactions []string // "comment emoji"
	statuses  []string
	comments  []string // "issue parent body"
	fail      bool
}

func (f *fakeLinear) Name() string { return "linear" }

func (f *fakeLinear) Received(_ context.Context, e Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactions = append(f.reactions, e.MessageRef()+" 👀")
	return nil
}

func (f *fakeLinear) ShowStatus(_ context.Context, st Status) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("down")
	}
	f.statuses = append(f.statuses, strings.TrimSpace(st.Emoji+" "+st.Label))
	return nil
}

func (f *fakeLinear) Deliver(_ context.Context, conv, parent, body string) (Posted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return Posted{}, errors.New("down")
	}
	f.comments = append(f.comments, strings.TrimPrefix(conv, "linear:")+" "+parent+" "+body)
	p := Posted{ID: "cmt-" + body[:1], Thread: parent}
	if parent == "" {
		p.Thread = p.ID
	}
	return p, nil
}

func (f *fakeLinear) Notify(ctx context.Context, e Event, text string) error {
	_, err := f.Deliver(ctx, e.Conversation, e.ThreadID, "@handle-"+e.AuthorID+" "+text)
	return err
}

func (f *fakeLinear) setFail(v bool) {
	f.mu.Lock()
	f.fail = v
	f.mu.Unlock()
}

func (f *fakeLinear) got() (reactions, statuses, comments []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reactions...), append([]string(nil), f.statuses...), append([]string(nil), f.comments...)
}

func linearServer(t *testing.T) (*Server, *fakeLinear, *client) {
	t.Helper()
	store, _ := openStore(t)
	fake := &fakeLinear{}
	s := &Server{Store: store, Token: "tok", Logger: log.New(io.Discard, "", 0)}
	s.AddSurface(fake, Policy{Reopen: true, NudgeAfter: 15 * time.Minute})
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	servers.Store(ts.URL, s)
	return s, fake, &client{t: t, url: ts.URL, token: "tok"}
}

func eventually(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestANewPingIsMarkedReceivedOnce(t *testing.T) {
	s, fake, _ := linearServer(t)
	s.ingestPing(ping("c1", "c1"), "d1")
	s.ingestPing(ping("c1", "c1"), "d2") // Linear's retry of the same comment
	s.ingestPing(ping("c2", "c1"), "d3")
	eventually(t, func() bool { r, _, _ := fake.got(); return len(r) == 2 }, "two reactions")
	time.Sleep(50 * time.Millisecond)
	if r, _, _ := fake.got(); len(r) != 2 || r[0] == r[1] {
		t.Errorf("reactions = %v", r)
	}
}

func TestStatusSaysSomethingOnlyWhenSomethingIsWrong(t *testing.T) {
	s, _, c := linearServer(t)
	if s.currentStatus() != offline {
		t.Errorf("nobody attached: %v", s.currentStatus())
	}
	c.attach(false)
	if st := s.currentStatus(); st != (Status{}) {
		t.Errorf("attached and idle: %v", st)
	}
	_, task := c.do("POST", "/v1/tasks", NewTask{Title: "Fix", Issue: "ENG-7"})
	c.do("POST", "/v1/tasks/"+task["id"].(string), TaskUpdate{Status: Working})
	if st := s.currentStatus(); st != (Status{}) {
		t.Errorf("working: %v", st)
	}
	c.do("POST", "/v1/tasks/"+task["id"].(string), TaskUpdate{Status: Blocked, Checkpoint: "needs a decision"})
	if st := s.currentStatus(); st.Label != "Blocked on Fix - needs you" {
		t.Errorf("blocked: %v", st)
	}
}

func TestWatchSetsEachStatusOnceRetriesAndLeavesItOffline(t *testing.T) {
	s, fake, c := linearServer(t)
	c.attach(false)
	fake.setFail(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Watch(ctx, 10*time.Millisecond); close(done) }()
	time.Sleep(50 * time.Millisecond)
	fake.setFail(false)
	eventually(t, func() bool { _, st, _ := fake.got(); return len(st) == 1 }, "status after a failure")
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	// The first check clears whatever an earlier run left.
	if _, st, _ := fake.got(); len(st) != 2 || st[0] != "" || st[1] != offline.Emoji+" "+offline.Label {
		t.Errorf("statuses = %v", st)
	}
}

func TestAPingLeftWaitingIsNudgedOnceMentioningItsAuthor(t *testing.T) {
	s, fake, c := linearServer(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	s.Store.now = s.now
	p := tping{Kind: "mention", Issue: "ENG-1", IssueID: "iss-1", Author: "Asha", AuthorID: "u1", Body: "fix", CommentID: "c1", ThreadID: "th-1"}
	s.ingestPing(p, "d1")

	s.nudge(context.Background())
	if _, _, cm := fake.got(); len(cm) != 0 {
		t.Fatalf("nudged too early: %v", cm)
	}
	now = now.Add(16 * time.Minute)
	fake.setFail(true)
	s.nudge(context.Background()) // fails: retried next time
	fake.setFail(false)
	s.nudge(context.Background())
	s.nudge(context.Background())
	_, _, cm := fake.got()
	if len(cm) != 1 || !strings.HasPrefix(cm[0], "iss-1 th-1 @handle-u1 I've received this.") || !strings.Contains(cm[0], "offline") {
		t.Fatalf("nudges = %v", cm)
	}

	// While a session is attached, the nudge says it is busy, not offline;
	// an accepted ping is not nudged.
	s.ingestPing(tping{Kind: "reply", IssueID: "iss-1", AuthorID: "u1", CommentID: "c2", ThreadID: "th-1"}, "d2")
	s.ingestPing(tping{Kind: "reply", IssueID: "iss-1", AuthorID: "u1", CommentID: "c3", ThreadID: "th-1"}, "d3")
	now = now.Add(16 * time.Minute)
	c.attach(false)
	c.do("POST", "/v1/accept", map[string]any{"events": []string{"linear:c3"}, "newTask": NewTask{Title: "x"}, "nextAction": "look"})
	s.nudge(context.Background())
	if _, _, cm := fake.got(); len(cm) != 2 || !strings.Contains(cm[1], "busy") {
		t.Errorf("nudges = %v", cm)
	}
}

func TestReplyPostsAsSenapatiAndJournalsIt(t *testing.T) {
	s, fake, c := linearServer(t)
	if code, _ := c.do("POST", "/v1/reply", map[string]string{"conversation": "linear:ENG-1", "body": "hi"}); code != http.StatusConflict {
		t.Errorf("unattached reply: %d", code)
	}
	c.attach(false)
	if code, _ := c.do("POST", "/v1/reply", map[string]string{"conversation": "linear:ENG-1"}); code != http.StatusBadRequest {
		t.Errorf("no body: %d", code)
	}
	code, out := c.do("POST", "/v1/reply", map[string]string{"conversation": "linear:ENG-1", "threadId": "th-1", "body": "Done"})
	if code != http.StatusOK || out["id"] != "cmt-D" || out["threadId"] != "th-1" {
		t.Fatalf("reply: %d %v", code, out)
	}
	_, _, cm := fake.got()
	if len(cm) != 1 || cm[0] != "ENG-1 th-1 Done" {
		t.Errorf("comments = %v", cm)
	}
	// A new top-level comment starts a thread Senapati is part of.
	c.do("POST", "/v1/reply", map[string]string{"conversation": "linear:ENG-1", "body": "New"})
	if !s.Store.Has("cmt-N") {
		t.Error("the new comment's thread was not learned")
	}
	// A failed post stays an unresolved action, to be checked before retrying.
	fake.setFail(true)
	if code, out := c.do("POST", "/v1/reply", map[string]string{"conversation": "linear:ENG-1", "body": "Lost"}); code != http.StatusBadGateway || out["actionId"] == nil {
		t.Errorf("failed reply: %d %v", code, out)
	}
	rec, _ := s.Store.Recovery()
	if len(rec.Uncertain) != 1 {
		t.Errorf("uncertain actions = %v", rec.Uncertain)
	}
}
