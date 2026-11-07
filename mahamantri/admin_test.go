package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopal-lohar/samrajya/mahamantri/bridge"
)

func TestMigrateConfigMovesWhatChanged(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "ws"), 0o755)
	os.WriteFile(filepath.Join(dir, "main.md"), []byte("m"), 0o600)
	os.WriteFile(filepath.Join(dir, "worker.md"), []byte("w"), 0o600)
	old := `# my config
mode: claude
workspace: ws
agents:
  enabled: true
  mainRole: main.md
  workerRole: worker.md
linear:
  signingSecret: s   # keep me
  listenAddr: ":8080"
  botUserID: bot
  botHandle: senapati
bridge:
  trustedUserIDs: [u-owner]
  keepMain: ["../x.sh", "--ensure"]
`
	in, out := filepath.Join(dir, "old.yaml"), filepath.Join(dir, "new.yaml")
	os.WriteFile(in, []byte(old), 0o600)
	if err := migrateConfig(in, out); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	for _, want := range []string{"# my config", "# keep me", "trustedUserIDs: [u-owner]"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, gone := range []string{"keepMain", "enabled"} {
		if strings.Contains(string(got), gone) {
			t.Errorf("%s kept:\n%s", gone, got)
		}
	}
}

func TestDowngradeStoreKeepsEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	s, err := bridge.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Ingest(bridge.Inbound{ID: "linear:c1", Conversation: "linear:i", Trusted: true, Body: "x"})
	s.Close()
	if err := downgradeStore(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".schema2.bak"); err != nil {
		t.Error("no backup")
	}
	// Opening it again upgrades it, as after a second cutover.
	s, err = bridge.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if p, _ := s.Pending(0); len(p) != 1 {
		t.Errorf("pending %v", p)
	}
}

func TestClaudesInstallDirectoryIsOnPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	os.MkdirAll(filepath.Join(home, ".local", "bin"), 0o755)
	os.WriteFile(filepath.Join(home, ".local", "bin", "claude"), []byte("#!/bin/sh\n"), 0o755)
	withClaudeOnPath()
	withClaudeOnPath()
	if p, err := exec.LookPath("claude"); err != nil || p != filepath.Join(home, ".local", "bin", "claude") {
		t.Errorf("claude: %q %v", p, err)
	}
	if strings.Count(os.Getenv("PATH"), ".local/bin") != 1 {
		t.Errorf("PATH %q", os.Getenv("PATH"))
	}
}
