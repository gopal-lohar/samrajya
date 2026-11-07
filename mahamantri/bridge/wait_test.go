package bridge

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// A waiter is handed each event once, wakes when one arrives, and a newer
// waiter replaces an older one.
func TestAWaiterIsHandedEachEventOnce(t *testing.T) {
	r, _ := chatRig(t)
	r.tick()
	main := mustAgent(t, r, MainAgent)
	hook := &client{t: t, url: r.url, token: "tok"}
	wait := func(secs int) map[string]any {
		_, out := hook.do("POST", "/v1/wait", map[string]any{"agent": MainAgent, "secret": main.Secret, "seconds": secs})
		return out
	}
	if code, _ := hook.do("POST", "/v1/wait", map[string]any{"agent": MainAgent, "secret": "stale", "seconds": 0}); code != http.StatusForbidden {
		t.Errorf("stale secret: %d", code)
	}
	if out := wait(0); out["events"] != float64(0) {
		t.Fatalf("nothing waiting: %v", out)
	}
	got := make(chan map[string]any, 1)
	go func() { got <- wait(10) }()
	time.Sleep(100 * time.Millisecond)
	r.s.Receive(chatMsg("1", "hello there"))
	select {
	case out := <-got:
		text, _ := out["text"].(string)
		if out["events"] != float64(1) || !strings.Contains(text, "> hello there") ||
			!strings.Contains(text, "samrajya_accept (event_id chat:1:1)") || !strings.Contains(text, "samrajya_reply in conversation chat:1, thread 1") {
			t.Fatalf("handed %v", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter was not woken")
	}
	if a := mustAgent(t, r, MainAgent); !a.InTurn {
		t.Error("a hand-over starts a turn")
	}
	if out := wait(0); out["events"] != float64(0) {
		t.Errorf("handed over twice: %v", out)
	}
	// A newer waiter replaces an older one.
	older := make(chan map[string]any, 1)
	go func() { older <- wait(10) }()
	time.Sleep(100 * time.Millisecond)
	go wait(1)
	select {
	case out := <-older:
		if out["superseded"] != true {
			t.Errorf("older waiter: %v", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the older waiter was not replaced")
	}
}

// A session that stopped listening is relaunched with what waits for it in
// its brief; one that listens is left alone.
func TestADeafSessionIsRelaunchedWithItsEvents(t *testing.T) {
	r, _ := chatRig(t)
	r.tick()
	r.attach(MainAgent)
	r.s.Receive(chatMsg("1", "are you there?"))
	r.advance(2 * time.Minute)
	r.keepAlive()
	r.s.mu.Lock()
	r.s.lastWaiter[MainAgent] = r.now // listening
	r.s.mu.Unlock()
	r.tick()
	if got := r.l.launched(); len(got) != 1 {
		t.Fatalf("a listening session was relaunched: %v", got)
	}
	r.advance(time.Minute)
	r.keepAlive()
	r.tick() // stops it
	r.tick() // launches it again
	got := r.l.launched()
	if len(got) != 2 || got[1] != "samrajya-main-g2" {
		t.Fatalf("launched %v", got)
	}
	if b := r.l.briefs["samrajya-main-g2"]; !strings.Contains(b, "are you there?") {
		t.Errorf("brief %q", b)
	}
}
