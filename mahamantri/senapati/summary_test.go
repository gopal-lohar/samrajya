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
	got := SummarizeEvent(ev, inst, "", base)
	if !strings.Contains(got, "sainik-ENG-1-fix-bug") || !strings.Contains(got, "ses_a") {
		t.Errorf("SummarizeEvent() = %q", got)
	}
}

func TestSummarizeEventFallsBackToSessionIDWithoutLabel(t *testing.T) {
	inst := attention.Instance{SessionID: "ses_a"}
	got := SummarizeEvent(opencode.Event{Summary: "done"}, inst, "", base)
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

const base = "http://127.0.0.1:4097"

func TestCompletionNoticeCarriesTheReportAndTheNextStep(t *testing.T) {
	inst := attention.Instance{SessionID: "ses_a", Label: "sainik-SEN-31-x", Phase: "planning"}
	got := SummarizeEvent(opencode.Event{Type: "session.execution.succeeded", SessionID: "ses_a"}, inst, "Plan:\n1. fix it", base)
	for _, want := range []string{"[Sainik notice]", `phase "planning"`, "finished its turn and is idle", "> Plan:\n> 1. fix it", "post the result on Linear"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	missing := SummarizeEvent(opencode.Event{Type: "session.execution.succeeded"}, inst, "", base)
	if !strings.Contains(missing, base+"/opencode/api/session/ses_a/message?type=assistant&order=desc&limit=1") {
		t.Errorf("without the reply, the notice must say how to fetch it:\n%s", missing)
	}
	long := SummarizeEvent(opencode.Event{Type: "session.execution.succeeded"}, inst, strings.Repeat("x", 5000), base)
	if !strings.Contains(long, "cut at 4000 characters") || len(long) > 5000 {
		t.Errorf("a long reply must be cut and say so (len %d)", len(long))
	}
}

// The reply goes to the session that asked - often a sainik's subagent - and
// the notice must carry the request id, or Senapati cannot answer it.
func TestPermissionNoticeSaysExactlyHowToAnswer(t *testing.T) {
	inst := attention.Instance{SessionID: "ses_a", Label: "sainik-SEN-31-x"}
	ev := opencode.Event{Type: "permission.asked", Severity: opencode.Blocking, SessionID: "ses_child",
		Raw: []byte(`{"id":"per_9","sessionID":"ses_child","action":"external_directory","resources":["/home/ubuntu/*"]}`)}
	got := SummarizeEvent(ev, inst, "", base)
	for _, want := range []string{"external_directory [/home/ubuntu/*]", "request per_9",
		"curl -s -X POST " + base + "/opencode/api/session/ses_child/permission/per_9/reply", `{"decision":"once"}`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// Shape captured live: the form is wrapped, and the notice must carry the
// questions and options so Senapati can answer without another call.
func TestQuestionNoticeSaysExactlyHowToAnswer(t *testing.T) {
	inst := attention.Instance{SessionID: "ses_a", Label: "sainik-SEN-31-x"}
	ev := opencode.Event{Type: "form.created", Severity: opencode.Blocking, SessionID: "ses_a", Raw: []byte(`{"form":{"id":"frm_2","sessionID":"ses_a","title":"Questions",
		"fields":[{"key":"q0","title":"Color preference","description":"Do you prefer red or blue?","type":"string","options":[{"value":"Red"},{"value":"Blue"}]}]}}`)}
	got := SummarizeEvent(ev, inst, "", base)
	for _, want := range []string{"form frm_2, session ses_a", `q0: Color preference - Do you prefer red or blue? (options: "Red", "Blue")`,
		"curl -s -X POST " + base + `/opencode/api/session/ses_a/form/frm_2/reply -H 'Content-Type: application/json' -d '{"answer":{"q0":"<answer>"}}'`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

type issueReg map[string][]attention.Instance

func (r issueReg) ForIssue(issue string) []attention.Instance { return r[issue] }

func TestIssueSainiksListsTheIssuesSainiks(t *testing.T) {
	list := IssueSainiks(issueReg{"SEN-31": {{SessionID: "ses_a", Label: "sainik-SEN-31-fix", Status: "running", Phase: "executing"}}})
	got := list("SEN-31")
	if len(got) != 1 || got[0].SessionID != "ses_a" || got[0].Status != "running" || got[0].Phase != "executing" || got[0].Label != "sainik-SEN-31-fix" {
		t.Errorf("IssueSainiks = %+v", got)
	}
	if len(list("SEN-99")) != 0 {
		t.Error("an issue without a sainik should list none")
	}
}
