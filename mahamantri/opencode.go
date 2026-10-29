package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/bridge"
	"github.com/gopal-lohar/samrajya/mahamantri/config"
	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

const (
	sessionKey     = "opencode.session"
	instructionKey = "samrajya.main"
)

// opencodeMain is the main session in mode opencode: one session on the
// opencode server, created once and remembered in the store, with the main
// role installed as its instructions. Linear pings are prompted into it;
// it reaches the bridge (accepting pings, tasks, actions, brain) through the
// Samrajya MCP server configured in opencode.
type opencodeMain struct {
	client  *opencode.Client
	cfg     config.Config
	store   *bridge.Store
	logger  *log.Logger
	session string
}

func newOpencodeMain(ctx context.Context, cfg config.Config, store *bridge.Store, logger *log.Logger) (*opencodeMain, error) {
	o := &opencodeMain{client: opencode.New(cfg.Opencode.ServerURL, cfg.Opencode.Password), cfg: cfg, store: store, logger: logger}
	if _, err := os.ReadFile(cfg.Opencode.InstructionsFile); err != nil {
		return nil, fmt.Errorf("opencode.instructionsFile: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := o.client.Ping(pingCtx); err != nil {
		return nil, fmt.Errorf("opencode server %s: %w (is `opencode serve` running?)", cfg.Opencode.ServerURL, err)
	}
	if err := o.ensureSession(pingCtx); err != nil {
		return nil, err
	}
	return o, nil
}

// ensureSession reuses the remembered main session or creates one, and
// (re)installs the role, so an edited role file applies on restart.
func (o *opencodeMain) ensureSession(ctx context.Context) error {
	id := o.store.Meta(sessionKey)
	if id != "" {
		if _, err := o.client.GetSession(ctx, id); errors.Is(err, opencode.ErrNotFound) {
			o.logger.Printf("opencode: main session %s is gone; creating a new one", id)
			id = ""
		} else if err != nil {
			return fmt.Errorf("opencode: reading main session %s: %w", id, err)
		}
	}
	if id == "" {
		req := opencode.CreateSessionRequest{Title: "Samrajya", Agent: o.cfg.Opencode.Agent}
		if o.cfg.Bridge.MainDirectory != "" {
			req.Location = &opencode.SessionLocation{Directory: o.cfg.Bridge.MainDirectory}
		}
		if o.cfg.Opencode.Model.ID != "" {
			req.Model = &opencode.SessionModel{ProviderID: o.cfg.Opencode.Model.ProviderID, ID: o.cfg.Opencode.Model.ID}
		}
		info, err := o.client.CreateSession(ctx, req)
		if err != nil {
			return fmt.Errorf("opencode: creating the main session: %w", err)
		}
		id = info.ID
		if err := o.store.SetMeta(sessionKey, id); err != nil {
			return err
		}
		o.logger.Printf("opencode: created main session %s", id)
	}
	role, err := os.ReadFile(o.cfg.Opencode.InstructionsFile)
	if err != nil {
		return err
	}
	if err := o.client.PutInstruction(ctx, id, instructionKey, string(role)); err != nil {
		return fmt.Errorf("opencode: installing the main role: %w", err)
	}
	o.session = id
	return nil
}

// deliver prompts the main session with each pending ping, queued behind
// its current turn. A ping is delivered once per run of mahamantri: it stays
// pending until the session accepts it, so after a restart whatever is
// still unaccepted is delivered again.
func (o *opencodeMain) deliver(ctx context.Context, srv *bridge.Server) {
	var after uint64
	backoff := time.Second
	for ctx.Err() == nil {
		pending, err := srv.WaitPending(ctx, after, 30*time.Second)
		if err == nil {
			for _, e := range pending {
				if err = o.prompt(ctx, e); err != nil {
					break
				}
				after = e.Seq
			}
		}
		if err == nil {
			backoff = time.Second
			continue
		}
		if ctx.Err() != nil {
			return
		}
		o.logger.Printf("opencode: delivering pings: %v (retrying in %s)", err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(2*backoff, time.Minute)
	}
}

func (o *opencodeMain) prompt(ctx context.Context, e bridge.Event) error {
	req := opencode.PromptRequest{Text: renderPing(e), Delivery: "queue", Metadata: map[string]string{"source": "samrajya", "event_id": e.ID}}
	err := o.client.Prompt(ctx, o.session, req)
	if errors.Is(err, opencode.ErrNotFound) {
		// The session was deleted under us: make a new one and try again.
		if err = o.ensureSession(ctx); err == nil {
			err = o.client.Prompt(ctx, o.session, req)
		}
	}
	if err == nil {
		o.logger.Printf("opencode: delivered %s to %s", e.ID, o.session)
	}
	return err
}

// renderPing is a ping as the main session reads it; it matches the channel
// plugin's rendering, with the routing details inline because a prompt has
// no metadata the model sees.
func renderPing(e bridge.Event) string {
	what := "mentioned you"
	if e.Kind == "reply" {
		what = "replied in a thread you are part of"
	}
	issue := "a Linear issue"
	if e.Issue != "" {
		issue = e.Issue
		if e.IssueTitle != "" {
			issue += fmt.Sprintf(" (%q)", e.IssueTitle)
		}
	}
	author := e.Author
	if author == "" {
		author = "someone"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Linear: %s %s on %s.", author, what, issue)
	if !e.Trusted {
		b.WriteString(" They are NOT a trusted requester.")
	}
	b.WriteString("\nComment:\n> " + strings.ReplaceAll(strings.TrimSpace(e.Body), "\n", "\n> ") + "\n")
	if e.URL != "" {
		fmt.Fprintf(&b, "Link: %s\n", e.URL)
	}
	thread := e.ThreadID
	if thread == "" {
		thread = "(new)"
	}
	fmt.Fprintf(&b, "Accept it with samrajya_accept (event_id %s) before working on it; reply in thread %s.", e.ID, thread)
	return b.String()
}
