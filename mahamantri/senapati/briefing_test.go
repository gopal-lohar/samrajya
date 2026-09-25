package senapati

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopal-lohar/samrajya/mahamantri/attention"
	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

func TestBriefingTellsSenapatiWhoItIsAndThatTheHandleIsItself(t *testing.T) {
	got := Briefing(BriefingInfo{
		LinearUserID: "bot-id", LinearName: "Senapati", LinearHandle: "senapati-bot",
		OpencodeURL: "http://localhost:4096", AttentionURL: "http://127.0.0.1:4097",
		Instructions: "WORKFLOW TEXT",
	})
	for _, want := range []string{
		"You are Senapati", `the user "Senapati"`, "bot-id",
		"@senapati-bot - that is you, not another person",
		"http://localhost:4096", "http://127.0.0.1:4097", "WORKFLOW TEXT",
		"without waiting to be asked twice",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("briefing is missing %q:\n%s", want, got)
		}
	}
}

func TestBriefingNeverContainsAPassword(t *testing.T) {
	got := Briefing(BriefingInfo{OpencodeURL: "http://localhost:4096"})
	if !strings.Contains(got, "$OPENCODE_SERVER_PASSWORD") {
		t.Error("should point at the env var")
	}
	// There is no password field to leak: the type has no way to carry one.
}

func TestBriefingOmitsIdentityWhenUnknown(t *testing.T) {
	got := Briefing(BriefingInfo{})
	if strings.Contains(got, "Who you are on Linear") || strings.Contains(got, "@") {
		t.Errorf("no identity configured, none should be claimed:\n%s", got)
	}
}

func TestLoadInstructionsStripsAgentFrontmatter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "senapati.md")
	os.WriteFile(path, []byte("---\ndescription: x\nmode: primary\n---\n\nDo the work.\n"), 0o644)
	got, err := LoadInstructions(path)
	if err != nil || got != "Do the work." {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestLoadInstructionsFailsLoudlyForAMissingFile(t *testing.T) {
	if _, err := LoadInstructions(filepath.Join(t.TempDir(), "nope.md")); err == nil {
		t.Error("a configured but unreadable instructions file must be an error, not silently no instructions")
	}
	if got, err := LoadInstructions(""); err != nil || got != "" {
		t.Errorf("empty path = %q, %v, want no instructions and no error", got, err)
	}
}

func TestSummarizeEventSaysWhatToDoAboutIt(t *testing.T) {
	inst := attention.Instance{SessionID: "ses_a", Label: "sainik-SEN-30-photos"}
	cases := []struct {
		ev   opencode.Event
		want string
	}{
		{opencode.Event{Type: "session.execution.succeeded"}, "Review what it produced, report progress on Linear"},
		{opencode.Event{Type: "session.execution.failed"}, "decide whether to retry"},
		{opencode.Event{Type: "permission.asked", Severity: opencode.Blocking, Summary: "permission requested: bash"}, "is blocked: permission requested: bash. Unblock it"},
	}
	for _, c := range cases {
		if got := SummarizeEvent(c.ev, inst); !strings.Contains(got, c.want) || !strings.Contains(got, "sainik-SEN-30-photos") {
			t.Errorf("%s -> %q, want it to contain %q", c.ev.Type, got, c.want)
		}
	}
}
