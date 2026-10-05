package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "pw")
	return c
}

func TestCreateSession(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/session" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"data": SessionInfo{ID: "ses_1", Agent: "build"}})
	})
	info, err := c.CreateSession(context.Background(), CreateSessionRequest{Title: "Senapati-1"})
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "ses_1" {
		t.Errorf("ID = %q, want ses_1", info.ID)
	}
}

func TestGetSessionNotFound(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if _, err := c.GetSession(context.Background(), "ses_missing"); err != ErrSessionNotFound {
		t.Errorf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestPromptSendsMetadataAndDelivery(t *testing.T) {
	var got PromptRequest
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"data": PromptResponse{ID: "msg_1", SessionID: "ses_1"}})
	})
	_, err := c.Prompt(context.Background(), "ses_1", PromptRequest{
		Text:     "hello",
		Delivery: "queue",
		Metadata: map[string]string{"source": "mahamantri"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "hello" || got.Delivery != "queue" || got.Metadata["source"] != "mahamantri" {
		t.Errorf("request body = %+v", got)
	}
}

func TestPromptConflict(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	})
	if _, err := c.Prompt(context.Background(), "ses_1", PromptRequest{Text: "x"}); err != ErrConflict {
		t.Errorf("err = %v, want ErrConflict", err)
	}
}

func TestListModels(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/model" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"data": []ModelInfo{
			{ID: "gpt-6-sol", ProviderID: "openai", Limit: ModelLimit{Context: 1048576, Output: 131072}},
		}})
	})
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].Limit.Context != 1048576 {
		t.Errorf("models = %+v", models)
	}
}

// The shape of a real GET /api/session/{id}/message?type=assistant&order=desc
// page from opencode 2.0.18: a {data, cursor} envelope, newest first, entries
// with object-valued metadata, a still-streaming newest step, tool parts.
const assistantPage = `{"data":[
 {"id":"msg_4","type":"assistant","metadata":{"instruction":{"paths":["/repo/AGENTS.md"]}},"time":{"created":400},"model":{"id":"gpt-6-sol","providerID":"openai"},"content":[{"type":"reasoning","text":"thinking"}]},
 {"id":"msg_3","type":"assistant","time":{"created":300,"completed":350},"model":{"id":"gpt-6-sol","providerID":"openai"},"tokens":{"input":100,"output":50,"reasoning":0,"cache":{"read":5000,"write":0}},
  "content":[{"type":"text","text":"Plan:\n1. do it"},{"type":"text","text":"Questions: none"}]},
 {"id":"msg_2","type":"assistant","time":{"created":200,"completed":250},"model":{"id":"gpt-6-sol","providerID":"openai"},"tokens":{"input":1},
  "content":[{"type":"tool","name":"shell","state":{"status":"completed","input":{"command":"ls"}}}]}
],"cursor":{"previous":"p","next":"n"}}`

func TestRecentAssistantMessagesAsksForANewestFirstAssistantPage(t *testing.T) {
	var query string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Path + "?" + r.URL.RawQuery
		w.Write([]byte(assistantPage))
	})
	if _, _, err := c.LatestAssistantMessage(context.Background(), "ses_1"); err != nil {
		t.Fatal(err)
	}
	if query != "/api/session/ses_1/message?type=assistant&order=desc&limit=20" {
		t.Errorf("query = %q", query)
	}
}

// Regression: object-valued metadata made this fail with "cannot unmarshal
// object into Go struct field .metadata.instruction of type string".
func TestLatestAssistantMessageSkipsStreamingStepsAndToleratesAnyMetadata(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(assistantPage)) })
	asst, ok, err := c.LatestAssistantMessage(context.Background(), "ses_1")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if asst.ID != "msg_3" || asst.ModelID != "gpt-6-sol" || asst.Tokens.Cache.Read != 5000 {
		t.Errorf("LatestAssistantMessage = %+v, want the newest completed one (msg_3)", asst)
	}
}

func TestLastReplyIsTheNewestText(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(assistantPage)) })
	got, err := c.LastReply(context.Background(), "ses_1")
	if err != nil || got != "Plan:\n1. do it\n\nQuestions: none" {
		t.Errorf("LastReply = %q, %v", got, err)
	}
}

func TestPingAndUnauthorized(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if _, pw, _ := r.BasicAuth(); pw != "right" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	})
	if err := c.Ping(context.Background()); err != ErrUnauthorized {
		t.Errorf("wrong password: err = %v, want ErrUnauthorized", err)
	}
	c.SetPassword("right")
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("after SetPassword: err = %v, want nil", err)
	}
}

func TestAttachCommandTargetsThisServerWithItsPassword(t *testing.T) {
	c := New("http://localhost:4096/", "Ets_S-8o")
	want := "OPENCODE_SERVER_PASSWORD=Ets_S-8o opencode --server http://localhost:4096 --session ses_abc"
	if got := c.AttachCommand("ses_abc"); got != want {
		t.Errorf("AttachCommand = %q, want %q", got, want)
	}
	c.SetPassword("it's a $pw")
	if got := c.AttachCommand("ses_abc"); !strings.Contains(got, `OPENCODE_SERVER_PASSWORD='it'\''s a $pw'`) {
		t.Errorf("password with shell metacharacters not quoted: %q", got)
	}
}

func TestSwitchModelAndAgentSendTheRightBodies(t *testing.T) {
	var gotPath, gotBody string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.URL.Path, string(b)
		w.WriteHeader(http.StatusNoContent) // what the real server answers
	})
	if err := c.SwitchModel(context.Background(), "ses_1", SessionModel{ID: "gpt-6-sol", ProviderID: "openai"}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/session/ses_1/model" || gotBody != `{"model":{"id":"gpt-6-sol","providerID":"openai"}}` {
		t.Errorf("model request = %s %s", gotPath, gotBody)
	}
	if err := c.SwitchAgent(context.Background(), "ses_1", "senapati"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/session/ses_1/agent" || gotBody != `{"agent":"senapati"}` {
		t.Errorf("agent request = %s %s", gotPath, gotBody)
	}
}

func TestRejectedRequestCarriesServerExplanation(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"message":"unknown model openai/nope"}`))
	})
	err := c.SwitchModel(context.Background(), "ses_1", SessionModel{ID: "nope", ProviderID: "openai"})
	if !errors.Is(err, ErrBadRequest) || !strings.Contains(err.Error(), "unknown model openai/nope") {
		t.Errorf("err = %v, want ErrBadRequest carrying the server's message", err)
	}
}
