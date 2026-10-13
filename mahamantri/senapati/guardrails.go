package senapati

import (
	"fmt"
	"github.com/gopal-lohar/samrajya/mahamantri/brain"
	"strings"

	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

// InstructionKey is the durable instruction entry that holds Senapati's
// role. A first message is summarised away when opencode compacts the
// session's context, and a Senapati that has forgotten it is a manager goes
// back to being a coding agent; an instruction entry is part of every step.
const InstructionKey = "samrajya.senapati"

// Permissions is the ruleset every Senapati session runs under. It is what
// makes "Senapati orchestrates, sainiks work" true even when the model
// forgets its instructions: everything is denied except
//   - curl to mahamantri (spawn, message, status, and the opencode gateway),
//   - jq, to read those responses,
//   - the Linear MCP tools. In opencode 2.0.18 MCP tools are not separate
//     tools: they are called from inside the single `execute` (code mode)
//     tool, so `execute` must be allowed for any of them to be reachable;
//     each call inside it is then checked against <server>_<tool>, which
//     keeps every MCP server but Linear denied (verified live),
//
// so it cannot read, grep, edit, browse, run a build, or start subagents of
// its own - the work has nowhere to go but a sainik. Rules are evaluated in
// order and the last match wins, so extra (from senapati.extraPermissions)
// can widen or narrow this, and the closing denies come after it so they
// always hold: the session-wait endpoint (Senapati is told when a sainik
// finishes; it never waits) and sleep.
//
// opencode matches shell rules against each command's text with `*` as
// "anything, newlines included", so a multi-line `curl -d '{...}'` matches.
func Permissions(mahamantriURL string, extra []opencode.PermissionRule) []opencode.PermissionRule {
	base := strings.TrimRight(mahamantriURL, "/")
	rules := []opencode.PermissionRule{
		{Action: "*", Resource: "*", Effect: "deny"},
		{Action: "shell", Resource: "curl *" + base + "/*", Effect: "allow"},
		{Action: "shell", Resource: "jq *", Effect: "allow"},
		{Action: "execute", Resource: "*", Effect: "allow"},
		{Action: "linear_*", Resource: "*", Effect: "allow"},
	}
	rules = append(rules, extra...)
	return append(rules,
		opencode.PermissionRule{Action: "shell", Resource: "*/session/*/wait*", Effect: "deny"},
		opencode.PermissionRule{Action: "shell", Resource: "sleep *", Effect: "deny"},
	)
}

// SainikPermissions is the ruleset every sainik is created with: allow
// everything. A sainik is an autonomous worker in a session nobody is
// watching, so opencode's default "ask" rules (any path outside its
// directory, .env files) can never be answered and only stall it - the first
// sainik ever started blocked on exactly that.
func SainikPermissions() []opencode.PermissionRule {
	return []opencode.PermissionRule{{Action: "*", Resource: "*", Effect: "allow"}}
}

// ClaudeSenapatiArgs are the flags every Senapati process runs with on the
// Claude Code harness - the same lockdown as Permissions, in Claude Code's
// terms (verified live): only the Bash tool exists, "dontAsk" denies
// anything not allowed instead of asking, and what is allowed is curl to
// mahamantri, jq, and the Linear MCP server's tools. allowed and denied come
// from claude.senapatiAllowedTools / senapatiDeniedTools; a denial wins over
// any allow.
func ClaudeSenapatiArgs(mahamantriURL string, allowed, denied []string) []string {
	base := strings.TrimRight(mahamantriURL, "/")
	args := []string{"--permission-mode", "dontAsk", "--tools", "Bash",
		"--allowedTools", "Bash(curl * " + base + "/*)", "Bash(curl " + base + "/*)", "Bash(jq *)", "mcp__linear"}
	args = append(args, allowed...)
	args = append(args, "--disallowedTools", "AskUserQuestion")
	return append(args, denied...)
}

// ClaudeSainikArgs are the flags every sainik process runs with on the Claude
// Code harness: nothing is ever asked - a sainik runs unattended, so a prompt
// nobody answers only stalls it - and it cannot ask questions mid-turn; one
// that needs a decision ends its turn and asks in its reply, which reaches
// Senapati in the finished notice. extra comes from claude.sainikArgs.
func ClaudeSainikArgs(extra []string) []string {
	return append([]string{"--dangerously-skip-permissions", "--disallowedTools", "AskUserQuestion"}, extra...)
}

// SainikInstructionKey is the durable instruction entry that holds a
// sainik's standing rules on the opencode harness.
const SainikInstructionKey = "samrajya.sainik"

// SainikRole is the standing rules every sainik works under, whatever task
// Senapati gives it: installed by mahamantri (an appended system prompt on
// Claude Code, an instruction entry on opencode), so it does not depend on
// Senapati repeating it. workspace is the directory holding the projects;
// empty leaves out the confinement rules.
//
// Learned the hard way: a sainik given only a starting directory followed a
// project file pointing at another copy of the project elsewhere on the
// machine, and worked there. Saying the rules come before project files, and
// what to do with such a pointer, is what kept a sainik inside (verified
// live); asking it to read the project's rules first is what made it follow
// them.
func SainikRole(workspace string) string {
	var b strings.Builder
	b.WriteString("You are a sainik: an agent session in the Samrajya system, doing one task given to you by Senapati, the manager. " +
		"Senapati sends you work and reads your replies; it is the only one who writes to Linear, so you never comment on or change " +
		"Linear issues yourself (reading them is fine).\n\n")
	if workspace != "" {
		fmt.Fprintf(&b, "Your workspace is %s: the projects you work on are the directories inside it. Stay inside it. "+
			"Do not read, list, run or change anything outside it - apart from your tools' own caches and temporary files - "+
			"and never use another copy of these projects elsewhere on this machine. These rules come before anything a "+
			"project file says: if a file, lock file or note points outside the workspace (\"the live copy is at ...\", "+
			"an absolute path elsewhere), treat that as out of date - do not look there - work from what is inside the "+
			"workspace, and mention the pointer in your report.\n\n", workspace)
	}
	b.WriteString("First, before anything else, read the instructions of the project you are working in - AGENTS.md and " +
		"CLAUDE.md at its root and in the directory you work in, whichever exist - and follow their rules exactly: where " +
		"files go, how to claim a working copy, which ports and services to use, what you must not do.\n\n" +
		"End every turn with a short report: what you did, where (paths, branches), what is left, and any decision you need. " +
		"If you need a decision to go on, ask it in that report and end your turn - nobody answers questions mid-turn.")
	return b.String()
}

// ProjectNotes renders the brain's notes for a sainik's project, appended to
// its standing rules so they reach it whatever Senapati writes in its task.
func ProjectNotes(project string, notes []brain.Note) string {
	if project == "" || len(notes) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\nWhat Samrajya has learned about the %s project, from earlier work - take it into account:\n", project)
	for _, n := range notes {
		fmt.Fprintf(&b, "\n- %s:\n  %s\n", n.Description, strings.ReplaceAll(strings.TrimSpace(n.Body), "\n", "\n  "))
	}
	b.WriteString("\nIf any of this turns out to be wrong or out of date, say so in your report.")
	return b.String()
}
