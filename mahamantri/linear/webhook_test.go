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

type fakeForwarder struct {
	got []string
	err error
}

func (f *fakeForwarder) Forward(text string) error {
	f.got = append(f.got, text)
	return f.err
}

func deliver(t *testing.T, h http.HandlerFunc, secret, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Linear-Event", "Issue")
	req.Header.Set("Linear-Delivery", "d-1")
	req.Header.Set("Linear-Signature", sign(secret, []byte(body)))
	req.Header.Set("Linear-Timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

const issueBody = `{"action":"update","type":"Issue","updatedFrom":{"assigneeId":null},"data":{"identifier":"SEN-30","title":"T","assignee":{"name":"Senapati"}},"actor":{"id":"user-1","name":"Gopal"}}`

func TestHandlerForwardsVerifiedDelivery(t *testing.T) {
	f := &fakeForwarder{}
	h := NewHandler("s3cret", Identity{}, f, log.New(&bytes.Buffer{}, "", 0))
	if rec := deliver(t, h, "s3cret", issueBody, nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(f.got) != 1 || !strings.Contains(f.got[0], "assigned to Senapati") {
		t.Errorf("forwarded = %q", f.got)
	}
}

func TestHandlerRejectsBadSignatureAndStaleTimestamp(t *testing.T) {
	f := &fakeForwarder{}
	h := NewHandler("s3cret", Identity{}, f, log.New(&bytes.Buffer{}, "", 0))
	if rec := deliver(t, h, "wrong-secret", issueBody, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad signature: status = %d, want 401", rec.Code)
	}
	stale := func(r *http.Request) {
		r.Header.Set("Linear-Timestamp", strconv.FormatInt(time.Now().Add(-5*time.Minute).UnixMilli(), 10))
	}
	if rec := deliver(t, h, "s3cret", issueBody, stale); rec.Code != http.StatusBadRequest {
		t.Errorf("stale timestamp: status = %d, want 400", rec.Code)
	}
	if len(f.got) != 0 {
		t.Errorf("rejected deliveries must not be forwarded: %q", f.got)
	}
}

// The bot's own Linear actions must not be fed back to it as new prompts.
func TestHandlerSkipsBotsOwnActionsButStillLogsThem(t *testing.T) {
	f := &fakeForwarder{}
	var logs bytes.Buffer
	h := NewHandler("s3cret", Identity{UserID: "user-1"}, f, log.New(&logs, "", 0))
	if rec := deliver(t, h, "s3cret", issueBody, nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 so Linear doesn't retry", rec.Code)
	}
	if len(f.got) != 0 {
		t.Errorf("bot's own action was forwarded: %q", f.got)
	}
	if !strings.Contains(logs.String(), "SEN-30") {
		t.Error("the delivery must still be logged in full")
	}
}

func TestHandlerStillRespondsOKWhenForwardFails(t *testing.T) {
	f := &fakeForwarder{err: errors.New("queue full")}
	var logs bytes.Buffer
	h := NewHandler("s3cret", Identity{}, f, log.New(&logs, "", 0))
	if rec := deliver(t, h, "s3cret", issueBody, nil); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (Linear must not retry a delivery we logged)", rec.Code)
	}
	if !strings.Contains(logs.String(), "queue full") {
		t.Error("forward failure should be logged")
	}
}
