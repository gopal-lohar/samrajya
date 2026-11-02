package bridge

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeLinear records what the server does on Linear.
type fakeLinear struct {
	mu        sync.Mutex
	reactions []string // "comment emoji"
	statuses  []string
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
	f.statuses = append(f.statuses, emoji+" "+label)
	return nil
}

func (f *fakeLinear) got() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reactions...), append([]string(nil), f.statuses...)
}

func linearServer(t *testing.T) (*Server, *fakeLinear, *httptest.Server) {
	t.Helper()
	store, _ := openStore(t)
	fake := &fakeLinear{}
	s := &Server{Store: store, Token: "tok", Logger: log.New(io.Discard, "", 0), Linear: fake, Received: "👀"}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, fake, ts
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
	eventually(t, func() bool { r, _ := fake.got(); return len(r) == 2 }, "two reactions")
	time.Sleep(50 * time.Millisecond)
	if r, _ := fake.got(); len(r) != 2 || r[0] == r[1] {
		t.Errorf("reactions = %v", r)
	}
}

func TestOnlyTheMainSessionReactsAndOnlyWithAKey(t *testing.T) {
	_, fake, ts := linearServer(t)
	c := &client{t: t, url: ts.URL, token: "tok"}
	body := map[string]string{"commentId": "c9", "emoji": "👍"}
	if code, _ := c.do("POST", "/v1/react", body); code != http.StatusConflict {
		t.Errorf("unattached react: %d", code)
	}
	c.attach(false)
	if code, _ := c.do("POST", "/v1/react", map[string]string{"commentId": "c9"}); code != http.StatusBadRequest {
		t.Errorf("no emoji: %d", code)
	}
	if code, _ := c.do("POST", "/v1/react", body); code != http.StatusOK {
		t.Errorf("react: %d", code)
	}
	if r, _ := fake.got(); len(r) != 1 || r[0] != "c9 👍" {
		t.Errorf("reactions = %v", r)
	}

	_, ts2 := newServer(t) // no Linear key
	c2 := &client{t: t, url: ts2.URL, token: "tok"}
	c2.attach(false)
	if code, _ := c2.do("POST", "/v1/react", body); code != http.StatusNotImplemented {
		t.Errorf("react without a key: %d", code)
	}
}

func TestStatusFollowsTheMainSession(t *testing.T) {
	s, fake, ts := linearServer(t)
	c := &client{t: t, url: ts.URL, token: "tok"}
	if s.currentStatus() != offline {
		t.Errorf("nobody attached: %v", s.currentStatus())
	}
	c.attach(false)
	if st := s.currentStatus(); st.Label != "Online" {
		t.Errorf("attached: %v", st)
	}
	_, task := c.do("POST", "/v1/tasks", NewTask{Title: "Fix", Issue: "ENG-7"})
	c.do("POST", "/v1/tasks/"+task["id"].(string), TaskUpdate{Status: Working})
	if st := s.currentStatus(); st.Label != "Working on ENG-7" {
		t.Errorf("working: %v", st)
	}

	// WatchStatus sets each change once, retries a failure, and leaves the
	// status offline when it stops.
	fake.fail = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.WatchStatus(ctx, 10*time.Millisecond); close(done) }()
	time.Sleep(50 * time.Millisecond)
	fake.mu.Lock()
	fake.fail = false
	fake.mu.Unlock()
	eventually(t, func() bool { _, st := fake.got(); return len(st) == 1 }, "status after a failure")
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if _, st := fake.got(); len(st) != 2 || st[0] != "🟢 Working on ENG-7" || st[1] != offline.Emoji+" "+offline.Label {
		t.Errorf("statuses = %v", st)
	}
}
