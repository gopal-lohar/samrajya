package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/attention"
	"github.com/gopal-lohar/samrajya/mahamantri/claudecode"
	"github.com/gopal-lohar/samrajya/mahamantri/config"
	"github.com/gopal-lohar/samrajya/mahamantri/control"
	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
	"github.com/gopal-lohar/samrajya/mahamantri/senapati"
)

// setupOpencode runs sessions on an already-running opencode server.
func setupOpencode(ctx context.Context, cfg config.Config, configPath string, raw chan<- opencode.Event, role func(string) string) (backend, error) {
	client := opencode.New(cfg.Opencode.ServerURL, cfg.Opencode.Password)
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := client.Ping(pingCtx)
	cancel()
	if err != nil {
		return backend{}, fmt.Errorf("%s", connectionProblem(cfg, configPath, err, false))
	}

	// Senapati can call mahamantri and use Linear, nothing else - so the
	// work has nowhere to go but a sainik (see senapati.Permissions).
	createReq := opencode.CreateSessionRequest{
		Agent:       cfg.Senapati.Agent,
		Permissions: senapati.Permissions(httpURL(cfg.Attention.ListenAddr), cfg.Senapati.ExtraPermissions),
	}
	if cfg.Senapati.Directory != "" {
		createReq.Location = &opencode.SessionLocation{Directory: cfg.Senapati.Directory}
	}
	if cfg.Senapati.Model.ID != "" {
		createReq.Model = &opencode.SessionModel{ID: cfg.Senapati.Model.ID, ProviderID: cfg.Senapati.Model.ProviderID}
	}

	return backend{
		parents:   client,
		sender:    client,
		createReq: createReq,
		replies:   client.LastReply,
		active:    client.ActiveSessions,
		lookup: func(ctx context.Context, id string) (string, error) {
			info, err := client.GetSession(ctx, id)
			return info.Title, err
		},
		routes: func(mux *http.ServeMux, reg *attention.Registry) {
			control.Routes(mux, client, reg, control.Options{
				Directory:    cfg.Sainik.Directory,
				DefaultModel: cfg.Sainik.DefaultModel,
				Agent:        cfg.Sainik.Agent,
				Permissions:  senapati.SainikPermissions(),
				Workspace:    cfg.Workspace,
				Role:         role,
				RoleKey:      senapati.SainikInstructionKey,
			})
		},
		attach: client.AttachCommand,
		problems: func() []string {
			if connected, err := client.Connection(); !connected {
				return []string{connectionProblem(cfg, configPath, err, true)}
			}
			return nil
		},
		onConfig: func(c config.Config) { client.SetPassword(c.Opencode.Password) },
		start:    func(ctx context.Context) { go client.Run(ctx, raw) },
		shutdown: func() {},
	}, nil
}

// setupClaude runs sessions on Claude Code, one `claude -p` process per busy
// session (see package claudecode).
func setupClaude(ctx context.Context, cfg config.Config, base string, raw chan<- opencode.Event, logger *log.Logger, role func(string) string) (backend, error) {
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	version, err := exec.CommandContext(vctx, cfg.Claude.Binary, "--version").Output()
	cancel()
	if err != nil {
		return backend{}, fmt.Errorf("cannot run claude.binary %q: %v (is Claude Code installed and logged in?)", cfg.Claude.Binary, err)
	}
	logger.Printf("claude harness: %s", strings.TrimSpace(string(version)))

	cc, err := claudecode.New(claudecode.Options{
		Binary:         cfg.Claude.Binary,
		StateFile:      cfg.Claude.StateFile,
		IdleExit:       cfg.Claude.IdleExit,
		ContextWindows: cfg.Claude.ContextWindows,
		Logger:         logger,
	}, raw)
	if err != nil {
		return backend{}, fmt.Errorf("reading %s: %w (fix or delete it)", cfg.Claude.StateFile, err)
	}

	createReq := opencode.CreateSessionRequest{
		Agent:    cfg.Senapati.Agent,
		Location: &opencode.SessionLocation{Directory: firstNonEmpty(cfg.Senapati.Directory, cfg.Dir)},
		// Non-nil so the Manager re-applies Senapati's lockdown flags to a
		// resumed session (ClaudeSender.SetPermissions).
		Permissions: []opencode.PermissionRule{},
	}
	if cfg.Senapati.Model.ID != "" {
		createReq.Model = &opencode.SessionModel{ID: cfg.Senapati.Model.ID}
	}
	sender := &senapati.ClaudeSender{
		H:             cc,
		Args:          senapati.ClaudeSenapatiArgs(base, cfg.Claude.SenapatiAllowedTools, cfg.Claude.SenapatiDeniedTools),
		FallbackModel: cfg.Senapati.FallbackModel,
	}

	return backend{
		parents:   noParents{},
		sender:    sender,
		createReq: createReq,
		replies:   cc.LastReply,
		active:    cc.ActiveSessions,
		lookup: func(_ context.Context, id string) (string, error) {
			info, err := cc.Info(id)
			return info.Title, err
		},
		routes: func(mux *http.ServeMux, reg *attention.Registry) {
			control.ClaudeRoutes(mux, cc, reg, control.ClaudeOptions{
				Directory:    cfg.Sainik.Directory,
				DefaultModel: cfg.Sainik.DefaultModel,
				Agent:        cfg.Sainik.Agent,
				Args:         senapati.ClaudeSainikArgs(cfg.Claude.SainikArgs),
				Workspace:    cfg.Workspace,
				Role:         role,
			})
		},
		attach: func(id string) string {
			info, err := cc.Info(id)
			if err != nil {
				return "claude --resume " + id
			}
			cmd := fmt.Sprintf("cd %s && claude --resume %s", shellQuote(info.Dir), id)
			if info.State == "running" {
				return fmt.Sprintf("mid-turn - stop it first: curl -s -X POST %s/claude/session/%s/interrupt ; then: %s", base, id, cmd)
			}
			return cmd
		},
		problems: func() []string { return claudeProblems(cc, cfg.Claude.UsageWarnAt) },
		onConfig: func(config.Config) {},
		start:    func(ctx context.Context) { go cc.Run(ctx) },
		shutdown: cc.Shutdown,
	}, nil
}

// claudeProblems are the TUI's red lines on the Claude Code harness: a
// subscription window near or at its limit, and sessions paused by one.
func claudeProblems(cc *claudecode.Harness, warnAt float64) []string {
	var out []string
	for _, l := range cc.Limits() {
		if l.Status != "" && l.Status != "allowed" {
			line := l.Provider + " usage limit reached"
			if l.ResetsAt != nil {
				line += " - resets " + l.ResetsAt.Format("15:04 MST")
			}
			out = append(out, line)
			continue
		}
		for name, w := range l.Windows {
			if w.Utilization >= warnAt {
				out = append(out, fmt.Sprintf("%s %s usage at %.0f%% - resets %s", l.Provider, strings.ReplaceAll(name, "_", "-"),
					w.Utilization*100, w.ResetsAt.Format("Jan 2 15:04 MST")))
			}
		}
	}
	for _, s := range cc.Sessions() {
		if s.State == "limited" && s.LimitedUntil != nil {
			out = append(out, fmt.Sprintf("%s is paused by a usage limit until %s", s.Title, s.LimitedUntil.Format("15:04 MST")))
		}
	}
	return out
}

// noParents: Claude Code subagents run inside their session's process, so no
// session has a parent session.
type noParents struct{}

func (noParents) SessionParent(string) (string, bool) { return "", false }

func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
