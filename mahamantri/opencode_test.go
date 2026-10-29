package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/bridge"
	"github.com/gopal-lohar/samrajya/mahamantri/config"
	"github.com/gopal-lohar/samrajya/mahamantri/linear"
)

// fakeOpencode answers the five calls mode opencode makes.
type fakeOpencode struct {
	mu           sync.Mutex
	sessions     map[string]bool
	created      []map[string]any
	instructions map[string]string
	prompts      []map[string]any
	n            int
}

func (f *fakeOpencode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, pw, _ := r.BasicAuth(); pw != "pw" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/"), "/")
	switch {
	case r.URL.Path == "/api/session/active":
		w.Write([]byte(`[]`))
	case r.Method == "POST" && r.URL.Path == "/api/session":
		f.n++
		id := "ses_" + string(rune('a'-1+f.n))
		f.sessions[id] = true
		f.created = append(f.created, body)
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"id": id, "title": "Samrajya"}})
	case parts[0] == "experimental":
		if !f.sessions[parts[2]] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.instructions[parts[2]] = body["value"].(string)
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 2 && r.Method == "GET":
		if !f.sessions[parts[1]] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"id": parts[1]}})
	case len(parts) == 3 && parts[2] == "prompt":
		if !f.sessions[parts[1]] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body["session"] = parts[1]
		f.prompts = append(f.prompts, body)
		w.Write([]byte(`{"data":{"id":"msg"}}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeOpencode) promptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

func setupOpencode(t *testing.T) (*fakeOpencode, config.Config, *bridge.Store) {
	t.Helper()
	fake := &fakeOpencode{sessions: map[string]bool{}, instructions: map[string]string{}}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	role := filepath.Join(dir, "role.md")
	os.WriteFile(role, []byte("You own outcomes."), 0o600)
	var cfg config.Config
	cfg.Mode = "opencode"
	cfg.Opencode.ServerURL, cfg.Opencode.Password, cfg.Opencode.InstructionsFile = ts.URL, "pw", role
	cfg.Opencode.Model.ProviderID, cfg.Opencode.Model.ID = "acme", "big"
	cfg.Bridge.MainDirectory = dir
	store, err := bridge.Open(filepath.Join(dir, "m.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return fake, cfg, store
}

var quiet = log.New(io.Discard, "", 0)

func TestOpencodeMainSessionIsCreatedOnceAndGetsTheRole(t *testing.T) {
	fake, cfg, store := setupOpencode(t)
	o, err := newOpencodeMain(context.Background(), cfg, store, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if o.session != "ses_a" || store.Meta(sessionKey) != "ses_a" || fake.instructions["ses_a"] != "You own outcomes." {
		t.Fatalf("session %q, meta %q, instructions %v", o.session, store.Meta(sessionKey), fake.instructions)
	}
	c := fake.created[0]
	if c["title"] != "Samrajya" || c["location"].(map[string]any)["directory"] != cfg.Bridge.MainDirectory ||
		c["model"].(map[string]any)["providerID"] != "acme" {
		t.Errorf("created with %v", c)
	}
	again, err := newOpencodeMain(context.Background(), cfg, store, quiet)
	if err != nil || again.session != "ses_a" || len(fake.created) != 1 {
		t.Errorf("restart: session %q, created %d, %v", again.session, len(fake.created), err)
	}
}

func TestOpencodeStartupSaysWhatIsWrong(t *testing.T) {
	_, cfg, store := setupOpencode(t)
	bad := cfg
	bad.Opencode.Password = "wrong"
	if _, err := newOpencodeMain(context.Background(), bad, store, quiet); err == nil || !strings.Contains(err.Error(), "password") {
		t.Errorf("wrong password: %v", err)
	}
	bad = cfg
	bad.Opencode.InstructionsFile = "/nonexistent/role.md"
	if _, err := newOpencodeMain(context.Background(), bad, store, quiet); err == nil || !strings.Contains(err.Error(), "instructionsFile") {
		t.Errorf("missing role: %v", err)
	}
}

func TestOpencodeDeliversEachPingOnceAndRecreatesALostSession(t *testing.T) {
	fake, cfg, store := setupOpencode(t)
	srv := &bridge.Server{Store: store, Token: "t", Logger: quiet}
	o, err := newOpencodeMain(context.Background(), cfg, store, quiet)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { o.deliver(ctx, srv); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	ping := linear.Ping{Kind: "mention", Issue: "ENG-1", Author: "Asha", AuthorID: "u", Body: "fix it", CommentID: "c1", ThreadID: "c1"}
	srv.Ingest(ping, "d1")
	waitFor(t, func() bool { return fake.promptCount() == 1 }, "first delivery")
	p := fake.prompts[0]
	if p["delivery"] != "queue" || !strings.Contains(p["text"].(string), "samrajya_accept (event_id linear:c1)") || p["session"] != "ses_a" {
		t.Errorf("prompt = %v", p)
	}

	srv.Ingest(ping, "d2") // a redelivery: same comment, nothing new
	time.Sleep(200 * time.Millisecond)
	if fake.promptCount() != 1 {
		t.Errorf("a redelivered ping was prompted again")
	}

	fake.mu.Lock()
	delete(fake.sessions, "ses_a") // deleted on the opencode side
	fake.mu.Unlock()
	srv.Ingest(linear.Ping{Kind: "mention", Body: "next", CommentID: "c2", ThreadID: "c2"}, "d3")
	waitFor(t, func() bool { return fake.promptCount() == 2 }, "delivery to a recreated session")
	if fake.prompts[1]["session"] != "ses_b" || store.Meta(sessionKey) != "ses_b" || fake.instructions["ses_b"] == "" {
		t.Errorf("after loss: prompt %v, meta %q", fake.prompts[1], store.Meta(sessionKey))
	}
}

func TestRenderPingMarksUntrustedAuthors(t *testing.T) {
	got := renderPing(bridge.Event{ID: "linear:c1", Kind: "reply", Issue: "ENG-1", IssueTitle: "Fix", Author: "Bo", Body: "a\nb", ThreadID: "t1"})
	for _, want := range []string{`Bo replied in a thread you are part of on ENG-1 ("Fix"). They are NOT a trusted requester.`, "> a\n> b", "reply in thread t1"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}
