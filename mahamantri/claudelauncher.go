package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/bridge"
)

// claudeLauncher runs agents as Claude Code background sessions
// (claude --bg) in the main directory, where the Samrajya plugin is
// installed. Each session learns its agent id, the path of its secret file
// and the bridge's discovery file through --settings env, which reaches the
// plugin's process and its hooks, and its role through
// --append-system-prompt-file, which outranks Claude Code's defaults.
type claudeLauncher struct {
	command    string
	dir        string // where sessions run
	discovery  string
	stateDir   string
	channel    string
	mainRole   string
	workerRole string
	mainArgs   []string
	workerArgs []string
	remote     string // the main's Remote Control name; "" for none
	workerRC   bool
	store      *bridge.Store
	run        func(ctx context.Context, argv []string, dir string) ([]byte, error)
}

// Tools withheld from every agent: comments go through samrajya_reply,
// which posts as Senapati whoever the connector belongs to.
var withheldAll = []string{"mcp__claude_ai_Linear__save_comment", "mcp__claude_ai_Linear__delete_comment"}

// Tools withheld from the main, so that long work can only be dispatched.
var withheldMain = []string{"Edit", "Write", "NotebookEdit"}

var backgroundedRE = regexp.MustCompile(`backgrounded · ([0-9a-f]+)`)

func (l *claudeLauncher) Launch(ctx context.Context, a bridge.Agent, brief string) (string, error) {
	if err := os.MkdirAll(l.stateDir, 0o700); err != nil {
		return "", err
	}
	secretFile := filepath.Join(l.stateDir, a.ID+".secret")
	tmp := secretFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(a.Secret), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, secretFile); err != nil {
		return "", err
	}
	settings, _ := json.Marshal(map[string]any{"env": map[string]string{
		"SAMRAJYA_AGENT":             a.ID,
		"SAMRAJYA_AGENT_SECRET_FILE": secretFile,
		"SAMRAJYA_BRIDGE_FILE":       l.discovery,
	}})
	role, extra, withheld := l.workerRole, l.workerArgs, withheldAll
	if a.Role == bridge.RoleMain {
		role, extra, withheld = l.mainRole, l.mainArgs, append(append([]string(nil), withheldAll...), withheldMain...)
	}
	argv := []string{l.command, "--bg", "-n", a.Name, "--settings", string(settings),
		"--append-system-prompt-file", role, "--dangerously-skip-permissions",
		"--channels", l.channel, "--disallowedTools"}
	argv = append(argv, withheld...)
	if rc := l.remoteName(a); rc != "" {
		argv = append(argv, "--remote-control", rc)
	}
	argv = append(argv, extra...)
	argv = append(argv, "--", brief)
	out, err := l.run(ctx, argv, l.dir)
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	m := backgroundedRE.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("claude did not say it backgrounded a session: %s", strings.TrimSpace(string(out)))
	}
	return string(m[1]), nil
}

// remoteName is the Remote Control session name: the configured one for the
// main, the task for a worker.
func (l *claudeLauncher) remoteName(a bridge.Agent) string {
	if a.Role == bridge.RoleMain {
		return l.remote
	}
	if !l.workerRC || l.store == nil {
		return ""
	}
	t, err := l.store.Task(a.TaskID)
	if err != nil {
		return a.ID
	}
	name := t.Title
	if len(name) > 60 {
		name = name[:60]
	}
	return name
}

// Stop stops a session and waits for its process to end, killing it if it
// does not.
func (l *claudeLauncher) Stop(ctx context.Context, s bridge.Session) error {
	if s.ID != "" {
		if out, err := l.run(ctx, []string{l.command, "stop", s.ID}, l.dir); err != nil {
			// Already gone is fine; anything else is checked below.
			_ = out
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		pid := l.pidOf(ctx, s)
		if pid == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			time.Sleep(time.Second)
			if l.pidOf(ctx, s) != 0 {
				return fmt.Errorf("session %s (pid %d) would not stop", s.Name, pid)
			}
			return nil
		}
		time.Sleep(time.Second)
	}
}

func (l *claudeLauncher) pidOf(ctx context.Context, s bridge.Session) int {
	list, err := l.list(ctx)
	if err != nil {
		return 0
	}
	for _, e := range list {
		if (s.ID != "" && e.ID == s.ID) || (s.Name != "" && e.Name == s.Name) {
			return e.PID
		}
	}
	return 0
}

type claudeSession struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	PID  int    `json:"pid"`
	Cwd  string `json:"cwd"`
}

func (l *claudeLauncher) list(ctx context.Context) ([]claudeSession, error) {
	out, err := l.run(ctx, []string{l.command, "agents", "--json"}, l.dir)
	if err != nil {
		return nil, fmt.Errorf("claude agents: %v: %s", err, strings.TrimSpace(string(out)))
	}
	var all []claudeSession
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, fmt.Errorf("claude agents: %v", err)
	}
	var live []claudeSession
	for _, s := range all {
		if s.PID > 0 && syscall.Kill(s.PID, 0) == nil {
			live = append(live, s)
		}
	}
	return live, nil
}

// Running lists live sessions in the main directory.
func (l *claudeLauncher) Running(ctx context.Context) ([]bridge.Session, error) {
	list, err := l.list(ctx)
	if err != nil {
		return nil, err
	}
	dir, _ := filepath.EvalSymlinks(l.dir)
	var out []bridge.Session
	for _, s := range list {
		cwd, _ := filepath.EvalSymlinks(s.Cwd)
		if cwd == dir {
			out = append(out, bridge.Session{ID: s.ID, Name: s.Name})
		}
	}
	return out, nil
}

// runIn runs argv in dir with Claude Code's install directory on PATH.
func runIn(ctx context.Context, argv []string, dir string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Env = append(os.Environ(), "PATH="+filepath.Join(home, ".local", "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return cmd.CombinedOutput()
}
