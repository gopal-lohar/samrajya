package bridge

import (
	"bytes"
	"encoding/json"
	"github.com/gopal-lohar/samrajya/mahamantri/brain"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type client struct {
	t          *testing.T
	url, token string
	attachment string
}

func newServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	store, _ := openStore(t)
	s := &Server{Store: store, Token: "tok", Logger: log.New(io.Discard, "", 0)}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		r = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, c.url+path, r)
	req.Header.Set("Authorization", "Bearer "+c.token)
	if c.attachment != "" {
		req.Header.Set("X-Samrajya-Attachment", c.attachment)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (c *client) attach(takeover bool) int {
	code, out := c.do("POST", "/v1/attach", map[string]any{"client": "test", "takeover": takeover})
	if code == http.StatusOK {
		c.attachment = out["attachment"].(string)
	}
	return code
}

func TestAPIRequiresTheTokenAndNoBrowser(t *testing.T) {
	_, ts := newServer(t)
	for _, tok := range []string{"", "wrong"} {
		c := &client{t: t, url: ts.URL, token: tok}
		if code, _ := c.do("GET", "/v1/status", nil); code != http.StatusUnauthorized {
			t.Errorf("token %q: %d", tok, code)
		}
	}
	req, _ := http.NewRequest("GET", ts.URL+"/v1/status", nil)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Origin", "https://evil.example")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("origin: %d", resp.StatusCode)
	}
	empty := &Server{Store: nil, Token: "", Logger: log.New(io.Discard, "", 0)}
	rec := httptest.NewRecorder()
	empty.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Error("an empty configured token must not match an empty header")
	}
}

func TestOnlyTheAttachedSessionMayReadOrChangeState(t *testing.T) {
	s, ts := newServer(t)
	main := &client{t: t, url: ts.URL, token: "tok"}
	other := &client{t: t, url: ts.URL, token: "tok"}
	if code, _ := other.do("POST", "/v1/tasks", NewTask{Title: "x"}); code != http.StatusConflict {
		t.Errorf("unattached mutation: %d", code)
	}
	if main.attach(false) != http.StatusOK {
		t.Fatal("first attach refused")
	}
	if other.attach(false) != http.StatusConflict {
		t.Fatal("second session attached over a live one")
	}
	if code, _ := main.do("POST", "/v1/tasks", NewTask{Title: "x"}); code != http.StatusOK {
		t.Errorf("attached mutation: %d", code)
	}
	if code, _ := other.do("GET", "/v1/status", nil); code != http.StatusOK {
		t.Error("status is readable by any authenticated client")
	}

	if other.attach(true) != http.StatusOK {
		t.Fatal("takeover refused")
	}
	if code, _ := main.do("GET", "/v1/events?wait=0", nil); code != http.StatusConflict {
		t.Errorf("the replaced session still reads the feed: %d", code)
	}

	// A holder that detaches frees the attachment at once.
	if code, _ := other.do("POST", "/v1/detach", nil); code != http.StatusOK {
		t.Fatalf("detach: %d", code)
	}
	if code, _ := other.do("GET", "/v1/events?wait=0", nil); code != http.StatusConflict {
		t.Errorf("a detached session still reads the feed: %d", code)
	}
	if main.attach(false) != http.StatusOK {
		t.Fatal("could not attach after the holder detached")
	}
	other.attach(true)

	// A holder that went quiet can be replaced without takeover.
	s.mu.Lock()
	s.lastSeen = time.Now().Add(-2 * attachTTL)
	s.mu.Unlock()
	if main.attach(false) != http.StatusOK {
		t.Error("could not attach after the holder went quiet")
	}
}

func TestFeedLongPollWakesOnIngestAndReplaysUntilAccepted(t *testing.T) {
	s, ts := newServer(t)
	c := &client{t: t, url: ts.URL, token: "tok"}
	c.attach(false)

	got := make(chan []any, 1)
	go func() {
		_, out := c.do("GET", "/v1/events?after=0&wait=10", nil)
		got <- out["events"].([]any)
	}()
	time.Sleep(100 * time.Millisecond)
	if err := s.Ingest(ping("c1", "c1"), "d1"); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-got:
		if len(ev) != 1 || ev[0].(map[string]any)["id"] != "linear:c1" {
			t.Fatalf("events = %v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long poll was not woken by the new event")
	}

	// A fresh connection (after=0) gets it again until it is accepted.
	if _, out := c.do("GET", "/v1/events?after=0&wait=0", nil); len(out["events"].([]any)) != 1 {
		t.Errorf("replay = %v", out)
	}
	code, out := c.do("POST", "/v1/accept", map[string]any{"events": []string{"linear:c1"}, "newTask": NewTask{Title: "Fix"}, "nextAction": "look"})
	if code != http.StatusOK || out["id"] != "T1" {
		t.Fatalf("accept: %d %v", code, out)
	}
	if _, out := c.do("GET", "/v1/events?after=0&wait=0", nil); len(out["events"].([]any)) != 0 {
		t.Errorf("accepted event still delivered: %v", out)
	}
}

func TestAPIErrorsAreExplicit(t *testing.T) {
	_, ts := newServer(t)
	c := &client{t: t, url: ts.URL, token: "tok"}
	c.attach(false)
	if code, _ := c.do("POST", "/v1/tasks/T9", TaskUpdate{Status: Working}); code != http.StatusNotFound {
		t.Errorf("unknown task: %d", code)
	}
	if code, out := c.do("POST", "/v1/tasks", map[string]any{"title": "x", "bogus": 1}); code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "bogus") {
		t.Errorf("unknown field: %d %v", code, out)
	}
	code, out := c.do("POST", "/v1/actions", Action{Kind: "linear_comment", Target: "ENG-1", Intent: "reply"})
	if code != http.StatusOK || out["status"] != Intended {
		t.Fatalf("intend: %d %v", code, out)
	}
	_, st := c.do("GET", "/v1/status", nil)
	if n := len(st["recovery"].(map[string]any)["unresolvedActions"].([]any)); n != 1 {
		t.Errorf("status shows %d unresolved actions", n)
	}
	if code, _ := c.do("POST", "/v1/actions/"+out["id"].(string), map[string]string{"status": "done", "ref": "cmt", "threadId": "th"}); code != http.StatusOK {
		t.Errorf("resolve: %d", code)
	}
}

func TestBrainIsServedReadableByAllWritableByTheMain(t *testing.T) {
	store, _ := openStore(t)
	mem, err := brain.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(&Server{Store: store, Token: "tok", Brain: mem, Logger: log.New(io.Discard, "", 0)})
	t.Cleanup(ts.Close)
	main := &client{t: t, url: ts.URL, token: "tok"}
	other := &client{t: t, url: ts.URL, token: "tok"}
	note := map[string]string{"description": "Deploys need a heads-up", "body": "Tell the person before deploying."}
	if code, _ := other.do("PUT", "/brain/deploys", note); code != http.StatusConflict {
		t.Errorf("unattached write: %d", code)
	}
	main.attach(false)
	if code, _ := main.do("PUT", "/brain/deploys", note); code != http.StatusCreated {
		t.Fatalf("write: %d", code)
	}
	if code, out := other.do("GET", "/brain/deploys", nil); code != http.StatusOK || out["body"] != note["body"] {
		t.Errorf("read: %d %v", code, out)
	}
	if _, st := other.do("GET", "/v1/status", nil); !strings.Contains(st["brainIndex"].(string), "Deploys need a heads-up") {
		t.Errorf("status brain index = %v", st["brainIndex"])
	}
}
