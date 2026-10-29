package linear

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeSink struct {
	got []Ping
	err error
}

func (f *fakeSink) Ingest(p Ping, delivery string) error {
	if f.err != nil {
		return f.err
	}
	f.got = append(f.got, p)
	return nil
}

// memThreads is a ThreadStore in memory; failAdd makes learning fail.
type memThreads struct {
	threadSet
	failAdd bool
}

func (m *memThreads) Add(id, issue string) error {
	if m.failAdd {
		return errors.New("disk full")
	}
	m.threadSet[id] = true
	return nil
}

func newHandler(logs *bytes.Buffer) (*Handler, *fakeSink, *memThreads) {
	sink, threads := &fakeSink{}, &memThreads{threadSet: threadSet{}}
	return &Handler{Secret: "s3cret", Self: senapati, Threads: threads, Sink: sink, Logger: log.New(logs, "", 0)}, sink, threads
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

func TestHandlerRecordsAMentionWithItsIdentityButNotItsText(t *testing.T) {
	var logs bytes.Buffer
	h, sink, _ := newHandler(&logs)
	if rec := deliver(t, h, "s3cret", mentionBody, nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(sink.got) != 1 {
		t.Fatalf("ingested = %v", sink.got)
	}
	p := sink.got[0]
	if p.CommentID != "c-1" || p.ThreadID != "c-1" || p.AuthorID != personID || p.Kind != "mention" || p.Issue != "ENG-31" {
		t.Errorf("ping = %+v", p)
	}
	if strings.Contains(logs.String(), "pick this up") {
		t.Error("comment text must not be logged")
	}
}

func TestHandlerRejectsBadSignatureAndStaleTimestamp(t *testing.T) {
	h, sink, _ := newHandler(&bytes.Buffer{})
	if rec := deliver(t, h, "wrong-secret", mentionBody, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad signature: status = %d, want 401", rec.Code)
	}
	stale := func(r *http.Request) {
		r.Header.Set("Linear-Timestamp", strconv.FormatInt(time.Now().Add(-5*time.Minute).UnixMilli(), 10))
	}
	if rec := deliver(t, h, "s3cret", mentionBody, stale); rec.Code != http.StatusBadRequest {
		t.Errorf("stale timestamp: status = %d, want 400", rec.Code)
	}
	if len(sink.got) != 0 {
		t.Errorf("rejected deliveries must not be recorded: %v", sink.got)
	}
}

// Label/status/priority edits are answered 200 and never reach the session.
func TestHandlerDropsIssueEditsAndSaysWhy(t *testing.T) {
	var logs bytes.Buffer
	h, sink, _ := newHandler(&logs)
	edit := `{"action":"update","type":"Issue","updatedFrom":{"labelIds":[]},"data":{"identifier":"ENG-31","title":"T"},"actor":{"id":"` + personID + `","name":"Asha","type":"user"}}`
	if rec := deliver(t, h, "s3cret", edit, nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 so Linear doesn't retry", rec.Code)
	}
	if len(sink.got) != 0 || !strings.Contains(logs.String(), "not recorded") {
		t.Errorf("recorded %v; log:\n%s", sink.got, logs.String())
	}
}

func TestHandlerRecordsRepliesOnlyInSenapatisThreads(t *testing.T) {
	h, sink, _ := newHandler(&bytes.Buffer{})
	deliver(t, h, "s3cret", ownCommentBody, nil)
	deliver(t, h, "s3cret", replyBody(ownCommentID), nil)
	deliver(t, h, "s3cret", replyBody("someone-elses-thread"), nil)
	if len(sink.got) != 1 || sink.got[0].Kind != "reply" || sink.got[0].ThreadID != ownCommentID {
		t.Errorf("ingested = %+v", sink.got)
	}
}

func TestSinkFailureAsksLinearToRetry(t *testing.T) {
	h, sink, _ := newHandler(&bytes.Buffer{})
	sink.err = errors.New("store closed")
	if rec := deliver(t, h, "s3cret", mentionBody, nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 so Linear redelivers", rec.Code)
	}
}

func TestUnlearnableOwnThreadAsksLinearToRetry(t *testing.T) {
	h, _, threads := newHandler(&bytes.Buffer{})
	threads.failAdd = true
	if rec := deliver(t, h, "s3cret", ownCommentBody, nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d: a reply in this thread would later be missed", rec.Code)
	}
}
