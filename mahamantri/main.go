// Mahamantri is the bridge between Linear and Samrajya's main assistant
// session: it keeps the durable inbox of Linear pings, the tasks they
// become, the journal of external actions and the brain, and serves them to
// the session over a local, authenticated API. See README.md.
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

// run serves the bridge: the Linear webhook into the durable inbox, and the
// local API the main session's tools use. It never stops the main session
// itself; Ctrl+C stops mahamantri only.
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

	store, err := bridge.Open(cfg.Bridge.Store, cfg.Bridge.TrustedUserIDs)
	if err != nil {
		return fmt.Errorf("bridge.store %s: %w", cfg.Bridge.Store, err)
	}
	defer store.Close()

	token, err := newToken()
	if err != nil {
		return err
	}
	srv := &bridge.Server{Store: store, Token: token, Logger: logger}
	if cfg.Brain != "" {
		if srv.Brain, err = brain.Open(cfg.Brain, func() []string { return projects(cfg.Workspace) }); err != nil {
			return fmt.Errorf("brain %s: %w", cfg.Brain, err)
		}
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
	linearLn, err := net.Listen("tcp", cfg.Linear.ListenAddr)
	if err != nil {
		bridgeLn.Close()
		return fmt.Errorf("cannot listen on linear.listenAddr %s: %w (is another mahamantri running?)", cfg.Linear.ListenAddr, err)
	}
	base := httpURL(bridgeLn.Addr().String())
	if err := writeDiscovery(cfg.Bridge.DiscoveryFile, base, token, cfg.Bridge.MainDirectory); err != nil {
		return fmt.Errorf("bridge.discoveryFile %s: %w", cfg.Bridge.DiscoveryFile, err)
	}
	defer os.Remove(cfg.Bridge.DiscoveryFile)

	bridgeSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 5 * time.Second}
	linearSrv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: &linear.Handler{
		Secret:  cfg.Linear.SigningSecret,
		Self:    linear.Identity{UserID: cfg.Linear.BotUserID, Name: cfg.Linear.BotName, Handle: cfg.Linear.BotHandle},
		Threads: store,
		Sink:    srv,
		Logger:  logger,
	}}
	errs := make(chan error, 2)
	go func() { errs <- bridgeSrv.Serve(bridgeLn) }()
	go func() { errs <- linearSrv.Serve(linearLn) }()
	if oc != nil {
		go oc.deliver(ctx, srv)
	}

	rec, err := store.Recovery()
	if err != nil {
		return err
	}
	logger.Printf("mahamantri (%s): bridge on %s, Linear webhook on %s, store %s", cfg.Mode, base, linearLn.Addr(), cfg.Bridge.Store)
	fmt.Fprintf(os.Stderr, "mahamantri (%s): bridge %s, Linear webhook %s\n"+
		"  store %s - %d pending pings, %d open tasks, %d unresolved actions\n",
		cfg.Mode, base, linearLn.Addr(), cfg.Bridge.Store, len(rec.Pending), len(rec.Open), len(rec.Uncertain))
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
	linearSrv.Shutdown(shutdownCtx)
	bridgeSrv.Shutdown(shutdownCtx)
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
