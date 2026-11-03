package linear

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIReactsAndSetsStatusAsTheKeysUser(t *testing.T) {
	var got []map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "lin_api_k" {
			w.Write([]byte(`{"errors":[{"message":"Authentication required"}]}`))
			return
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		got = append(got, req)
		q := req["query"].(string)
		switch {
		case strings.Contains(q, "viewer"):
			w.Write([]byte(`{"data":{"viewer":{"id":"u-bot"}}}`))
		case strings.Contains(q, "reactionCreate"):
			w.Write([]byte(`{"data":{"reactionCreate":{"success":true}}}`))
		case strings.Contains(q, "userUpdate"):
			w.Write([]byte(`{"data":{"userUpdate":{"success":true}}}`))
		}
	}))
	defer ts.Close()
	a := &API{Key: "lin_api_k", URL: ts.URL}
	ctx := context.Background()
	if err := a.React(ctx, "c1", "👀"); err != nil {
		t.Fatal(err)
	}
	if in := got[0]["variables"].(map[string]any)["input"].(map[string]any); in["commentId"] != "c1" || in["emoji"] != "👀" {
		t.Errorf("reaction input = %v", in)
	}
	if err := a.SetStatus(ctx, "🟢", "Online"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetStatus(ctx, "", ""); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 { // the viewer is looked up once
		t.Fatalf("%d requests", len(got))
	}
	v := got[2]["variables"].(map[string]any)
	if v["id"] != "u-bot" || v["input"].(map[string]any)["statusLabel"] != "Online" {
		t.Errorf("status variables = %v", v)
	}
	if in := got[3]["variables"].(map[string]any)["input"].(map[string]any); in["statusLabel"] != nil {
		t.Errorf("clearing sent %v", in)
	}

	bad := &API{Key: "wrong", URL: ts.URL}
	if err := bad.React(ctx, "c1", "👀"); err == nil || !strings.Contains(err.Error(), "Authentication required") {
		t.Errorf("bad key: %v", err)
	}
}

func TestAPICommentsInAThreadAndLooksUpHandles(t *testing.T) {
	var got []map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		got = append(got, req)
		switch q := req["query"].(string); {
		case strings.Contains(q, "commentCreate"):
			w.Write([]byte(`{"data":{"commentCreate":{"success":true,"comment":{"id":"c9","url":"u","parent":{"id":"th"},"issue":{"id":"0b0e6f0c-1a1a-4b4b-8c8c-000000000001","identifier":"ENG-1"}}}}}`))
		case strings.Contains(q, "issue(id"):
			w.Write([]byte(`{"data":{"issue":{"id":"0b0e6f0c-1a1a-4b4b-8c8c-000000000001"}}}`))
		default:
			w.Write([]byte(`{"data":{"user":{"displayName":"asha"}}}`))
		}
	}))
	defer ts.Close()
	a := &API{Key: "k", URL: ts.URL}
	p, err := a.Comment(context.Background(), "ENG-1", "th", "hi")
	if err != nil {
		t.Fatal(err)
	}
	// An issue key is looked up first: the mutation takes its id.
	in := got[1]["variables"].(map[string]any)["input"].(map[string]any)
	if in["issueId"] != "0b0e6f0c-1a1a-4b4b-8c8c-000000000001" || in["parentId"] != "th" || in["body"] != "hi" {
		t.Errorf("comment input = %v", in)
	}
	if p.ID != "c9" || p.ThreadID != "th" || p.Issue != "ENG-1" {
		t.Errorf("posted = %+v", p)
	}
	if h, err := a.Handle(context.Background(), "u1"); err != nil || h != "asha" {
		t.Errorf("handle = %q, %v", h, err)
	}
}
