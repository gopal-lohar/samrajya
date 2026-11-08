package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gopal-lohar/samrajya/mahamantri/bridge"
)

func gitRepo(t *testing.T) (string, func(args ...string) string) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	commit := func(path, text, msg string) {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755)
		os.WriteFile(filepath.Join(dir, path), []byte(text), 0o644)
		git("add", ".")
		git("commit", "-q", "-m", msg)
	}
	commit("mahamantri/main.go", "v1", "First")
	commit("mahamantri/main.go", "v2", "Second")
	git("checkout", "-q", "-b", "shorter")
	commit("senapati/main.md", "be brief", "Make replies shorter")
	return dir, git
}

func TestDescribeSaysWhatADeployChanges(t *testing.T) {
	dir, git := gitRepo(t)
	u := newSelfUpdater(dir, "/x/samrajya", "svc", "/b.json", t.TempDir())
	u.running = git("rev-parse", "main")
	ctx := context.Background()
	sha, detail, err := u.Describe(ctx, bridge.Deploy, "shorter")
	if err != nil || sha != git("rev-parse", "shorter") {
		t.Fatalf("%q %v", sha, err)
	}
	for _, want := range []string{"Running now: " + u.running[:10], "Make replies shorter", "main session is replaced", "put back"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, detail)
		}
	}
	if _, _, err := u.Describe(ctx, bridge.Deploy, "main"); err == nil {
		t.Error("deployed the running release again")
	}
	for _, bad := range []string{"nope", "--output=/tmp/x", ""} {
		if _, _, err := u.Describe(ctx, bridge.Deploy, bad); err == nil {
			t.Errorf("ref %q accepted", bad)
		}
	}
}

func TestStartRunsTheScriptOutsideTheServiceAndReadsItsReport(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "samrajya")
	os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755)
	u := newSelfUpdater("/code", script, "svc", "/b.json", filepath.Join(dir, "approvals"))
	var argv, env []string
	u.start = func(a, e []string, _ string) error { argv, env = a, e; return nil }
	u.systemd = true
	a := bridge.Approval{ID: "R4", Kind: bridge.Deploy, Ref: "abc"}
	if err := u.Start(a); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("systemd-run"); err == nil {
		if !strings.HasSuffix(argv[0], "systemd-run") || !slices.Contains(argv, "--unit=svc-r4") {
			t.Errorf("argv %v", argv)
		}
	}
	if !slices.Equal(argv[len(argv)-3:], []string{script, "deploy", "abc"}) || !slices.Contains(env, "SAMRAJYA_REPORT="+filepath.Join(dir, "approvals", "R4.json")) ||
		!slices.Contains(env, "SAMRAJYA_BRIDGE_FILE=/b.json") {
		t.Errorf("argv %v env %v", argv, env)
	}
	if done, _, _ := u.Result(a); done {
		t.Error("done before a report")
	}
	os.WriteFile(filepath.Join(dir, "approvals", "R4.json"), []byte(`{"ok":true,"text":"Deployed abc"}`), 0o600)
	if done, ok, text := u.Result(a); !done || !ok || text != "Deployed abc" {
		t.Errorf("%v %v %q", done, ok, text)
	}
	if err := u.Start(bridge.Approval{ID: "R5", Kind: "format_disk"}); err == nil {
		t.Error("an unknown kind started")
	}
}

func TestSessionsMayNotRestartSamrajyaThemselves(t *testing.T) {
	deny := selfDeny("samrajya-mahamantri")
	for _, want := range []string{"Bash(systemctl --user restart samrajya-mahamantri*)", "Bash(*scripts/samrajya deploy*)", "Bash(claude stop*)"} {
		if !slices.Contains(deny, want) {
			t.Errorf("not denied: %s", want)
		}
	}
}

func TestReleaseCommit(t *testing.T) {
	dir := t.TempDir()
	if got := releaseCommit(dir); got != "" {
		t.Fatalf("no RELEASE file: got %q, want empty", got)
	}
	if got := orUnknown(releaseCommit(dir)); got != "unknown (not a release)" {
		t.Fatalf("no RELEASE file in the log: got %q", got)
	}
	sha := "0123456789abcdef0123456789abcdef01234567"
	if err := os.WriteFile(filepath.Join(dir, "RELEASE"), []byte(sha+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := releaseCommit(dir); got != sha {
		t.Fatalf("got %q, want %q", got, sha)
	}
	if err := os.WriteFile(filepath.Join(dir, "RELEASE"), []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := orUnknown(releaseCommit(dir)); got != "unknown (not a release)" {
		t.Fatalf("empty RELEASE file: got %q", got)
	}
	unreadable := filepath.Join(t.TempDir(), "RELEASE")
	if err := os.Mkdir(unreadable, 0o755); err != nil { // a directory cannot be read as a file
		t.Fatal(err)
	}
	if got := releaseCommit(filepath.Dir(unreadable)); got != "" {
		t.Fatalf("unreadable RELEASE: got %q, want empty", got)
	}
}
