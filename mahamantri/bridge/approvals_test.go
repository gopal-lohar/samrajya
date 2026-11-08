package bridge

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeUpdater struct {
	mu      sync.Mutex
	started []Approval
	done    bool
	ok      bool
	text    string
}

func (f *fakeUpdater) Describe(_ context.Context, kind, ref string) (string, string, error) {
	if kind == Deploy {
		return "abc123def4567", "Changes: abc123d Make replies shorter", nil
	}
	return "", "", nil
}

func (f *fakeUpdater) Start(a Approval) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, a)
	return nil
}

func (f *fakeUpdater) Result(Approval) (bool, bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.done, f.ok, f.text
}

func (f *fakeUpdater) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started)
}

// A worker asks to deploy its change; only the person's own answer in that
// conversation starts it, and how it went is told in the chat and to the
// worker.
func TestADeployRunsOnlyOnceThePersonApprovesIt(t *testing.T) {
	r, chat := chatRig(t)
	up := &fakeUpdater{}
	r.s.SelfUpdate = up
	r.s.Receive(chatMsg("1", "make your replies shorter"))
	r.tick()
	main := r.attach(MainAgent)
	_, task := main.do("POST", "/v1/dispatch", map[string]any{"title": "Shorter replies", "brief": "do it", "conversation": "chat:1", "threadId": "1",
		"events": []string{"chat:1:1"}})
	w := task["agent"].(string)
	r.tick()
	wc := r.attach(w)

	if code, _ := wc.do("POST", "/v1/restart", map[string]any{"kind": "deploy", "reason": "x"}); code != http.StatusBadRequest {
		t.Errorf("a deploy without a ref was asked: %d", code)
	}
	if code, _ := wc.do("POST", "/v1/restart", map[string]any{"kind": "restart", "reason": "x", "conversation": "chat:2"}); code != http.StatusForbidden {
		t.Errorf("a worker asked outside its conversations: %d", code)
	}
	code, out := wc.do("POST", "/v1/restart", map[string]any{"kind": "deploy", "ref": "shorter-replies", "reason": "Replies are now at most three sentences. Tests pass."})
	if code != http.StatusOK || out["id"] != "R1" || out["ref"] != "abc123def4567" {
		t.Fatalf("ask: %d %v", code, out)
	}
	asked := chat.got()
	if len(asked) != 1 || !strings.Contains(asked[0], "deploy abc123def4 and restart Samrajya") || !strings.Contains(asked[0], "approve R1") ||
		!strings.Contains(asked[0], "Make replies shorter") {
		t.Fatalf("asked %q", asked)
	}
	if code, _ := wc.do("POST", "/v1/restart", map[string]any{"kind": "restart", "reason": "y"}); code != http.StatusOK {
		t.Errorf("a second open request: %d", code)
	}

	// None of these approve it: a stranger, the person elsewhere, a quote
	// of the request, an edit, the wrong id.
	stranger := chatMsg("2", "approve R1")
	stranger.Trusted = false
	elsewhere := chatMsg("3", "approve R1")
	elsewhere.ID, elsewhere.Conversation = "chat:9:3", "chat:9"
	quoted := chatMsg("4", "(replying to: To go ahead, send a message that says only: approve R1)\nhmm, what does it change?")
	quoted2 := chatMsg("5", "(replying to: approve R1)\nnot yet")
	edit := chatMsg("6", "approve R1")
	edit.Kind = "edit"
	wrong := chatMsg("7", "approve R9")
	for _, in := range []Inbound{stranger, elsewhere, quoted, quoted2, edit, wrong} {
		r.s.Receive(in)
	}
	if up.count() != 0 {
		t.Fatalf("started without the person's approval: %+v", up.started)
	}

	r.s.Receive(chatMsg("8", "(replying to: Permission needed)\nApprove R1"))
	if up.count() != 1 || up.started[0].Ref != "abc123def4567" || up.started[0].Status != Running {
		t.Fatalf("started %+v", up.started)
	}
	r.s.Receive(chatMsg("9", "approve R1"))
	if up.count() != 1 {
		t.Error("approved twice, started twice")
	}
	// The other request waits: one change at a time.
	if code, _ := wc.do("POST", "/v1/restart", map[string]any{"kind": "restart_main", "reason": "z"}); code != http.StatusConflict {
		t.Errorf("asked while R1 runs: %d", code)
	}

	r.s.followApprovals(context.Background())
	if a, _ := r.s.Store.Approval("R1"); a.Status != Running {
		t.Fatalf("finished before its result: %+v", a)
	}
	up.mu.Lock()
	up.done, up.ok, up.text = true, true, "Deployed abc123def4; the main session attached."
	up.mu.Unlock()
	r.s.followApprovals(context.Background())
	a, _ := r.s.Store.Approval("R1")
	if a.Status != Succeeded || a.DecidedBy != "Asha" {
		t.Fatalf("R1 %+v", a)
	}
	got := chat.got()
	if !strings.Contains(got[len(got)-1], "R1 (deploy abc123def4 and restart Samrajya) is done.") {
		t.Errorf("told %q", got[len(got)-1])
	}
	p, _ := r.s.Store.PendingFor(w, 0)
	found := false
	for _, e := range p {
		found = found || (e.Kind == "approval" && strings.Contains(e.Body, "is done"))
	}
	if !found {
		t.Errorf("the worker was not told: %+v", p)
	}
}

func TestARefusedOrUnansweredRequestChangesNothing(t *testing.T) {
	r, chat := chatRig(t)
	up := &fakeUpdater{}
	r.s.SelfUpdate = up
	r.s.Home = "chat:1"
	r.tick()
	main := r.attach(MainAgent)
	if code, out := main.do("POST", "/v1/restart", map[string]any{"kind": "restart_main", "reason": "new role"}); code != http.StatusOK {
		t.Fatalf("ask: %d %v", code, out)
	}
	r.s.Receive(chatMsg("1", "refuse R1"))
	if a, _ := r.s.Store.Approval("R1"); a.Status != Refused || up.count() != 0 {
		t.Errorf("R1 %+v, started %d", a, up.count())
	}
	if got := chat.got(); !strings.Contains(got[len(got)-1], "Refused R1") {
		t.Errorf("told %q", got)
	}

	main.do("POST", "/v1/restart", map[string]any{"kind": "restart", "reason": "again"})
	r.advance(25 * time.Hour)
	r.s.followApprovals(context.Background())
	r.s.Receive(chatMsg("2", "approve R2"))
	if a, _ := r.s.Store.Approval("R2"); a.Status != Expired || up.count() != 0 {
		t.Errorf("R2 %+v, started %d", a, up.count())
	}
}

func TestWithoutSelfUpdateNothingCanBeAsked(t *testing.T) {
	r, _ := chatRig(t)
	r.s.Home = "chat:1"
	r.tick()
	main := r.attach(MainAgent)
	if code, _ := main.do("POST", "/v1/restart", map[string]any{"kind": "restart", "reason": "x"}); code != http.StatusNotImplemented {
		t.Errorf("asked: %d", code)
	}
}
