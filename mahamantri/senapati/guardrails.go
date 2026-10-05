package senapati

import (
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
