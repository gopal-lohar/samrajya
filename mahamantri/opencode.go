package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
// role installed as its instructions. Messages are prompted into it; it
// reaches the bridge (accepting them, tasks, actions, brain) through the
// Samrajya MCP server configured in opencode, which attaches as the main
// agent with the fixed secret in <agents.stateDir>/main.secret. There are no
// workers in this mode.
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
	secret, err := store.FixMainSecret()
	if err != nil {
		return nil, err
	}
	if err := writeSecret(filepath.Join(cfg.Agents.StateDir, bridge.MainAgent+".secret"), secret); err != nil {
		return nil, fmt.Errorf("main secret: %w", err)
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
	req := opencode.PromptRequest{Text: renderPrompt(e), Delivery: "queue", Metadata: map[string]string{"source": "samrajya", "event_id": e.ID}}
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

// renderPrompt is an event as the main session reads it: a prompt has no
// metadata the model sees, so how to accept and answer it is spelled out.
func renderPrompt(e bridge.Event) string {
	text := bridge.RenderEvent(e)
	if e.Source == "samrajya" {
		return text + fmt.Sprintf("\nAcknowledge it with samrajya_accept (event_id %s); it needs no task.", e.ID)
	}
	thread := e.ThreadID
	if thread == "" {
		thread = "(none)"
	}
	return text + fmt.Sprintf("\nAccept it with samrajya_accept (event_id %s) before working on it; answer with samrajya_reply in conversation %s, thread %s.",
		e.ID, e.Conversation, thread)
}
