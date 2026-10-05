package senapati

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBriefingTellsSenapatiWhoItIsAndThatTheHandleIsItself(t *testing.T) {
	got := Briefing(BriefingInfo{
		LinearUserID: "bot-id", LinearName: "Senapati", LinearHandle: "senapati-bot",
		MahamantriURL: "http://127.0.0.1:4097",
		DefaultModel:  "opencode-go/deepseek-v4.1-flash",
		Models: []ModelChoice{
			{ID: "opencode-go/deepseek-v4.1-flash", Credits: "plentiful", Use: "routine work"},
			{ID: "openai/gpt-6-sol", Credits: "scarce", Use: "hard reasoning"},
		},
		Instructions: "WORKFLOW TEXT: curl -s $MAHAMANTRI/opencode/api/session",
	})
	for _, want := range []string{
		"You are Senapati", `the user "Senapati"`, "bot-id",
		"@senapati-bot - that is you, not another person",
		"http://127.0.0.1:4097", "http://127.0.0.1:4097/opencode/openapi.json", "you never need, test or ask for an opencode password",
		"Default when you name none: opencode-go/deepseek-v4.1-flash",
		"openai/gpt-6-sol [credits: scarce] - hard reasoning", "WORKFLOW TEXT",
		"curl -s http://127.0.0.1:4097/opencode/api/session",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("briefing is missing %q:\n%s", want, got)
		}
	}
}

// Senapati used to be told to authenticate to opencode itself and then
// stalled with "credentials unavailable". It must not be asked to.
func TestBriefingNeverAsksSenapatiToHandleOpencodeCredentials(t *testing.T) {
	got := Briefing(BriefingInfo{MahamantriURL: "http://127.0.0.1:4097"})
	if strings.Contains(got, "OPENCODE_SERVER_PASSWORD") || strings.Contains(got, "basic auth") {
		t.Errorf("briefing mentions opencode credentials:\n%s", got)
	}
}

// The reported failure: Senapati read every Linear ping as a person asking it
// to do the task, and did it in its own context instead of a sainik's.
func TestBriefingMakesSenapatiAManagerThatNeverWaits(t *testing.T) {
	got := Briefing(BriefingInfo{MahamantriURL: "http://127.0.0.1:4097"})
	for _, want := range []string{
		"You are Senapati, the manager", "you never do the work yourself", "one separate opencode session per issue",
		"The only shell commands that run are `curl` to Mahamantri's address and `jq`",
		"never a sign that Mahamantri or Linear is down", "report the exact command and error",
		"from inside the `execute` tool", "tools.linear.get_issue",
		"never from a person typing to you", "[Linear ping]", "[Sainik notice]", "You never wait for anything",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("briefing is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "the worker") {
		t.Errorf("briefing must not call Senapati the worker:\n%s", got)
	}
}

// The instructions file shipped with the repo must say the same things.
func TestShippedInstructionsCoverPingsAndDelivery(t *testing.T) {
	text, err := LoadInstructions("../../senapati/senapati.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Handling a Linear ping", "## Handling a sainik notice", "It never means Mahamantri",
		"tools.linear.get_issue", "from inside the `execute` tool",
		`"delivery": "queue"`, "$MAHAMANTRI/opencode/api/session/<id>/interrupt", "$MAHAMANTRI/opencode/api/session -H",
		"sainik-<ISSUE>-<slug>", `\u0027`,
		"Never wait",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("senapati.md is missing %q", want)
		}
	}
	for _, banned := range []string{"OPENCODE_SERVER_PASSWORD", "sleep ", "$MAHAMANTRI/sainiks", "/status"} {
		if strings.Contains(text, banned) {
			t.Errorf("senapati.md must not mention %q", banned)
		}
	}
}

func TestBriefingOmitsTheModelSectionWhenNoneConfigured(t *testing.T) {
	if got := Briefing(BriefingInfo{MahamantriURL: "http://x"}); strings.Contains(got, "Models -") {
		t.Errorf("no models configured, none should be listed:\n%s", got)
	}
}

func TestBriefingOmitsIdentityWhenUnknown(t *testing.T) {
	got := Briefing(BriefingInfo{})
	if strings.Contains(got, "Who you are on Linear") || strings.Contains(got, " as @") {
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
