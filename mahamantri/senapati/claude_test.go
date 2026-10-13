package senapati

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopal-lohar/samrajya/mahamantri/claudecode"
	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

// The lockdown verified live: only Bash exists, nothing is asked, and only
// curl to mahamantri, jq and Linear are allowed.
func TestClaudeSenapatiArgs(t *testing.T) {
	got := strings.Join(ClaudeSenapatiArgs("http://127.0.0.1:4097/", []string{"mcp__github"}, []string{"mcp__linear__delete_comment"}), "\n")
	want := strings.Join([]string{"--permission-mode", "dontAsk", "--tools", "Bash",
		"--allowedTools", "Bash(curl * http://127.0.0.1:4097/*)", "Bash(curl http://127.0.0.1:4097/*)", "Bash(jq *)", "mcp__linear", "mcp__github",
		"--disallowedTools", "AskUserQuestion", "mcp__linear__delete_comment"}, "\n")
	if got != want {
		t.Errorf("args:\n%s\nwant:\n%s", got, want)
	}
	if s := strings.Join(ClaudeSainikArgs([]string{"--effort", "high"}), " "); s != "--dangerously-skip-permissions --disallowedTools AskUserQuestion --effort high" {
		t.Errorf("sainik args = %s", s)
	}
}

func TestClaudeSenderCarriesRoleArgsAndFallbackIntoTheSession(t *testing.T) {
	dir := t.TempDir()
	h, err := claudecode.New(claudecode.Options{Binary: "/bin/false", StateFile: filepath.Join(dir, "s.json")}, make(chan opencode.Event, 8))
	if err != nil {
		t.Fatal(err)
	}
	c := &ClaudeSender{H: h, Args: []string{"--tools", "Bash"}, FallbackModel: "gpt-6-sol"}
	ctx := context.Background()
	info, err := c.CreateSession(ctx, opencode.CreateSessionRequest{Title: "Senapati-1",
		Location: &opencode.SessionLocation{Directory: dir}, Model: &opencode.SessionModel{ID: "claude-opus-5"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PutInstruction(ctx, info.ID, InstructionKey, "ROLE"); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetSession(ctx, info.ID)
	if err != nil || got.Title != "Senapati-1" {
		t.Errorf("GetSession = %+v, %v", got, err)
	}
	if _, err := c.GetSession(ctx, "nope"); err != opencode.ErrSessionNotFound {
		t.Errorf("missing session: %v", err)
	}
	if i, _ := h.Info(info.ID); i.Model != "claude-opus-5" || i.Dir != dir {
		t.Errorf("info = %+v", i)
	}
	if _, _, err := c.LatestAssistantMessage(ctx, info.ID); err != nil {
		t.Error(err)
	}
	if _, ok, _ := c.LatestAssistantMessage(ctx, info.ID); ok {
		t.Error("no turn yet: no usage to report")
	}
}
