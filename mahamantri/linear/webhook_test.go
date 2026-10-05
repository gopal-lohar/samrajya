package linear

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeForwarder struct {
	got []string
	err error
}

func (f *fakeForwarder) Forward(text string) error {
	f.got = append(f.got, text)
	return f.err
}

func newHandler(t *testing.T, f *fakeForwarder, logs *bytes.Buffer) *Handler {
	t.Helper()
	threads, err := LoadThreads(filepath.Join(t.TempDir(), "threads.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Handler{Secret: "s3cret", Self: senapati, Threads: threads, Forward: f, Logger: log.New(logs, "", 0)}
}

func deliver(t *testing.T, h http.Handler, secret, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Linear-Event", "Comment")
	req.Header.Set("Linear-Delivery", "d-1")
	req.Header.Set("Linear-Signature", sign(secret, []byte(body)))
	req.Header.Set("Linear-Timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandlerForwardsAMentionWithItsSainik(t *testing.T) {
	f := &fakeForwarder{}
	h := newHandler(t, f, &bytes.Buffer{})
	h.Sainiks = func(issue string) []Sainik {
		return []Sainik{{Label: "sainik-" + issue + "-fix", SessionID: "ses_k", Status: "running"}}
	}
	h.Mahamantri = "http://127.0.0.1:4097"
	if rec := deliver(t, h, "s3cret", mentionBody, nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(f.got) != 1 || !strings.Contains(f.got[0], "[Linear ping] SEN-31") || !strings.Contains(f.got[0], `"sainik-SEN-31-fix" (session ses_k): running`) ||
		!strings.Contains(f.got[0], "http://127.0.0.1:4097/opencode/api/session/ses_k/prompt") {
		t.Errorf("forwarded = %q", f.got)
	}
}

func TestHandlerRejectsBadSignatureAndStaleTimestamp(t *testing.T) {
	f := &fakeForwarder{}
	h := newHandler(t, f, &bytes.Buffer{})
	if rec := deliver(t, h, "wrong-secret", mentionBody, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad signature: status = %d, want 401", rec.Code)
	}
	stale := func(r *http.Request) {
		r.Header.Set("Linear-Timestamp", strconv.FormatInt(time.Now().Add(-5*time.Minute).UnixMilli(), 10))
	}
	if rec := deliver(t, h, "s3cret", mentionBody, stale); rec.Code != http.StatusBadRequest {
		t.Errorf("stale timestamp: status = %d, want 400", rec.Code)
	}
	if len(f.got) != 0 {
		t.Errorf("rejected deliveries must not be forwarded: %q", f.got)
	}
}

// The noise problem: label/status/priority edits each produced a message.
// They are logged, answered 200, and never reach Senapati.
func TestHandlerDropsIssueEditsButStillLogsThem(t *testing.T) {
	f := &fakeForwarder{}
	var logs bytes.Buffer
	h := newHandler(t, f, &logs)
	edit := `{"action":"update","type":"Issue","updatedFrom":{"labelIds":[]},"data":{"identifier":"SEN-31","title":"T"},"actor":{"id":"` + gopalID + `","name":"Gopal","type":"user"}}`
	if rec := deliver(t, h, "s3cret", edit, nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 so Linear doesn't retry", rec.Code)
	}
	if len(f.got) != 0 {
		t.Errorf("an issue edit was forwarded: %q", f.got)
	}
	if !strings.Contains(logs.String(), `"labelIds"`) || !strings.Contains(logs.String(), "not forwarded") {
		t.Errorf("the delivery must still be logged in full, with why it was dropped:\n%s", logs.String())
	}
}

// End to end: Senapati comments, the person replies in that thread, and only
// the reply reaches Senapati - also after a restart (the index is on disk).
func TestHandlerForwardsReplyInSenapatisThreadAcrossRestart(t *testing.T) {
	f := &fakeForwarder{}
	h := newHandler(t, f, &bytes.Buffer{})
	deliver(t, h, "s3cret", ownCommentBody, nil)
	if len(f.got) != 0 {
		t.Fatalf("Senapati's own comment was forwarded: %q", f.got)
	}

	reloaded, err := LoadThreads(h.Threads.path)
	if err != nil {
		t.Fatal(err)
	}
	h.Threads = reloaded
	deliver(t, h, "s3cret", replyBody(ownCommentID), nil)
	if len(f.got) != 1 || !strings.Contains(f.got[0], "replied in a comment thread you are part of") {
		t.Errorf("forwarded = %q", f.got)
	}

	deliver(t, h, "s3cret", replyBody("someone-elses-thread"), nil)
	if len(f.got) != 1 {
		t.Errorf("a reply in a thread Senapati is not in was forwarded: %q", f.got[1:])
	}
}

func TestHandlerStillRespondsOKWhenForwardFails(t *testing.T) {
	f := &fakeForwarder{err: errors.New("queue full")}
	var logs bytes.Buffer
	h := newHandler(t, f, &logs)
	if rec := deliver(t, h, "s3cret", mentionBody, nil); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (Linear must not retry a delivery we logged)", rec.Code)
	}
	if !strings.Contains(logs.String(), "queue full") {
		t.Error("forward failure should be logged")
	}
}
