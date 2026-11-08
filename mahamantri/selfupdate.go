package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gopal-lohar/samrajya/mahamantri/bridge"
)

// selfUpdater carries out approved deploys and restarts of Samrajya with
// scripts/samrajya, in a transient systemd unit of its own: the work
// restarts mahamantri, so it must not run inside mahamantri's service. The
// script writes how it went to a report file, which mahamantri reads back,
// after its restart if need be.
type selfUpdater struct {
	code    string // the codebase
	script  string // scripts/samrajya
	service string // mahamantri's systemd user service
	bridge  string // the bridge's discovery file, for the script
	dir     string // where reports and logs go
	// running is the release mahamantri runs (RELEASE next to it), if known.
	running string
	// systemd says whether mahamantri runs under systemd.
	systemd bool
	run     func(ctx context.Context, name string, args ...string) ([]byte, error)
	start   func(argv []string, env []string, log string) error
}

func newSelfUpdater(code, script, service, bridgeFile, dir string) *selfUpdater {
	u := &selfUpdater{code: code, script: script, service: service, bridge: bridgeFile, dir: dir, systemd: os.Getenv("INVOCATION_ID") != ""}
	if script == "" {
		u.script = filepath.Join(releaseRoot(), "scripts", "samrajya")
	}
	u.running = releaseCommit(releaseRoot())
	u.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
	u.start = u.startDetached
	return u
}

// releaseRoot is the release (or checkout) mahamantri's binary is part of:
// the binary is <root>/mahamantri/mahamantri.
func releaseRoot() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return filepath.Dir(filepath.Dir(exe))
}

// releaseCommit is the commit a release was built from, read from the
// RELEASE file at its root; "" when there is none or it cannot be read, as
// when mahamantri runs from a checkout.
func releaseCommit(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "RELEASE"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (u *selfUpdater) git(ctx context.Context, args ...string) (string, error) {
	out, err := u.run(ctx, "git", append([]string{"-C", u.code}, args...)...)
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// Describe resolves a deploy's ref to a commit and says what deploying it
// changes against the running release.
func (u *selfUpdater) Describe(ctx context.Context, kind, ref string) (string, string, error) {
	if kind != bridge.Deploy {
		return "", "", nil
	}
	if strings.HasPrefix(ref, "-") {
		return "", "", fmt.Errorf("ref %q is not a commit or branch", ref)
	}
	sha, err := u.git(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil || sha == "" {
		return "", "", fmt.Errorf("%q is not a commit or branch of the codebase (%s); commit your change first", ref, u.code)
	}
	if sha == u.running {
		return "", "", fmt.Errorf("%s is the release already running", sha[:10])
	}
	var b strings.Builder
	if u.running != "" {
		if _, err := u.git(ctx, "cat-file", "-e", u.running+"^{commit}"); err == nil {
			log, _ := u.git(ctx, "log", "--no-decorate", "--format=%h %s", "-n", "20", u.running+".."+sha)
			back, _ := u.git(ctx, "rev-list", "--count", sha+".."+u.running)
			stat, _ := u.git(ctx, "diff", "--shortstat", u.running, sha)
			fmt.Fprintf(&b, "Running now: %s. Deploying: %s.\n", u.running[:10], sha[:10])
			if log != "" {
				b.WriteString("Commits it adds:\n" + log + "\n")
			}
			if back != "" && back != "0" {
				fmt.Fprintf(&b, "It leaves out %s commit(s) the running release has.\n", back)
			}
			if stat != "" {
				b.WriteString(stat + "\n")
			}
			if roles, _ := u.git(ctx, "diff", "--name-only", u.running, sha, "--", "senapati", "integrations/claude-channel"); roles != "" {
				b.WriteString("The sessions' role or plugin changes, so the main session is replaced too (workers get it at their next launch).\n")
			}
			b.WriteString("It is built and tested first; if Samrajya does not come back up on it, the running release is put back.")
			return sha, strings.TrimSpace(b.String()), nil
		}
	}
	log, _ := u.git(ctx, "log", "--no-decorate", "--format=%h %s", "-n", "5", sha)
	return sha, "Deploying: " + sha[:10] + ". Latest commits:\n" + log +
		"\nIt is built and tested first; if Samrajya does not come back up on it, the running release is put back.", nil
}

func (u *selfUpdater) report(a bridge.Approval) string { return filepath.Join(u.dir, a.ID+".json") }

// Start hands the approved work to scripts/samrajya, detached.
func (u *selfUpdater) Start(a bridge.Approval) error {
	if err := os.MkdirAll(u.dir, 0o700); err != nil {
		return err
	}
	var args []string
	switch a.Kind {
	case bridge.Deploy:
		args = []string{"deploy", a.Ref}
	case bridge.Restart:
		args = []string{"restart"}
	case bridge.RestartMain:
		args = []string{"restart-main"}
	default:
		return fmt.Errorf("unknown kind %q", a.Kind)
	}
	if _, err := os.Stat(u.script); err != nil {
		return fmt.Errorf("no script at %s", u.script)
	}
	os.Remove(u.report(a))
	env := []string{"SAMRAJYA_REPORT=" + u.report(a), "SAMRAJYA_CODE=" + u.code, "SAMRAJYA_SERVICE=" + u.service, "SAMRAJYA_BRIDGE_FILE=" + u.bridge}
	argv := append([]string{u.script}, args...)
	if u.systemd {
		if run, err := exec.LookPath("systemd-run"); err == nil {
			unit := u.service + "-" + strings.ToLower(a.ID)
			wrapped := []string{run, "--user", "--unit=" + unit, "--collect", "--quiet",
				"--description=Samrajya " + a.ID + ": " + strings.Join(args, " ")}
			for _, e := range append(env, "PATH="+os.Getenv("PATH"), "HOME="+os.Getenv("HOME")) {
				wrapped = append(wrapped, "--setenv="+e)
			}
			argv = append(append(wrapped, "--"), argv...)
		}
	}
	return u.start(argv, env, filepath.Join(u.dir, a.ID+".log"))
}

// startDetached starts argv in a session of its own: systemd-run returns at
// once; without systemd, the script itself runs on after mahamantri.
func (u *selfUpdater) startDetached(argv, env []string, logPath string) error {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// Result reads the script's report.
func (u *selfUpdater) Result(a bridge.Approval) (bool, bool, string) {
	data, err := os.ReadFile(u.report(a))
	if errors.Is(err, os.ErrNotExist) {
		return false, false, ""
	}
	var r struct {
		OK   bool   `json:"ok"`
		Text string `json:"text"`
	}
	if err != nil || json.Unmarshal(data, &r) != nil {
		return true, false, "its report could not be read: " + u.report(a)
	}
	return true, r.OK, r.Text
}
