// Mahamantri is Samrajya's core: it takes messages from every surface
// (Linear, Telegram) into a durable inbox, routes them to the agent that
// owns their work, runs and supervises the agents (a main session and a
// worker per piece of work), and keeps the tasks, the journal of external
// actions and the brain. See README.md.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/brain"
	"github.com/gopal-lohar/samrajya/mahamantri/bridge"
	"github.com/gopal-lohar/samrajya/mahamantri/config"
	"github.com/gopal-lohar/samrajya/mahamantri/linear"
	"github.com/gopal-lohar/samrajya/mahamantri/telegram"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: mahamantri [config.yaml]   run (default ./mahamantri.yaml)
       mahamantri check config.yaml                   validate a config
       mahamantri migrate-config old.yaml new.yaml    convert a config from before surfaces
       mahamantri downgrade-store store.db            mark a store schema 1, to roll back`)
	}
	flag.Parse()
	withClaudeOnPath()
	var err error
	switch flag.Arg(0) {
	case "check":
		err = checkConfig(flag.Arg(1))
	case "migrate-config":
		err = migrateConfig(flag.Arg(1), flag.Arg(2))
	case "downgrade-store":
		err = downgradeStore(flag.Arg(1))
	default:
		configPath := flag.Arg(0)
		if configPath == "" {
			configPath = "mahamantri.yaml"
		}
		err = run(configPath)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mahamantri:", err)
		os.Exit(1)
	}
}

// run serves the bridge: surfaces into the durable inbox, the local API the
// sessions' tools use, and (mode claude) the supervisor that runs the
// sessions. Ctrl+C stops mahamantri; the sessions it launched keep running
// and reattach when it is back.
func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(cfg.LogFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	logger := log.New(logFile, "", log.LstdFlags)

	store, err := bridge.Open(cfg.Bridge.Store)
	if err != nil {
		return fmt.Errorf("bridge.store %s: %w", cfg.Bridge.Store, err)
	}
	defer store.Close()
	if n, err := store.LinkOpenIssues(); err != nil {
		return err
	} else if n > 0 {
		logger.Printf("bridge: linked %d open tasks to their Linear issues", n)
	}
	if _, err := store.EnsureMain(); err != nil {
		return err
	}

	token, err := newToken()
	if err != nil {
		return err
	}
	srv := &bridge.Server{Store: store, Token: token, Logger: logger, Home: cfg.Home, FilesDir: filepath.Join(cfg.Agents.StateDir, "files")}
	if cfg.Brain != "" {
		if srv.Brain, err = brain.Open(cfg.Brain, func() []string { return projects(cfg.Workspace) }); err != nil {
			return fmt.Errorf("brain %s: %w", cfg.Brain, err)
		}
	}
	workers := cfg.Mode == "claude"

	var lin *linear.Surface
	if cfg.LinearOn() {
		lin = &linear.Surface{Receiver: srv, ReceivedEmoji: cfg.Linear.ReceivedEmoji}
		if cfg.Linear.APIKey != "" {
			lin.API = &linear.API{Key: cfg.Linear.APIKey, URL: cfg.Linear.APIURL}
		}
		if len(cfg.Linear.TrustedUserIDs) > 0 {
			lin.Trusted = map[string]bool{}
			for _, id := range cfg.Linear.TrustedUserIDs {
				lin.Trusted[id] = true
			}
		}
		srv.AddSurface(lin, bridge.Policy{StartsWork: workers && *cfg.Linear.StartsWork, Reopen: true, NudgeAfter: cfg.Linear.NudgeAfter})
	}
	var bot *telegram.Bot
	if cfg.TelegramOn() {
		bot = &telegram.Bot{Token: cfg.Telegram.BotToken, URL: cfg.Telegram.APIURL, ReceivedEmoji: cfg.Telegram.ReceivedEmoji, Logger: logger,
			TrustedUsers: map[int64]bool{}, Chats: map[int64]bool{}}
		for _, id := range cfg.Telegram.TrustedUserIDs {
			bot.TrustedUsers[id] = true
		}
		for _, id := range cfg.Telegram.ChatIDs {
			bot.Chats[id] = true
		}
		srv.AddSurface(bot, bridge.Policy{StartsWork: workers && cfg.Telegram.StartsWork, NudgeAfter: cfg.Telegram.NudgeAfter})
	}

	var sv *bridge.Supervisor
	if workers {
		sv = &bridge.Supervisor{Store: store, Server: srv, Logger: logger, Prefix: cfg.Agents.NamePrefix, MaxWorkers: cfg.Agents.MaxWorkers,
			Launcher: &claudeLauncher{
				command: cfg.Agents.Command, dir: cfg.Bridge.MainDirectory, discovery: cfg.Bridge.DiscoveryFile,
				stateDir: cfg.Agents.StateDir,
				mainRole: cfg.Agents.MainRole, workerRole: cfg.Agents.WorkerRole,
				mainArgs: cfg.Agents.MainArgs, workerArgs: cfg.Agents.WorkerArgs,
				remote: cfg.Agents.RemoteControl, workerRC: cfg.Agents.WorkerRemote,
				store: store, run: runIn, service: cfg.SelfUpdate.Service,
			}}
		srv.Supervisor = sv
	}
	if cfg.SelfUpdate.Code != "" {
		up := newSelfUpdater(cfg.SelfUpdate.Code, cfg.SelfUpdate.Script, cfg.SelfUpdate.Service, filepath.Join(cfg.Agents.StateDir, "approvals"))
		srv.SelfUpdate = up
		logger.Printf("self-update: codebase %s, release %s, script %s", cfg.SelfUpdate.Code, orUnknown(up.running), up.script)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var oc *opencodeMain
	if cfg.Mode == "opencode" {
		if oc, err = newOpencodeMain(ctx, cfg, store, logger); err != nil {
			return err
		}
	}

	bridgeLn, err := net.Listen("tcp", cfg.Bridge.ListenAddr)
	if err != nil {
		return fmt.Errorf("cannot listen on bridge.listenAddr %s: %w (is another mahamantri running?)", cfg.Bridge.ListenAddr, err)
	}
	base := httpURL(bridgeLn.Addr().String())
	if err := writeDiscovery(cfg.Bridge.DiscoveryFile, base, token, cfg.Bridge.MainDirectory); err != nil {
		return fmt.Errorf("bridge.discoveryFile %s: %w", cfg.Bridge.DiscoveryFile, err)
	}
	defer os.Remove(cfg.Bridge.DiscoveryFile)

	servers := []*http.Server{{Handler: srv, ReadHeaderTimeout: 5 * time.Second}}
	listeners := []net.Listener{bridgeLn}
	if lin != nil {
		linearLn, err := net.Listen("tcp", cfg.Linear.ListenAddr)
		if err != nil {
			bridgeLn.Close()
			return fmt.Errorf("cannot listen on linear.listenAddr %s: %w (is another mahamantri running?)", cfg.Linear.ListenAddr, err)
		}
		listeners = append(listeners, linearLn)
		servers = append(servers, &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: &linear.Handler{
			Secret:  cfg.Linear.SigningSecret,
			Self:    linear.Identity{UserID: cfg.Linear.BotUserID, Name: cfg.Linear.BotName, Handle: cfg.Linear.BotHandle},
			Threads: store,
			Sink:    lin,
			Logger:  logger,
		}})
	}
	errs := make(chan error, len(servers))
	for i := range servers {
		go func() { errs <- servers[i].Serve(listeners[i]) }()
	}
	if bot != nil {
		go bot.Run(ctx, srv)
	}
	if oc != nil {
		go oc.deliver(ctx, srv)
	}
	if sv != nil {
		go sv.Loop(ctx, 15*time.Second)
	}
	statusDone := make(chan struct{})
	go func() { srv.Watch(ctx, 20*time.Second); close(statusDone) }()

	rec, err := store.Recovery()
	if err != nil {
		return err
	}
	var surfaces []string
	if lin != nil {
		surfaces = append(surfaces, "linear (webhook on "+listeners[1].Addr().String()+")")
	}
	if bot != nil {
		surfaces = append(surfaces, "telegram")
	}
	if len(surfaces) == 0 {
		surfaces = append(surfaces, "none")
	}
	logger.Printf("mahamantri (%s): bridge on %s, surfaces: %s, store %s", cfg.Mode, base, strings.Join(surfaces, ", "), cfg.Bridge.Store)
	fmt.Fprintf(os.Stderr, "mahamantri (%s): bridge %s, surfaces: %s\n"+
		"  store %s - %d pending messages, %d open tasks, %d unresolved actions\n",
		cfg.Mode, base, strings.Join(surfaces, ", "), cfg.Bridge.Store, len(rec.Pending), len(rec.Open), len(rec.Uncertain))
	if sv != nil {
		fmt.Fprintf(os.Stderr, "  agents: a main and up to %d workers, sessions named %s-*\n", cfg.Agents.MaxWorkers, cfg.Agents.NamePrefix)
	}
	if oc != nil {
		fmt.Fprintf(os.Stderr, "  main session: opencode session %s\n", oc.session)
	}

	select {
	case <-ctx.Done():
	case err = <-errs:
		logger.Printf("server stopped: %v", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(shutdownCtx)
	}
	stop()
	<-statusDone // leaves the status offline
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// writeDiscovery tells the plugin where the bridge is, the token for this
// run and where the main session runs, readable only by this user.
func writeDiscovery(path, url, token, mainDir string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(mainDir); err == nil {
		mainDir = real
	}
	data, err := json.Marshal(map[string]any{"url": url, "token": token, "pid": os.Getpid(), "mainDirectory": mainDir})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// projects lists the project directories in the workspace.
func projects(workspace string) []string {
	if workspace == "" {
		return nil
	}
	entries, err := os.ReadDir(workspace)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out
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

// withClaudeOnPath puts Claude Code's usual install directory on PATH, which
// a systemd user service does not have. exec looks a command up in
// mahamantri's own PATH, so a child's environment would be too late.
func withClaudeOnPath() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, ".local", "bin")
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p == dir {
			return
		}
	}
	os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown (not a release)"
	}
	return s
}
