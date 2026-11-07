package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gopal-lohar/samrajya/mahamantri/bridge"
)

func TestClaudeLauncherStartsABackgroundSessionAsTheAgent(t *testing.T) {
	dir := t.TempDir()
	var got []string
	var ranIn string
	l := &claudeLauncher{command: "claude", dir: dir, discovery: "/state/bridge.json", stateDir: filepath.Join(dir, "agents"),
		mainRole: "/roles/main.md", workerRole: "/roles/worker.md",
		workerArgs: []string{"--model", "sonnet"}, remote: "Samrajya",
		run: func(_ context.Context, argv []string, d string) ([]byte, error) {
			got, ranIn = argv, d
			return []byte("backgrounded · 1a2b3c4d · x\n  claude agents  list sessions\n"), nil
		}}
	id, err := l.Launch(context.Background(), bridge.Agent{ID: "w-T3", Role: bridge.RoleWorker, Name: "samrajya-w-T3-g2", Secret: "s3cret"}, "do T3")
	if err != nil || id != "1a2b3c4d" || ranIn != dir {
		t.Fatalf("launch: %q %v in %s", id, err, ranIn)
	}
	secret := filepath.Join(dir, "agents", "w-T3.secret")
	if b, err := os.ReadFile(secret); err != nil || string(b) != "s3cret" {
		t.Errorf("secret file: %q %v", b, err)
	}
	if st, _ := os.Stat(secret); st.Mode().Perm() != 0o600 {
		t.Errorf("secret file mode %v", st.Mode())
	}
	at := func(flag string) string {
		i := slices.Index(got, flag)
		if i < 0 || i+1 >= len(got) {
			t.Fatalf("no %s in %q", flag, got)
		}
		return got[i+1]
	}
	if got[1] != "--bg" || at("-n") != "samrajya-w-T3-g2" || at("--append-system-prompt-file") != "/roles/worker.md" ||
		slices.Contains(got, "--channels") || at("--model") != "sonnet" || got[len(got)-1] != "do T3" || got[len(got)-2] != "--" {
		t.Errorf("argv %q", got)
	}
	var settings struct{ Env map[string]string }
	json.Unmarshal([]byte(at("--settings")), &settings)
	if settings.Env["SAMRAJYA_AGENT"] != "w-T3" || settings.Env["SAMRAJYA_AGENT_SECRET_FILE"] != secret || settings.Env["SAMRAJYA_BRIDGE_FILE"] != "/state/bridge.json" {
		t.Errorf("settings %v", settings)
	}
	if slices.Contains(got, "Edit") || slices.Contains(got, "--remote-control") {
		t.Errorf("worker argv %q", got)
	}

	l.Launch(context.Background(), bridge.Agent{ID: "main", Role: bridge.RoleMain, Name: "samrajya-main-g1", Secret: "m"}, "hello")
	if !slices.Contains(got, "Edit") || !slices.Contains(got, "Write") || at("--remote-control") != "Samrajya" || at("--append-system-prompt-file") != "/roles/main.md" {
		t.Errorf("main argv %q", got)
	}
}
