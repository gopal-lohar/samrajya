package harness

import (
	"strings"
	"testing"
)

const base = "http://127.0.0.1:4097"

func TestToolsSectionsAreCompleteForEachHarness(t *testing.T) {
	common := []string{"## Your tools", "It never means Mahamantri", "sainik-<ISSUE>-<slug>", `'`,
		`"delivery": "queue"`, base + "/instances/<id>", "?parallel=true"}
	cases := map[string][]string{
		"opencode": {"from inside the `execute` tool", "tools.linear.get_issue",
			base + "/opencode/api/session/<id>/interrupt", base + "/opencode/api/session -H"},
		"claude": {"mcp__linear__get_issue", base + "/claude/session/<id>/interrupt",
			base + `/claude/session -H 'Content-Type: application/json' -d '{"title":"sainik-ENG-33-fix-login","text":`,
			base + "/claude/session/<id>/model", base + "/claude/limits", "`limited`", "`manual`"},
	}
	for name, wants := range cases {
		got := For(name, base+"/").Tools()
		for _, want := range append(common, wants...) {
			if !strings.Contains(got, want) {
				t.Errorf("%s tools section missing %q", name, want)
			}
		}
		if strings.Contains(got, "$MAHAMANTRI") {
			t.Errorf("%s: $MAHAMANTRI must be replaced by the real address", name)
		}
	}
	if strings.Contains(For("claude", base).Tools(), "/opencode/") {
		t.Error("claude tools must not mention the opencode API")
	}
}

func TestCommandsPerHarness(t *testing.T) {
	oc, cc := For("opencode", base), For("claude", base)
	if oc.Name() != "opencode" || cc.Name() != "claude" || For("", base).Name() != "opencode" {
		t.Error("names")
	}
	for _, c := range []struct{ got, want string }{
		{oc.Interrupt("s1"), "curl -s -X POST " + base + "/opencode/api/session/s1/interrupt"},
		{cc.Interrupt("s1"), "curl -s -X POST " + base + "/claude/session/s1/interrupt"},
		{cc.Prompt("s1"), base + "/claude/session/s1/prompt"},
		{cc.LastReply("s1"), "curl -s " + base + "/claude/session/s1 | jq -r .lastReply"},
		{oc.LastReply("s1"), base + "/opencode/api/session/s1/message?type=assistant&order=desc&limit=1"},
	} {
		if !strings.Contains(c.got, c.want) {
			t.Errorf("%q does not contain %q", c.got, c.want)
		}
	}
}
