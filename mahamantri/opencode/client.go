package opencode

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Client struct {
	BaseURL  string
	HTTP     *http.Client
	sessions *sessionTracker

	mu       sync.RWMutex
	pw       string
	connOK   bool
	connProb error
}

func New(baseURL, password string) *Client {
	return &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		HTTP:     &http.Client{},
		sessions: newSessionTracker(),
		pw:       password,
	}
}

// SetPassword swaps the credential used by every subsequent request, so a
// restarted opencode server's new password can be applied without
// restarting this program (the SSE loop retries with it on its own).
func (c *Client) SetPassword(password string) {
	c.mu.Lock()
	c.pw = password
	c.mu.Unlock()
}

func (c *Client) password() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pw
}

// Connection reports whether the SSE event stream is currently established
// and, if not, why the last attempt failed.
func (c *Client) Connection() (connected bool, problem error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connOK, c.connProb
}

func (c *Client) setConnection(ok bool, problem error) {
	c.mu.Lock()
	c.connOK, c.connProb = ok, problem
	c.mu.Unlock()
}

// Ping makes one cheap authenticated request, so a wrong password or an
// unreachable server is reported up front rather than as a failure deep
// inside session creation.
func (c *Client) Ping(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodGet, "/api/session/active", nil, nil)
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// AttachCommand is the command that opens sessionID in the real opencode
// TUI against *this* server. Without --server the CLI talks to the default
// background service, which doesn't have sessions created on another server.
func (c *Client) AttachCommand(sessionID string) string {
	return fmt.Sprintf("OPENCODE_SERVER_PASSWORD=%s opencode --server %s --session %s",
		shellQuote(c.password()), shellQuote(c.BaseURL), shellQuote(sessionID))
}

// Run connects to GET {BaseURL}/api/event with HTTP Basic auth and streams
// classified Events to out until ctx is cancelled. It reconnects with capped
// exponential backoff, resuming via Last-Event-ID, and emits synthetic
// client.* Events around each disconnect/reconnect so a dropped connection
// (which has no server-side explanatory event) is still visible downstream.
func (c *Client) Run(ctx context.Context, out chan<- Event) {
	var lastEventID string
	backoff := 500 * time.Millisecond
	const maxBackoff = 10 * time.Second
	failed := false

	for {
		if ctx.Err() != nil {
			return
		}
		newID, err := c.connect(ctx, lastEventID, out)
		if newID != "" {
			lastEventID = newID
		}
		if ctx.Err() != nil {
			return
		}
		c.setConnection(false, err)
		if err != nil {
			failed = true
			sendSynthetic(ctx, out, "client.disconnected", Warning, err.Error())
			sendSynthetic(ctx, out, "client.reconnecting", Warning, fmt.Sprintf("retrying in %s", backoff))
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		if failed {
			sendSynthetic(ctx, out, "client.reconnected", Info, "connection restored")
			failed = false
		}
		backoff = 500 * time.Millisecond
	}
}

// SessionParent returns the parentID observed for sessionID via
// session.created/session.updated events seen so far, and whether anything
// has been observed for it at all. Lets other packages (e.g. attention/)
// walk session ancestry without this package exporting sessionTracker.
func (c *Client) SessionParent(sessionID string) (parentID string, ok bool) {
	return c.sessions.parent(sessionID)
}

func sendSynthetic(ctx context.Context, out chan<- Event, typ string, sev Severity, summary string) {
	select {
	case out <- Event{Type: typ, Severity: sev, Summary: summary}:
	case <-ctx.Done():
	}
}

// connect runs one connection's read loop. It returns the last-seen SSE
// event id (for Last-Event-ID resume) and an error if the connection failed
// or was rejected (e.g. wrong password).
func (c *Client) connect(ctx context.Context, lastEventID string, out chan<- Event) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/event", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.SetBasicAuth("opencode", c.password())
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return "", ErrUnauthorized
		}
		return "", fmt.Errorf("opencode server returned %d", resp.StatusCode)
	}
	c.setConnection(true, nil)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var dataLines []string
	for scanner.Scan() {
		if ctx.Err() != nil {
			return lastEventID, ctx.Err()
		}
		line := scanner.Text()
		switch {
		case line == "":
			if len(dataLines) > 0 {
				raw := []byte(strings.Join(dataLines, "\n"))
				dataLines = nil
				env, err := decodeEnvelope(raw)
				if err == nil {
					select {
					case out <- classify(env, c.sessions):
					case <-ctx.Done():
						return lastEventID, ctx.Err()
					}
				}
			}
		case strings.HasPrefix(line, ":"):
			// SSE comment/heartbeat, ignore.
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case strings.HasPrefix(line, "id:"):
			lastEventID = strings.TrimPrefix(strings.TrimPrefix(line, "id:"), " ")
		}
	}
	if err := scanner.Err(); err != nil {
		return lastEventID, err
	}
	return lastEventID, fmt.Errorf("event stream closed")
}
