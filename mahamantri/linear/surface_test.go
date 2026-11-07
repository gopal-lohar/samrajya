package linear

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopal-lohar/samrajya/mahamantri/bridge"
)

type receiver struct{ got []bridge.Inbound }

func (r *receiver) Receive(in bridge.Inbound) error { r.got = append(r.got, in); return nil }

func TestAPingBecomesAMessageInItsIssuesConversation(t *testing.T) {
	r := &receiver{}
	s := &Surface{Receiver: r, Trusted: map[string]bool{"u-owner": true}}
	s.Ingest(Ping{Kind: "mention", Issue: "ENG-1", IssueID: "uuid-1", IssueTitle: "Fix login", Author: "Asha", AuthorID: "u-owner",
		Body: "fix", CommentID: "c1", ThreadID: "t1"}, "d1")
	s.Ingest(Ping{Kind: "reply", Issue: "ENG-1", IssueID: "uuid-1", AuthorID: "u-x", CommentID: "c2", ThreadID: "t1"}, "d2")
	in := r.got[0]
	if in.ID != "linear:c1" || in.Conversation != "linear:uuid-1" || in.Title != "ENG-1: Fix login" || in.Thread != "t1" ||
		in.Ref != "c1" || !in.Trusted || len(in.Links) != 1 || in.Links[0] != "ENG-1" {
		t.Errorf("inbound %+v", in)
	}
	if r.got[1].Trusted {
		t.Error("an author not listed is trusted")
	}
}

func TestCanonicalNamesAnIssueByItsUUID(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "ENG-1") {
			t.Errorf("query %s", body)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issue": map[string]any{"id": "0b6c1f4e-1111-4a2b-9c3d-123456789abc"}}})
	}))
	defer ts.Close()
	s := &Surface{API: &API{Key: "k", URL: ts.URL}}
	got, err := s.Canonical(context.Background(), "linear:ENG-1")
	if err != nil || got != "linear:0b6c1f4e-1111-4a2b-9c3d-123456789abc" {
		t.Errorf("canonical %q %v", got, err)
	}
	if same, _ := s.Canonical(context.Background(), got); same != got {
		t.Errorf("a UUID changed: %q", same)
	}
}
