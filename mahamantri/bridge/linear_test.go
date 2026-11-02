package bridge

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/linear"
)

// fakeLinear records what the server does on Linear.
type fakeLinear struct {
	mu        sync.Mutex
	reactions []string // "comment emoji"
	statuses  []string
	comments  []string // "issue parent body"
	fail      bool
}

func (f *fakeLinear) React(_ context.Context, comment, emoji string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactions = append(f.reactions, comment+" "+emoji)
	return nil
}

func (f *fakeLinear) SetStatus(_ context.Context, emoji, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("down")
	}
	f.statuses = append(f.statuses, strings.TrimSpace(emoji+" "+label))
	return nil
}

func (f *fakeLinear) Comment(_ context.Context, issue, parent, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("down")
	}
	f.comments = append(f.comments, issue+" "+parent+" "+body)
	return nil
}

func (f *fakeLinear) Handle(_ context.Context, userID string) (string, error) {
	return "handle-" + userID, nil
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
	s := &Server{Store: store, Token: "tok", Logger: log.New(io.Discard, "", 0), Linear: fake, Received: "👀"}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
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
	s.Ingest(ping("c1", "c1"), "d1")
	s.Ingest(ping("c1", "c1"), "d2") // Linear's retry of the same comment
	s.Ingest(ping("c2", "c1"), "d3")
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
	if st := s.currentStatus(); st.Label != "Blocked on ENG-7 - needs you" {
		t.Errorf("blocked: %v", st)
	}
}

func TestWatchSetsEachStatusOnceRetriesAndLeavesItOffline(t *testing.T) {
	s, fake, c := linearServer(t)
	c.attach(false)
	fake.setFail(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Watch(ctx, 10*time.Millisecond, 0); close(done) }()
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
	p := linear.Ping{Kind: "mention", Issue: "ENG-1", IssueID: "iss-1", Author: "Asha", AuthorID: "u1", Body: "fix", CommentID: "c1", ThreadID: "th-1"}
	s.Ingest(p, "d1")

	s.nudge(context.Background(), 15*time.Minute)
	if _, _, cm := fake.got(); len(cm) != 0 {
		t.Fatalf("nudged too early: %v", cm)
	}
	now = now.Add(16 * time.Minute)
	fake.setFail(true)
	s.nudge(context.Background(), 15*time.Minute) // fails: retried next time
	fake.setFail(false)
	s.nudge(context.Background(), 15*time.Minute)
	s.nudge(context.Background(), 15*time.Minute)
	_, _, cm := fake.got()
	if len(cm) != 1 || !strings.HasPrefix(cm[0], "iss-1 th-1 @handle-u1 I've received this.") || !strings.Contains(cm[0], "offline") {
		t.Fatalf("nudges = %v", cm)
	}

	// While a session is attached, the nudge says it is busy, not offline;
	// an accepted ping is not nudged.
	s.Ingest(linear.Ping{Kind: "reply", IssueID: "iss-1", AuthorID: "u1", CommentID: "c2", ThreadID: "th-1"}, "d2")
	s.Ingest(linear.Ping{Kind: "reply", IssueID: "iss-1", AuthorID: "u1", CommentID: "c3", ThreadID: "th-1"}, "d3")
	now = now.Add(16 * time.Minute)
	c.attach(false)
	c.do("POST", "/v1/accept", map[string]any{"events": []string{"linear:c3"}, "newTask": NewTask{Title: "x"}, "nextAction": "look"})
	s.nudge(context.Background(), 15*time.Minute)
	if _, _, cm := fake.got(); len(cm) != 2 || !strings.Contains(cm[1], "busy") {
		t.Errorf("nudges = %v", cm)
	}
}
