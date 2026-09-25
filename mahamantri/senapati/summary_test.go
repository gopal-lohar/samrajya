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
		{SessionID: "ses_a", Label: "sainik-ENG-1", Status: "running"},
		{SessionID: "ses_b", Status: "blocked"},
	}
	got := HandoffSummary(sainiks, "ses_old")
	if !strings.Contains(got, "ses_old") || !strings.Contains(got, "sainik-ENG-1") || !strings.Contains(got, "ses_b") {
		t.Errorf("HandoffSummary() = %q", got)
	}
}

func TestHandoffSummaryNoSainiks(t *testing.T) {
	got := HandoffSummary(nil, "ses_old")
	if !strings.Contains(got, "No sainiks") {
		t.Errorf("HandoffSummary() = %q, want a no-sainiks note", got)
	}
}
