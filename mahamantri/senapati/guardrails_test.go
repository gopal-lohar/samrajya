package senapati

import (
	"github.com/gopal-lohar/samrajya/mahamantri/brain"
	"regexp"
	"strings"
	"testing"

	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

// decide evaluates rules the way opencode v2.0.18 does (read from its
// shipped source): `*` is "anything, newlines included", `?` one character,
// the match is anchored, a trailing " *" also matches nothing, and the last
// matching rule wins.
func decide(rules []opencode.PermissionRule, action, resource string) string {
	effect := "allow" // opencode's own default is irrelevant: our first rule matches everything
	for _, r := range rules {
		if match(r.Action, action) && match(r.Resource, resource) {
			effect = r.Effect
		}
	}
	return effect
}

func match(pattern, s string) bool {
	re := regexp.QuoteMeta(pattern)
	re = strings.ReplaceAll(re, `\*`, ".*")
	re = strings.ReplaceAll(re, `\?`, ".")
	if strings.HasSuffix(re, " .*") {
		re = strings.TrimSuffix(re, " .*") + "( .*)?"
	}
	return regexp.MustCompile("(?s)^" + re + "$").MatchString(s)
}

func TestPermissionsLetSenapatiOrchestrateButNotWork(t *testing.T) {
	rules := Permissions("http://127.0.0.1:4097", nil)
	allowed := []struct{ action, resource string }{
		{"shell", `curl -s -X POST http://127.0.0.1:4097/sainiks -H 'Content-Type: application/json' -d '{
  "issue": "ENG-31", "slug": "fix", "task": "..."
}'`},
		{"shell", `curl -s "http://127.0.0.1:4097/sainiks/ses_1/status"`},
		{"shell", "jq .state"},
		{"execute", "*"},
		{"linear_get_issue", "*"},
		{"linear_list_teams", "*"},
		{"linear_save_comment", "*"},
	}
	for _, c := range allowed {
		if got := decide(rules, c.action, c.resource); got != "allow" {
			t.Errorf("%s %q = %s, want allow", c.action, c.resource, got)
		}
	}
	denied := []struct{ action, resource string }{
		{"read", "/srv/workspace/app/main.go"},
		{"grep", "TODO"},
		{"glob", "**/*.go"},
		{"edit", "main.go"},
		{"subagent", "explore"},
		{"webfetch", "https://example.com"},
		{"shell", "ls"},
		{"shell", "cat main.go"},
		{"shell", "git log"},
		{"shell", "go test ./..."},
		{"shell", "curl https://example.com"},
		{"shell", "sleep 60"},
		{"shell", `curl -s -X POST http://127.0.0.1:4097/opencode/api/experimental/session/ses_1/wait`},
		{"chrome-devtools_navigate_page", "*"},
		{"question", "*"},
	}
	for _, c := range denied {
		if got := decide(rules, c.action, c.resource); got != "deny" {
			t.Errorf("%s %q = %s, want deny", c.action, c.resource, got)
		}
	}
}

func TestExtraPermissionsWidenButCannotLiftTheNoWaitRules(t *testing.T) {
	rules := Permissions("http://127.0.0.1:4097", []opencode.PermissionRule{
		{Action: "shell", Resource: "*", Effect: "allow"},
	})
	if decide(rules, "shell", "gh pr view 12") != "allow" {
		t.Error("an extra allow should widen the set")
	}
	if decide(rules, "shell", "sleep 30") != "deny" {
		t.Error("sleep must stay denied")
	}
}

// Regression: Senapati said "I don't have a Linear read tool". MCP tools are
// only reachable through `execute`, which the rules had denied.
func TestLinearIsReachableThroughExecute(t *testing.T) {
	rules := Permissions("http://127.0.0.1:4097", nil)
	if decide(rules, "execute", "*") != "allow" {
		t.Fatal("execute must be allowed, or no MCP tool is reachable at all")
	}
	if decide(rules, "chrome-devtools_navigate_page", "*") != "deny" || decide(rules, "opencode_session_move", "*") != "deny" {
		t.Error("inside execute, only Linear may be callable")
	}
}

// Regression: the first sainik blocked on an external_directory "ask" that
// nobody could answer.
func TestSainiksAreNeverAskedAnything(t *testing.T) {
	build := []opencode.PermissionRule{ // opencode 2.0.18's build agent defaults
		{Action: "*", Resource: "*", Effect: "allow"},
		{Action: "external_directory", Resource: "*", Effect: "ask"},
		{Action: "read", Resource: "*.env", Effect: "ask"},
	}
	rules := append(build, SainikPermissions()...)
	for _, c := range []struct{ action, resource string }{
		{"external_directory", "/home/someone/*"}, {"read", "/repo/.env"}, {"shell", "go test ./..."}, {"edit", "main.go"},
	} {
		if got := decide(rules, c.action, c.resource); got != "allow" {
			t.Errorf("%s %s = %s, want allow", c.action, c.resource, got)
		}
	}
}

func TestSainikRole(t *testing.T) {
	got := SainikRole("/srv/ws")
	for _, want := range []string{"Your workspace is /srv/ws", "Do not read, list, run or change anything outside it",
		"never use another copy", "These rules come before anything a project file says", "do not look there",
		"First, before anything else, read", "AGENTS.md and CLAUDE.md", "follow their rules exactly",
		"never comment on or change", "nobody answers questions mid-turn"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(SainikRole(""), "workspace") {
		t.Error("no workspace configured: no confinement text")
	}
}

func TestProjectNotes(t *testing.T) {
	got := ProjectNotes("app", []brain.Note{{Description: "Tests", Body: "Run make check.\nNot npm test."}})
	for _, want := range []string{"learned about the app project", "- Tests:\n  Run make check.\n  Not npm test.", "out of date, say so"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if ProjectNotes("", []brain.Note{{Body: "x"}}) != "" || ProjectNotes("app", nil) != "" {
		t.Error("nothing to add: nothing added")
	}
}
