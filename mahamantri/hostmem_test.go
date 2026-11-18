package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopal-lohar/samrajya/mahamantri/bridge"
)

func TestParseMemAvailable(t *testing.T) {
	in := "MemTotal:       12218368 kB\nMemFree:          292388 kB\nMemAvailable:    1311936 kB\n"
	got, ok := parseMemAvailable(strings.NewReader(in))
	if !ok || got != 1311936<<10 {
		t.Errorf("got %d %v", got, ok)
	}
	if _, ok := parseMemAvailable(strings.NewReader("MemFree: 1 kB\n")); ok {
		t.Error("found MemAvailable where there is none")
	}
}

func TestWhyGoneReadsTheDaemonsLastRetireLine(t *testing.T) {
	log := filepath.Join(t.TempDir(), "daemon.log")
	lines := "[2026-11-04T07:32:11Z] [bg] bg retire aaaa1111: idle-prompt, idle 8h\n" +
		"[2026-11-04T12:45:11Z] [bg] bg retire bbbb2222: idle-prompt, idle 18m [low memory]\n" +
		"[2026-11-04T12:45:14Z] [bg] bg settled bbbb2222 (done)\n" +
		"[2026-11-04T13:29:13Z] [bg] bg retire aaaa1111: settled, idle 2m [low memory]\n"
	if err := os.WriteFile(log, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &claudeLauncher{daemonLog: log}
	if got := l.WhyGone(bridge.Session{ID: "aaaa1111"}); got != "Claude Code retired it: settled, idle 2m [low memory]" {
		t.Errorf("aaaa1111: %q", got)
	}
	if got := l.WhyGone(bridge.Session{ID: "bbbb2222-0000-0000-0000-000000000000"}); !strings.HasSuffix(got, "idle 18m [low memory]") {
		t.Errorf("bbbb2222: %q", got)
	}
	if got := l.WhyGone(bridge.Session{ID: "cccc3333"}); got != "" {
		t.Errorf("unknown session: %q", got)
	}
	if got := (&claudeLauncher{daemonLog: filepath.Join(t.TempDir(), "none")}).WhyGone(bridge.Session{ID: "aaaa1111"}); got != "" {
		t.Errorf("no log: %q", got)
	}
}
