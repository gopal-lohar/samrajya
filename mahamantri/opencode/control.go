package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// Interrupt stops whatever the session is doing right now - a running tool
// call included - and leaves it idle with its context intact. Verified live:
// a running `sleep` was killed within a second, and a prompt sent right
// after was answered in seconds. Messages alone cannot do this: `steer`
// delivery still waits for the tool call to finish.
func (c *Client) Interrupt(ctx context.Context, sessionID string) error {
	return c.doJSON(ctx, http.MethodPost, "/api/session/"+sessionID+"/interrupt", nil, nil)
}

// Proxy returns a handler that forwards requests to the opencode server with
// this client's current credentials added, after removing stripPrefix from
// the path. Whoever calls it gets the whole opencode API - every operation,
// not a curated subset - without ever holding the password. Streaming
// responses (the event feed) are flushed as they arrive.
func (c *Client) Proxy(stripPrefix string) http.Handler {
	target, err := url.Parse(c.BaseURL)
	if err != nil {
		panic(fmt.Sprintf("opencode: bad base URL %q: %v", c.BaseURL, err))
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, stripPrefix)
			pr.Out.URL.RawPath = ""
			pr.Out.Header.Del("Authorization")
			pr.Out.SetBasicAuth("opencode", c.password())
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "opencode server unreachable: "+err.Error(), http.StatusBadGateway)
		},
	}
}

// Snapshot is a compact answer to "what is this session doing right now?" -
// enough for a manager to decide whether to step in, without reading the
// whole transcript.
type Snapshot struct {
	SessionID  string        `json:"sessionID"`
	Title      string        `json:"title"`
	State      string        `json:"state"` // "running" | "idle"
	RunningFor string        `json:"runningFor,omitempty"`
	Doing      *ToolActivity `json:"doing,omitempty"` // the tool call executing at this moment
	LastText   string        `json:"lastText,omitempty"`
	Model      string        `json:"model,omitempty"`
	CostUSD    float64       `json:"costUSD"`
	Messages   int           `json:"messages"`
}

type ToolActivity struct {
	Tool  string `json:"tool"`
	Input string `json:"input"`
}

func (c *Client) Snapshot(ctx context.Context, sessionID string) (Snapshot, error) {
	info, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	var active map[string]struct {
		Type string `json:"type"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/session/active", nil, &active); err != nil {
		return Snapshot{}, err
	}
	msgs, err := c.listMessages(ctx, sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	return buildSnapshot(info, msgs, active[sessionID].Type == "running", time.Now()), nil
}

func buildSnapshot(info SessionInfo, msgs []rawMessage, running bool, now time.Time) Snapshot {
	s := Snapshot{SessionID: info.ID, Title: info.Title, State: "idle", Messages: len(msgs)}
	if running {
		s.State = "running"
	}

	var latestAsst rawMessage
	var latestUser int64
	haveAsst := false
	for _, m := range msgs {
		switch m.Type {
		case "assistant":
			s.CostUSD += m.Cost
			if !haveAsst || m.Time.Created > latestAsst.Time.Created {
				latestAsst, haveAsst = m, true
			}
		case "user":
			if m.Time.Created > latestUser {
				latestUser = m.Time.Created
			}
		}
	}
	if running && latestUser > 0 {
		s.RunningFor = now.Sub(time.UnixMilli(latestUser)).Round(time.Second).String()
	}
	if haveAsst {
		s.Model = latestAsst.Model.ProviderID + "/" + latestAsst.Model.ID
		for _, c := range latestAsst.Content {
			if c.Type == "tool" && c.State.Status == "running" {
				s.Doing = &ToolActivity{Tool: c.Name, Input: describeInput(c.State.Input)}
			}
		}
	}
	// The last thing it said: newest assistant message that has any text.
	var newest int64
	for _, m := range msgs {
		if m.Type != "assistant" || m.Time.Created < newest {
			continue
		}
		for _, c := range m.Content {
			if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
				s.LastText, newest = truncate(strings.TrimSpace(c.Text), 500), m.Time.Created
			}
		}
	}
	return s
}

// describeInput renders a tool call's input as one short string: its command
// if it has one, otherwise the JSON.
func describeInput(in map[string]any) string {
	if cmd, ok := in["command"].(string); ok {
		return truncate(cmd, 300)
	}
	data, _ := json.Marshal(in)
	return truncate(string(data), 300)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
