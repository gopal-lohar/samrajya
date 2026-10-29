package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/bridge"
	"github.com/gopal-lohar/samrajya/mahamantri/config"
	"github.com/gopal-lohar/samrajya/mahamantri/linear"
)

// runNative serves the bridge for a native Claude Code main session: the
// Linear webhook into the durable inbox, and the local API the Samrajya
// channel plugin feeds the session from. It starts no Claude session, no
// sainik and no TUI - the session is yours, started with the plugin's
// channel enabled - and it stops nothing of it on exit.
func runNative(cfg config.Config, logger *log.Logger) error {
	store, err := bridge.Open(cfg.Native.Store, cfg.Native.TrustedUserIDs)
	if err != nil {
		return fmt.Errorf("native.store %s: %w", cfg.Native.Store, err)
	}
	defer store.Close()
	if n, err := store.ImportThreads(cfg.State.LinearThreadsFile); err != nil {
		return fmt.Errorf("importing %s: %w", cfg.State.LinearThreadsFile, err)
	} else if n > 0 {
		logger.Printf("native: imported %d known Linear threads from %s", n, cfg.State.LinearThreadsFile)
	}

	bridgeLn, err := net.Listen("tcp", cfg.Native.ListenAddr)
	if err != nil {
		return fmt.Errorf("cannot listen on native.listenAddr %s: %w (is another mahamantri running?)", cfg.Native.ListenAddr, err)
	}
	linearLn, err := net.Listen("tcp", cfg.Linear.ListenAddr)
	if err != nil {
		bridgeLn.Close()
		return fmt.Errorf("cannot listen on linear.listenAddr %s: %w (is another mahamantri running?)", cfg.Linear.ListenAddr, err)
	}

	token, err := newToken()
	if err != nil {
		return err
	}
	srv := &bridge.Server{Store: store, Token: token, Logger: logger}
	base := httpURL(bridgeLn.Addr().String())
	if err := writeDiscovery(cfg.Native.DiscoveryFile, base, token); err != nil {
		return fmt.Errorf("native.discoveryFile %s: %w", cfg.Native.DiscoveryFile, err)
	}
	defer os.Remove(cfg.Native.DiscoveryFile)

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

	rec, err := store.Recovery()
	if err != nil {
		return err
	}
	logger.Printf("native: bridge on %s, Linear webhook on %s, store %s", base, linearLn.Addr(), cfg.Native.Store)
	fmt.Fprintf(os.Stderr, "mahamantri (native): bridge %s, Linear webhook %s\n"+
		"  store %s - %d pending pings, %d open tasks, %d unresolved actions\n"+
		"  the main session reads it through the Samrajya channel plugin; Ctrl+C stops mahamantri only\n",
		base, linearLn.Addr(), cfg.Native.Store, len(rec.Pending), len(rec.Open), len(rec.Uncertain))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case err = <-errs:
		logger.Printf("native: server stopped: %v", err)
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

// writeDiscovery tells the plugin where the bridge is and the token for this
// run, readable only by this user.
func writeDiscovery(path, url, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(map[string]any{"url": url, "token": token, "pid": os.Getpid()})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
