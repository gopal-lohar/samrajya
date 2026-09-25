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

func TestLatestAssistantMessage(t *testing.T) {
	// Live-verified: GET /api/session/{id}/message returns newest-first.
	// Listed in that order, with distinct Time.Created values, so the test
	// exercises timestamp comparison rather than list order.
	user := rawMessage{ID: "msg_3", Type: "user"}
	user.Time.Created = 300
	done := rawMessage{ID: "msg_2", Type: "assistant", Tokens: TokenUsage{Input: 100, Output: 50}}
	done.Model.ID, done.Model.ProviderID = "gpt-6-sol", "openai"
	done.Time.Created, done.Time.Completed = 200, 250
	older := rawMessage{ID: "msg_1", Type: "assistant", Tokens: TokenUsage{Input: 1}}
	older.Time.Created, older.Time.Completed = 100, 150
	// A newer, still-streaming step with no tokens yet must be skipped.
	streaming := rawMessage{ID: "msg_4", Type: "assistant"}
	streaming.Time.Created = 400

	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []rawMessage{streaming, user, done, older}})
	})

	asst, ok, err := c.LatestAssistantMessage(context.Background(), "ses_1")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if asst.ID != "msg_2" || asst.ModelID != "gpt-6-sol" || asst.Tokens.Input != 100 {
		t.Errorf("LatestAssistantMessage = %+v, want the newest completed one (msg_2)", asst)
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
