package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gopal-lohar/samrajya/mahamantri/attention"
	"github.com/gopal-lohar/samrajya/mahamantri/config"
	"github.com/gopal-lohar/samrajya/mahamantri/linear"
	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
	"github.com/gopal-lohar/samrajya/mahamantri/senapati"
	"github.com/gopal-lohar/samrajya/mahamantri/ui"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: mahamantri [config.yaml]   (default ./mahamantri.yaml)")
	}
	flag.Parse()
	configPath := flag.Arg(0)
	if configPath == "" {
		configPath = "mahamantri.yaml"
	}
	if err := run(configPath); err != nil {
		fmt.Fprintln(os.Stderr, "mahamantri:", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	logFile, err := os.OpenFile(cfg.LogFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	logger := log.New(logFile, "", log.LstdFlags)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Everything that can fail on startup fails here, before a Senapati
	// session is created and before the TUI takes over the screen, with a
	// message that says what to fix.
	client := opencode.New(cfg.Opencode.ServerURL, cfg.Opencode.Password)
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = client.Ping(pingCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("%s", connectionProblem(cfg, configPath, err, false))
	}

	attnLn, err := net.Listen("tcp", cfg.Attention.ListenAddr)
	if err != nil {
		return fmt.Errorf("cannot listen on attention.listenAddr %s: %w (is another mahamantri running?)", cfg.Attention.ListenAddr, err)
	}
	linearLn, err := net.Listen("tcp", cfg.Linear.ListenAddr)
	if err != nil {
		return fmt.Errorf("cannot listen on linear.listenAddr %s: %w (is another mahamantri running?)", cfg.Linear.ListenAddr, err)
	}

	raw := make(chan opencode.Event, 64)
	go client.Run(ctx, raw)
	bcast := attention.NewBroadcaster()
	go bcast.Run(ctx, raw)

	reg := attention.NewRegistry(cfg.Attention.RegistryFile, client)
	if err := reg.ReloadFromDisk(); err != nil {
		return fmt.Errorf("reading %s: %w (fix or delete it)", cfg.Attention.RegistryFile, err)
	}
	go reg.PollFile(ctx, 3*time.Second)

	// Fail now, not at the next rotation, if the instructions file is unreadable.
	if _, err := senapati.LoadInstructions(cfg.Senapati.InstructionsFile); err != nil {
		return err
	}
	mgr := senapati.New(client, cfg.State.SenapatiFile, cfg.Senapati.RotationThreshold, reg)
	mgr.SetBriefing(func() (string, error) {
		instructions, err := senapati.LoadInstructions(cfg.Senapati.InstructionsFile)
		if err != nil {
			return "", err
		}
		return senapati.Briefing(senapati.BriefingInfo{
			LinearUserID: cfg.Linear.BotUserID,
			LinearName:   cfg.Linear.BotName,
			LinearHandle: cfg.Linear.BotHandle,
			OpencodeURL:  cfg.Opencode.ServerURL,
			AttentionURL: httpURL(cfg.Attention.ListenAddr),
			Instructions: instructions,
		}), nil
	})
	createReq := opencode.CreateSessionRequest{Agent: cfg.Senapati.Agent}
	if cfg.Senapati.Directory != "" {
		createReq.Location = &opencode.SessionLocation{Directory: cfg.Senapati.Directory}
	}
	if cfg.Senapati.Model.ID != "" {
		createReq.Model = &opencode.SessionModel{ID: cfg.Senapati.Model.ID, ProviderID: cfg.Senapati.Model.ProviderID}
	}
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	senapatiID, err := mgr.Start(startCtx, createReq)
	cancel()
	if err != nil {
		return err
	}
	reg.Protect(senapatiID) // must happen before the attention API starts serving
	logger.Printf("senapati session: %s (%s)", senapatiID, mgr.Current().Title)
	go mgr.Run(ctx)

	// One subscriber, in order: Observe first so the registry's status is
	// settled for this event, then the takeover/attention checks that may
	// override or act on it.
	events := bcast.Subscribe(64)
	go func() {
		for ev := range events {
			reg.Observe(ev)
			instID, ok := reg.Owner(ev.SessionID)
			if !ok {
				continue
			}
			inst, ok := reg.Get(instID)
			if !ok {
				continue
			}
			// Judged on the registered session itself only: its subagents
			// receive task prompts from opencode that carry no tag.
			if action, manual := attention.DetectManualTakeover(ev); manual && ev.SessionID == instID {
				reg.MarkManual(instID, "a person intervened")
				forward(logger, mgr, senapati.SummarizeManualTakeover(inst, action))
				continue
			}
			if reg.NeedsAttention(ev) {
				forward(logger, mgr, senapati.SummarizeEvent(ev, inst))
			}
		}
	}()

	lookup := func(ctx context.Context, id string) (string, error) {
		info, err := client.GetSession(ctx, id)
		return info.Title, err
	}
	attnSrv := attention.NewServer(reg, bcast, lookup)
	linearSrv := &http.Server{Handler: linear.NewHandler(cfg.Linear.SigningSecret, linear.Identity{UserID: cfg.Linear.BotUserID, Name: cfg.Linear.BotName, Handle: cfg.Linear.BotHandle}, mgr, logger)}
	go attnSrv.Serve(attnLn)
	go linearSrv.Serve(linearLn)

	// Editing the config while running: the password takes effect at once
	// (opencode serve picks a new one every start); anything else needs a
	// restart, and the TUI says so instead of silently ignoring it.
	var configNotice atomic.Value
	go config.Watch(ctx, configPath, 2*time.Second, func(c config.Config, err error) {
		if err != nil {
			configNotice.Store(filepath.Base(configPath) + " not applied: " + err.Error())
			return
		}
		client.SetPassword(c.Opencode.Password)
		running, reloaded := cfg, c
		running.Opencode.Password, reloaded.Opencode.Password = "", ""
		if running != reloaded {
			configNotice.Store(filepath.Base(configPath) + " changed - restart mahamantri to apply everything except the password")
			return
		}
		configNotice.Store("")
	})

	problems := func() []string {
		var out []string
		if connected, err := client.Connection(); !connected {
			out = append(out, connectionProblem(cfg, configPath, err, true))
		}
		if err := mgr.LastError(); err != nil {
			out = append(out, err.Error())
		}
		if cfg.Senapati.Model.ID == "" {
			out = append(out, "senapati.model is not set - Senapati runs on the opencode server's default model, not the one you pick in the TUI")
		}
		if notice, _ := configNotice.Load().(string); notice != "" {
			out = append(out, notice)
		}
		return out
	}

	_, tuiErr := tea.NewProgram(ui.New(reg, mgr, client.AttachCommand, problems)).Run()

	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	attnSrv.Shutdown(shutdownCtx)
	linearSrv.Shutdown(shutdownCtx)
	return tuiErr
}

// httpURL turns a listen address into a URL another process can call.
func httpURL(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "http://" + listenAddr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func forward(logger *log.Logger, mgr *senapati.Manager, text string) {
	if err := mgr.Forward(text); err != nil {
		logger.Printf("could not queue message for senapati: %v", err)
	}
}

// connectionProblem turns a connect/auth failure into a line that says what
// is wrong and what to do about it. At startup (running=false) it can be
// long and explain the fix; inside the TUI it must fit on one terminal row.
func connectionProblem(cfg config.Config, configPath string, err error, running bool) string {
	name := filepath.Base(configPath)
	switch {
	case err == nil:
		return "connecting to the opencode server..."
	case errors.Is(err, opencode.ErrUnauthorized) && running:
		return "opencode rejected the password - update opencode.password in " + name + " (applied live)"
	case errors.Is(err, opencode.ErrUnauthorized):
		return fmt.Sprintf("opencode server %s rejected the password in %s (opencode.password). "+
			"`opencode serve` picks a new password on every start - put the current one there and start mahamantri again, "+
			"or start the server as OPENCODE_SERVER_PASSWORD=<fixed> opencode serve so it never changes",
			cfg.Opencode.ServerURL, name)
	case running:
		return fmt.Sprintf("cannot reach opencode at %s (%v) - retrying", cfg.Opencode.ServerURL, err)
	default:
		return fmt.Sprintf("cannot reach the opencode server at %s: %v (is it running?)", cfg.Opencode.ServerURL, err)
	}
}
