package senapati

import (
	"strings"
	"testing"

	"github.com/gopal-lohar/samrajya/mahamantri/attention"
	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

func TestSummarizeEventUsesLabelWhenPresent(t *testing.T) {
	inst := attention.Instance{SessionID: "ses_a", Label: "sainik-ENG-1-fix-bug"}
	ev := opencode.Event{Summary: "permission requested: write on [file.go]"}
	got := SummarizeEvent(ev, inst)
	if !strings.Contains(got, "sainik-ENG-1-fix-bug") || !strings.Contains(got, "ses_a") {
		t.Errorf("SummarizeEvent() = %q", got)
	}
}

func TestSummarizeEventFallsBackToSessionIDWithoutLabel(t *testing.T) {
	inst := attention.Instance{SessionID: "ses_a"}
	got := SummarizeEvent(opencode.Event{Summary: "done"}, inst)
	if !strings.Contains(got, "ses_a") {
		t.Errorf("SummarizeEvent() = %q", got)
	}
}

func TestSummarizeManualTakeover(t *testing.T) {
	inst := attention.Instance{SessionID: "ses_a", Label: "sainik-ENG-1"}
	got := SummarizeManualTakeover(inst, `sent it a message: "stop"`)
	if !strings.Contains(got, "manually taken over") || !strings.Contains(got, "sainik-ENG-1") || !strings.Contains(got, `sent it a message: "stop"`) {
		t.Errorf("SummarizeManualTakeover() = %q", got)
	}
}

func TestHandoffSummaryListsSainiks(t *testing.T) {
	sainiks := []attention.Instance{
		{SessionID: "ses_a", Label: "sainik-ENG-1", Status: "running", Phase: "awaiting plan approval"},
		{SessionID: "ses_b", Status: "blocked"},
	}
	got := HandoffSummary(sainiks, "ses_old")
	if !strings.Contains(got, "ses_old") || !strings.Contains(got, "sainik-ENG-1") || !strings.Contains(got, "ses_b") || !strings.Contains(got, `phase="awaiting plan approval"`) {
		t.Errorf("HandoffSummary() = %q", got)
	}
}

func TestHandoffSummaryNoSainiks(t *testing.T) {
	got := HandoffSummary(nil, "ses_old")
	if !strings.Contains(got, "No sainiks") {
		t.Errorf("HandoffSummary() = %q, want a no-sainiks note", got)
	}
}

func TestCompletionNoticeSaysQueuedWorkIsDoneAndWhatIsNext(t *testing.T) {
	inst := attention.Instance{SessionID: "ses_a", Label: "sainik-SEN-31-x", Phase: "executing"}
	got := SummarizeEvent(opencode.Event{Type: "session.execution.succeeded"}, inst)
	for _, want := range []string{"[Sainik notice]", `phase "executing"`, "anything you queued for it has been handled", "status"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

type issueReg map[string][]attention.Instance

func (r issueReg) ForIssue(issue string) []attention.Instance { return r[issue] }

func TestIssueSainiksDescribesStateForAPing(t *testing.T) {
	describe := IssueSainiks(issueReg{"SEN-31": {{SessionID: "ses_a", Label: "sainik-SEN-31-fix", Status: "running", Phase: "executing"}}})
	got := describe("SEN-31")
	if !strings.Contains(got, `"sainik-SEN-31-fix" (session ses_a): running`) || !strings.Contains(got, `phase "executing"`) {
		t.Errorf("IssueSainiks = %q", got)
	}
	if describe("SEN-99") != "" {
		t.Error("an issue without a sainik should describe as empty")
	}
}
