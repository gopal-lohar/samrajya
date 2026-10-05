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

// PermissionRule is one entry of a session's permission ruleset. Rules are
// evaluated in order and the last one matching wins (verified live against
// v2.0.18: {"*","*","deny"} followed by {"shell","sleep *","allow"} let
// `sleep 15` run and rejected `ls /`). Action is the tool's permission name
// (shell, read, edit, glob, grep, subagent, webfetch, websearch, skill,
// question, external_directory, or <mcp-server>_<tool>); Resource is what
// it acts on - for shell, the command text, matched per command.
type PermissionRule struct {
	Action   string `json:"action" yaml:"action"`
	Resource string `json:"resource" yaml:"resource"`
	Effect   string `json:"effect" yaml:"effect"` // "allow" | "deny" | "ask"
}

type CreateSessionRequest struct {
	Title       string           `json:"title,omitempty"`
	Agent       string           `json:"agent,omitempty"`
	Model       *SessionModel    `json:"model,omitempty"`
	Location    *SessionLocation `json:"location,omitempty"`
	Permissions []PermissionRule `json:"permissions,omitempty"`
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

// ActiveSessions returns the IDs of the sessions running right now; a
// session absent from it is idle.
func (c *Client) ActiveSessions(ctx context.Context) (map[string]bool, error) {
	var active map[string]struct {
		Type string `json:"type"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/session/active", nil, &active); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(active))
	for id, a := range active {
		out[id] = a.Type == "running"
	}
	return out, nil
}

// SetPermissions replaces the session's own permission ruleset, which is
// applied on top of its agent's - how an already-existing session is locked
// down the same way a newly created one is.
func (c *Client) SetPermissions(ctx context.Context, sessionID string, rules []PermissionRule) error {
	return c.doJSON(ctx, http.MethodPatch, "/api/session/"+sessionID, map[string][]PermissionRule{"permissions": rules}, nil)
}

// PutInstruction attaches (or replaces) a durable instruction entry on the
// session. Unlike a message, it is part of the session's instructions on
// every step, so it survives context compaction. The endpoint is marked
// experimental in v2.0.18.
func (c *Client) PutInstruction(ctx context.Context, sessionID, key, value string) error {
	return c.doJSON(ctx, http.MethodPut, "/api/experimental/session/"+sessionID+"/instructions/entries/"+key, map[string]string{"value": value}, nil)
}

func (c *Client) ListModels(ctx context.Context) ([]ModelInfo, error) {
	var models []ModelInfo
	err := c.doJSON(ctx, http.MethodGet, "/api/model", nil, &models)
	return models, err
}

// rawMessage is the part of a message list entry mahamantri reads. Only
// fields it uses are declared: entries also carry metadata whose values are
// objects (e.g. a synthetic message's {"instruction":{"paths":[...]}}), and
// declaring metadata as strings made every such session unreadable.
type rawMessage struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Model struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerID"`
	} `json:"model"`
	Tokens TokenUsage `json:"tokens"`
	Time   struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Content []rawContent `json:"content"`
}

// rawContent is one part of an assistant message: text, reasoning or a tool
// call. Only text is read.
type rawContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// recentAssistant is how many of the newest assistant entries are fetched:
// one turn writes an entry per step, and the newest is often still
// streaming, so a few are needed to find the last completed one.
const recentAssistant = 20

// recentAssistantMessages returns the session's newest assistant entries,
// newest first, using the list endpoint's own type filter and ordering -
// the list is paginated, so reading "everything" only ever saw one page.
func (c *Client) recentAssistantMessages(ctx context.Context, sessionID string) ([]rawMessage, error) {
	var msgs []rawMessage
	path := fmt.Sprintf("/api/session/%s/message?type=assistant&order=desc&limit=%d", sessionID, recentAssistant)
	err := c.doJSON(ctx, http.MethodGet, path, nil, &msgs)
	return msgs, err
}

// LatestAssistantMessage returns the most recent *completed* assistant
// entry on sessionID, for context-usage rotation. Entries still streaming
// carry zero tokens, which would read as "this session uses no context".
func (c *Client) LatestAssistantMessage(ctx context.Context, sessionID string) (AssistantMessage, bool, error) {
	msgs, err := c.recentAssistantMessages(ctx, sessionID)
	if err != nil {
		return AssistantMessage{}, false, err
	}
	latest, ok := latestByCreatedAt(msgs, func(m rawMessage) bool { return m.Time.Completed != 0 })
	if !ok {
		return AssistantMessage{}, false, nil
	}
	return AssistantMessage{ID: latest.ID, ModelID: latest.Model.ID, ProviderID: latest.Model.ProviderID, Tokens: latest.Tokens, Type: latest.Type}, true, nil
}

// LastReply returns the text of the newest assistant entry that has any -
// what a session said at the end of its turn, i.e. its report.
func (c *Client) LastReply(ctx context.Context, sessionID string) (string, error) {
	msgs, err := c.recentAssistantMessages(ctx, sessionID)
	if err != nil {
		return "", err
	}
	latest, ok := latestByCreatedAt(msgs, func(m rawMessage) bool { return replyText(m) != "" })
	if !ok {
		return "", nil
	}
	return replyText(latest), nil
}

func replyText(m rawMessage) string {
	var parts []string
	for _, c := range m.Content {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			parts = append(parts, strings.TrimSpace(c.Text))
		}
	}
	return strings.Join(parts, "\n\n")
}

// latestByCreatedAt returns the message matching keep with the largest
// Time.Created among msgs - comparing the timestamps msgs carry rather than
// trusting the list's order.
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
