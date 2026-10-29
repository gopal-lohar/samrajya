// Package opencode is the little of an opencode server's API (v2,
// `opencode serve`) that mode opencode uses: one main session, its
// instructions, and prompting it with each Linear ping.
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
	"time"
)

var (
	ErrNotFound     = errors.New("opencode: not found")
	ErrUnauthorized = errors.New("opencode: unauthorized - check opencode.password")
)

type Client struct {
	BaseURL  string
	Password string
	HTTP     *http.Client
}

func New(baseURL, password string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Password: password, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type SessionModel struct {
	ID         string `json:"id,omitempty"`
	ProviderID string `json:"providerID,omitempty"`
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
	ID    string `json:"id"`
	Title string `json:"title"`
}

// PromptRequest sends a message into a session. Delivery "queue" waits for
// the current turn to end; "steer" interrupts it.
type PromptRequest struct {
	Text     string            `json:"text"`
	Delivery string            `json:"delivery,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Ping makes one cheap authenticated request, so a wrong password or an
// unreachable server is reported up front.
func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/api/session/active", nil, nil)
}

func (c *Client) CreateSession(ctx context.Context, req CreateSessionRequest) (SessionInfo, error) {
	var info SessionInfo
	err := c.do(ctx, http.MethodPost, "/api/session", req, &info)
	return info, err
}

func (c *Client) GetSession(ctx context.Context, id string) (SessionInfo, error) {
	var info SessionInfo
	err := c.do(ctx, http.MethodGet, "/api/session/"+id, nil, &info)
	return info, err
}

func (c *Client) Prompt(ctx context.Context, id string, req PromptRequest) error {
	return c.do(ctx, http.MethodPost, "/api/session/"+id+"/prompt", req, nil)
}

// PutInstruction attaches (or replaces) a durable instruction entry on the
// session: part of its instructions on every step, so it survives context
// compaction. Experimental in opencode v2.0.18.
func (c *Client) PutInstruction(ctx context.Context, id, key, value string) error {
	return c.do(ctx, http.MethodPut, "/api/experimental/session/"+id+"/instructions/entries/"+key, map[string]string{"value": value}, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
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
	req.SetBasicAuth("opencode", c.Password)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case resp.StatusCode == http.StatusNotFound:
		return withDetail(ErrNotFound, resp.Body)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return withDetail(fmt.Errorf("opencode: %s %s: status %d", method, path, resp.StatusCode), resp.Body)
	case out == nil || resp.StatusCode == http.StatusNoContent:
		return nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	// Single resources come wrapped as {"data": ...}.
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(data, &envelope) == nil && envelope.Data != nil {
		data = envelope.Data
	}
	return json.Unmarshal(data, out)
}

// withDetail appends the server's own explanation to err, so a rejected
// request says why instead of just its status.
func withDetail(err error, body io.Reader) error {
	data, _ := io.ReadAll(io.LimitReader(body, 400))
	if detail := strings.TrimSpace(string(data)); detail != "" {
		return fmt.Errorf("%w: %s", err, detail)
	}
	return err
}
