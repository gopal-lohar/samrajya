package senapati

import (
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
  "issue": "SEN-31", "slug": "fix", "task": "..."
}'`},
		{"shell", `curl -s "http://127.0.0.1:4097/sainiks/ses_1/status"`},
		{"shell", "jq .state"},
		{"linear_get_issue", "*"},
		{"linear_create_comment", "*"},
	}
	for _, c := range allowed {
		if got := decide(rules, c.action, c.resource); got != "allow" {
			t.Errorf("%s %q = %s, want allow", c.action, c.resource, got)
		}
	}
	denied := []struct{ action, resource string }{
		{"read", "/home/ubuntu/samrajya/repos/autowrite/main.go"},
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
