package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// keepMain starts the main session whenever none has been attached for
// grace, by running bridge.keepMain, and at most once every gap: Claude Code
// ends a background session after hours idle, and a ping should not wait
// until someone notices. The command decides what starting means (the
// restart script resumes the last conversation).
type keepMain struct {
	argv   []string
	live   func() bool
	logger *log.Logger
	grace  time.Duration
	gap    time.Duration
	now    func() time.Time
	run    func(ctx context.Context, argv []string) ([]byte, error)

	downSince time.Time
	lastRun   time.Time
}

func newKeepMain(argv []string, live func() bool, logger *log.Logger) *keepMain {
	return &keepMain{argv: argv, live: live, logger: logger, grace: 2 * time.Minute, gap: 5 * time.Minute, now: time.Now, run: runCommand}
}

func (k *keepMain) check(ctx context.Context) {
	if k.live() {
		k.downSince = time.Time{}
		return
	}
	now := k.now()
	if k.downSince.IsZero() {
		k.downSince = now
	}
	if now.Sub(k.downSince) < k.grace || (!k.lastRun.IsZero() && now.Sub(k.lastRun) < k.gap) {
		return
	}
	k.lastRun = now
	k.logger.Printf("keepMain: no main session for %s, running %s", now.Sub(k.downSince).Round(time.Second), strings.Join(k.argv, " "))
	out, err := k.run(ctx, k.argv)
	if err != nil {
		k.logger.Printf("keepMain: %v: %s", err, strings.TrimSpace(string(out)))
	} else if s := strings.TrimSpace(string(out)); s != "" {
		k.logger.Printf("keepMain: %s", s)
	}
}

func (k *keepMain) loop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		k.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// runCommand runs argv with Claude Code's usual install directory on PATH,
// which a systemd user service does not have.
func runCommand(ctx context.Context, argv []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Env = append(os.Environ(), "PATH="+filepath.Join(home, ".local", "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return cmd.CombinedOutput()
}
