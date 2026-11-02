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
		if strings.Contains(req["query"].(string), "commentCreate") {
			w.Write([]byte(`{"data":{"commentCreate":{"success":true}}}`))
		} else {
			w.Write([]byte(`{"data":{"user":{"displayName":"asha"}}}`))
		}
	}))
	defer ts.Close()
	a := &API{Key: "k", URL: ts.URL}
	if err := a.Comment(context.Background(), "iss", "th", "hi"); err != nil {
		t.Fatal(err)
	}
	if in := got[0]["variables"].(map[string]any)["input"].(map[string]any); in["issueId"] != "iss" || in["parentId"] != "th" || in["body"] != "hi" {
		t.Errorf("comment input = %v", in)
	}
	if h, err := a.Handle(context.Background(), "u1"); err != nil || h != "asha" {
		t.Errorf("handle = %q, %v", h, err)
	}
}
