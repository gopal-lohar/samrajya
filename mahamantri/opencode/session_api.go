package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var (
	ErrSessionNotFound = errors.New("opencode: session not found")
	ErrConflict        = errors.New("opencode: conflict")
	ErrBadRequest      = errors.New("opencode: bad request")
	ErrUnauthorized    = errors.New("opencode: unauthorized")
)

type SessionModel struct {
	ID         string `json:"id,omitempty"`
	ProviderID string `json:"providerID,omitempty"`
	Variant    string `json:"variant,omitempty"`
}

type SessionLocation struct {
	Directory string `json:"directory,omitempty"`
}

type CreateSessionRequest struct {
	Title    string           `json:"title,omitempty"`
	Agent    string           `json:"agent,omitempty"`
	Model    *SessionModel    `json:"model,omitempty"`
	Location *SessionLocation `json:"location,omitempty"`
}

type SessionInfo struct {
	ID      string `json:"id"`
	Agent   string `json:"agent"`
	Title   string `json:"title"`
	Outcome string `json:"outcome"`
	Time    struct {
		Created  int64 `json:"created"`
		Updated  int64 `json:"updated"`
		Idle     int64 `json:"idle"`
		Viewed   int64 `json:"viewed"`
		Archived int64 `json:"archived"`
	} `json:"time"`
}

// PromptRequest sends a message into an existing session. Metadata is a
// freeform, persisted tag (live-verified to round-trip exactly as sent via
// GET /api/session/{id}/message) - mahamantri uses it to mark messages it
// sent itself, since the real opencode TUI never sets metadata at all.
type PromptRequest struct {
	Text     string            `json:"text"`
	Delivery string            `json:"delivery,omitempty"` // "steer" | "queue"
	Metadata map[string]string `json:"metadata,omitempty"`
}

type PromptResponse struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
}

type ModelLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

type ModelInfo struct {
	ID         string     `json:"id"`
	ProviderID string     `json:"providerID"`
	Limit      ModelLimit `json:"limit"`
}

// AssistantMessage carries which model answered and how many tokens it
// used - the live source for context-usage rotation, since Session.Info's
// own tokens field was observed empty in this build.
type AssistantMessage struct {
	ID         string     `json:"id"`
	ModelID    string     `json:"-"`
	ProviderID string     `json:"-"`
	Tokens     TokenUsage `json:"tokens"`
	Type       string     `json:"type"`
}

type TokenUsage struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Cache     struct {
		Read  int `json:"read"`
		Write int `json:"write"`
	} `json:"cache"`
}

func (c *Client) CreateSession(ctx context.Context, req CreateSessionRequest) (SessionInfo, error) {
	var info SessionInfo
	err := c.doJSON(ctx, http.MethodPost, "/api/session", req, &info)
	return info, err
}

func (c *Client) GetSession(ctx context.Context, sessionID string) (SessionInfo, error) {
	var info SessionInfo
	err := c.doJSON(ctx, http.MethodGet, "/api/session/"+sessionID, nil, &info)
	return info, err
}

func (c *Client) Prompt(ctx context.Context, sessionID string, req PromptRequest) (PromptResponse, error) {
	var resp PromptResponse
	err := c.doJSON(ctx, http.MethodPost, "/api/session/"+sessionID+"/prompt", req, &resp)
	return resp, err
}

// SwitchModel changes the model used for the session's subsequent turns.
func (c *Client) SwitchModel(ctx context.Context, sessionID string, model SessionModel) error {
	return c.doJSON(ctx, http.MethodPost, "/api/session/"+sessionID+"/model", map[string]SessionModel{"model": model}, nil)
}

// SwitchAgent changes the agent used for the session's subsequent turns.
func (c *Client) SwitchAgent(ctx context.Context, sessionID, agent string) error {
	return c.doJSON(ctx, http.MethodPost, "/api/session/"+sessionID+"/agent", map[string]string{"agent": agent}, nil)
}

func (c *Client) ListModels(ctx context.Context) ([]ModelInfo, error) {
	var models []ModelInfo
	err := c.doJSON(ctx, http.MethodGet, "/api/model", nil, &models)
	return models, err
}

// rawMessage is the wire shape shared by user and assistant message list
// entries - just enough fields to route to the right typed struct above.
type rawMessage struct {
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	Metadata map[string]string `json:"metadata"`
	Model    struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerID"`
	} `json:"model"`
	Tokens TokenUsage `json:"tokens"`
	Cost   float64    `json:"cost"`
	Text   string     `json:"text"` // user messages carry their text at the top level
	Time   struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Content []rawContent `json:"content"`
}

// rawContent is one part of an assistant message: reasoning, text, or a
// tool call with its live state.
type rawContent struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Text  string `json:"text"`
	State struct {
		Status string         `json:"status"`
		Input  map[string]any `json:"input"`
	} `json:"state"`
}

// LatestAssistantMessage returns the most recent *completed*
// assistant-authored message on sessionID, for context-usage rotation
// checking. A single logical turn produces several assistant message
// entries (one per agent step - reasoning, tool calls, final reply), and
// live testing showed the newest one is very often still streaming with
// zero tokens recorded - skipping incomplete entries (Time.Completed == 0)
// avoids reading a transient zero as "this session uses no context."
func (c *Client) LatestAssistantMessage(ctx context.Context, sessionID string) (AssistantMessage, bool, error) {
	msgs, err := c.listMessages(ctx, sessionID)
	if err != nil {
		return AssistantMessage{}, false, err
	}
	latest, ok := latestByCreatedAt(msgs, func(m rawMessage) bool {
		return m.Type == "assistant" && m.Time.Completed != 0
	})
	if !ok {
		return AssistantMessage{}, false, nil
	}
	return AssistantMessage{ID: latest.ID, ModelID: latest.Model.ID, ProviderID: latest.Model.ProviderID, Tokens: latest.Tokens, Type: latest.Type}, true, nil
}

// latestByCreatedAt returns the message matching keep with the largest
// Time.Created among msgs - found by comparing timestamps directly rather
// than assuming a particular list order. Live testing against the real
// server showed GET /api/session/{id}/message returns newest-first, but
// relying on that ordering (as an earlier version of this function did) is
// fragile - comparing the timestamp msgs actually carry is not.
func latestByCreatedAt(msgs []rawMessage, keep func(rawMessage) bool) (rawMessage, bool) {
	var latest rawMessage
	found := false
	for _, m := range msgs {
		if !keep(m) {
			continue
		}
		if !found || m.Time.Created > latest.Time.Created {
			latest = m
			found = true
		}
	}
	return latest, found
}

func (c *Client) listMessages(ctx context.Context, sessionID string) ([]rawMessage, error) {
	var msgs []rawMessage
	err := c.doJSON(ctx, http.MethodGet, "/api/session/"+sessionID+"/message", nil, &msgs)
	return msgs, err
}

// doJSON issues one request/response call against the opencode REST API
// (Basic auth, JSON body in/out). Uses ctx's own deadline, never a
// client-level timeout - Client.HTTP is deliberately timeout-less because
// it's shared with the long-lived SSE Run() loop.
func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetBasicAuth("opencode", c.password())

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusOK, http.StatusCreated:
		if out == nil {
			return nil
		}
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		// The list endpoints (/api/model, /api/session/{id}/message) return
		// a bare array/object, not the {"data": ...} envelope that the
		// single-resource endpoints use - try the envelope first, fall back
		// to unmarshalling the body directly.
		if err := json.Unmarshal(data, &envelope); err == nil && envelope.Data != nil {
			return json.Unmarshal(envelope.Data, out)
		}
		return json.Unmarshal(data, out)
	case http.StatusBadRequest:
		return withDetail(ErrBadRequest, resp.Body)
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusNotFound:
		return withDetail(ErrSessionNotFound, resp.Body)
	case http.StatusConflict:
		return withDetail(ErrConflict, resp.Body)
	default:
		return withDetail(fmt.Errorf("opencode: unexpected status %d", resp.StatusCode), resp.Body)
	}
}

// withDetail appends the server's own explanation to err, so a rejected
// request says why (e.g. an unknown model) instead of just "bad request".
// errors.Is against the sentinel still works.
func withDetail(err error, body io.Reader) error {
	data, _ := io.ReadAll(io.LimitReader(body, 400))
	detail := strings.TrimSpace(string(data))
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}
