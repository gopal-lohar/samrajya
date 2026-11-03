package bridge

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/linear"
)

func TestLimitResetsReadsClaudeCodesMessage(t *testing.T) {
	now := time.Date(2026, 11, 2, 16, 38, 0, 0, time.UTC)
	kolkata, _ := time.LoadLocation("Asia/Kolkata")
	for msg, want := range map[string]time.Time{
		"You've hit your 5-hour limit · resets 7:10pm (UTC)":     time.Date(2026, 11, 2, 19, 10, 0, 0, time.UTC),
		"You've hit your 5-hour limit · resets 9am (UTC)":        time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC),
		"You've hit your weekly limit · resets Nov 4, 9am (UTC)": time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC),
		"limit reached · resets 12:30am (Asia/Kolkata)":          time.Date(2026, 11, 3, 0, 30, 0, 0, kolkata),
		"You've hit your limit · resets Jan 2 at 1pm (UTC)":      time.Date(2027, 1, 2, 13, 0, 0, 0, time.UTC),
	} {
		got, ok := limitResets(msg, now)
		if !ok || !got.Equal(want) {
			t.Errorf("%q: %v %v, want %v", msg, got, ok, want)
		}
	}
	for _, msg := range []string{"API Error: Rate limit reached", "resets 7:10pm (Mars/Olympus)", "resets 13pm (UTC)"} {
		if _, ok := limitResets(msg, now); ok {
			t.Errorf("%q parsed", msg)
		}
	}
}

func TestALimitIsShownAndTheSessionWokenWhenItResets(t *testing.T) {
	s, fake, c := linearServer(t)
	now := time.Date(2026, 11, 2, 16, 38, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.Store.now = s.now
	c.attach(false)

	// The hook reports with the token alone: it has no attachment.
	hook := &client{t: t, url: c.url, token: "tok"}
	if code, _ := hook.do("POST", "/v1/limit", map[string]string{"message": "You've hit your 5-hour limit · resets 7:10pm (UTC)"}); code != http.StatusOK {
		t.Fatalf("limit: %d", code)
	}
	if st := s.currentStatus(); st.Label != "At my usage limit until 7:10pm UTC - pings wait until then" {
		t.Errorf("status = %v", st)
	}
	s.Ingest(linear.Ping{Kind: "mention", IssueID: "iss", AuthorID: "u1", CommentID: "c1", ThreadID: "th"}, "d1")
	now = now.Add(20 * time.Minute)
	c.do("GET", "/v1/status", nil) // keep the attachment fresh
	s.mu.Lock()
	s.lastSeen = now
	s.mu.Unlock()
	s.nudge(context.Background(), 15*time.Minute)
	if _, _, cm := fake.got(); len(cm) != 1 || !strings.Contains(cm[0], "usage limit until 7:10pm UTC") {
		t.Errorf("nudge = %v", cm)
	}

	s.wakeAfterLimit()
	if _, out := c.do("GET", "/v1/events?after=1&wait=0", nil); len(out["events"].([]any)) != 0 {
		t.Fatal("woken before the reset")
	}
	now = time.Date(2026, 11, 2, 19, 11, 0, 0, time.UTC)
	s.mu.Lock()
	s.lastSeen = now
	s.mu.Unlock()
	s.wakeAfterLimit()
	s.wakeAfterLimit() // once only
	_, out := c.do("GET", "/v1/events?after=1&wait=0", nil)
	ev := out["events"].([]any)
	if len(ev) != 1 || ev[0].(map[string]any)["kind"] != "limit_reset" {
		t.Fatalf("events after reset = %v", ev)
	}
	if !s.limitedUntil().IsZero() || s.currentStatus() != (Status{}) {
		t.Error("still limited after the reset")
	}
	// A notice is acknowledged without a task; a ping cannot be.
	id := ev[0].(map[string]any)["id"].(string)
	if code, out := c.do("POST", "/v1/accept", map[string]any{"events": []string{id}}); code != http.StatusOK {
		t.Errorf("acknowledge notice: %d %v", code, out)
	}
	if code, _ := c.do("POST", "/v1/accept", map[string]any{"events": []string{"linear:c1"}}); code == http.StatusOK {
		t.Error("a ping was acknowledged without a task")
	}
}

func TestTheSessionActingAgainEndsTheLimit(t *testing.T) {
	s, _, c := linearServer(t)
	c.attach(false)
	c.do("POST", "/v1/limit", map[string]string{"message": "API Error: Rate limit reached"})
	if until := s.limitedUntil(); until.IsZero() || until.Sub(time.Now()) > unknownLimit+time.Minute {
		t.Fatalf("an unreadable limit: until %v", until)
	}
	c.do("POST", "/v1/tasks", NewTask{Title: "x"})
	if !s.limitedUntil().IsZero() {
		t.Error("still limited after the session acted")
	}
}

func TestOneNudgePerThreadAndReason(t *testing.T) {
	s, fake, _ := linearServer(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	s.Store.now = s.now
	for _, id := range []string{"c1", "c2"} {
		s.Ingest(linear.Ping{Kind: "reply", IssueID: "iss", AuthorID: "u1", CommentID: id, ThreadID: "th"}, "d-"+id)
	}
	s.Ingest(linear.Ping{Kind: "mention", IssueID: "iss", AuthorID: "u1", CommentID: "c3", ThreadID: "other"}, "d3")
	now = now.Add(16 * time.Minute)
	s.nudge(context.Background(), 15*time.Minute)
	if _, _, cm := fake.got(); len(cm) != 2 {
		t.Fatalf("nudges = %v", cm)
	}
	// A later ping in the same thread, still offline: covered already.
	s.Ingest(linear.Ping{Kind: "reply", IssueID: "iss", AuthorID: "u1", CommentID: "c4", ThreadID: "th"}, "d4")
	now = now.Add(16 * time.Minute)
	s.nudge(context.Background(), 15*time.Minute)
	if _, _, cm := fake.got(); len(cm) != 2 {
		t.Errorf("nudged the same thread twice: %v", cm)
	}
}
