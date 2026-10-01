package senapati

import (
	"fmt"
	"strings"

	"github.com/gopal-lohar/samrajya/mahamantri/attention"
	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

func label(inst attention.Instance) string {
	if inst.Label != "" {
		return inst.Label
	}
	return inst.SessionID
}

// SummarizeEvent formats an attention-required event on a registered sainik
// for Senapati. Each one says what it means and what is expected - including
// that nothing is to be waited on: the next notice comes by itself.
func SummarizeEvent(ev opencode.Event, inst attention.Instance) string {
	who := fmt.Sprintf("[Sainik notice] %q (session %s)", label(inst), inst.SessionID)
	if inst.Phase != "" {
		who += fmt.Sprintf(", phase %q,", inst.Phase)
	}
	switch {
	case ev.Type == "session.execution.succeeded" || ev.Type == "session.idle":
		return who + " finished - it is idle now, and anything you queued for it has been handled too. " +
			"Read its status (lastText) for what it produced; ask it for a short report only if that is not enough. " +
			"Then do the next step of the issue's workflow: post the result on Linear, give it its next task, or close it out."
	case ev.Type == "session.execution.failed":
		return who + " failed. Read its status to see why, then retry it with a sharper instruction, " +
			"switch model if it ran out of credits, or report the problem on Linear."
	case ev.Type == "session.execution.interrupted":
		return who + " was interrupted and is idle. If you interrupted it, the message you sent with the interrupt is what it works on next; nothing else to do."
	case ev.Severity == opencode.Blocking:
		return fmt.Sprintf("%s is blocked: %s. Decide it if it is within the issue's scope; otherwise ask on Linear.", who, ev.Summary)
	}
	return fmt.Sprintf("%s needs attention: %s", who, ev.Summary)
}

// SummarizeManualTakeover formats the "a human took this session over"
// notice sent to Senapati, saying what the person did (the action phrase
// from attention.DetectManualTakeover).
func SummarizeManualTakeover(inst attention.Instance, action string) string {
	return fmt.Sprintf("[Sainik notice] %q (session %s) was manually taken over: a person %s. "+
		"Leave it alone until they are done - do not message or interrupt it - and note it on the issue if it matters.",
		label(inst), inst.SessionID, action)
}

// IssueSainiks describes, for a Linear ping about issue, the sainiks working
// on it as mahamantri last saw them - one line each, "" for none.
func IssueSainiks(reg interface {
	ForIssue(issue string) []attention.Instance
}) func(issue string) string {
	return func(issue string) string {
		var lines []string
		for _, inst := range reg.ForIssue(issue) {
			line := fmt.Sprintf("- %q (session %s): %s", label(inst), inst.SessionID, describeStatus(inst.Status))
			if inst.Phase != "" {
				line += fmt.Sprintf(", phase %q", inst.Phase)
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n")
	}
}

func describeStatus(status string) string {
	switch status {
	case "running":
		return "running (busy with a turn)"
	case "idle":
		return "idle (waiting for its next instruction)"
	case "blocked":
		return "blocked (needs a decision)"
	case "manual":
		return "being used by a person directly - leave it alone"
	}
	return status
}

// HandoffSummary is the first message sent into a freshly-rotated Senapati
// session: a short orientation, not a cold start.
func HandoffSummary(sainiks []attention.Instance, retiredFrom string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This is a fresh Senapati session, continuing from a rotated-out predecessor (%s) whose context filled up.", retiredFrom)
	if len(sainiks) == 0 {
		b.WriteString(" No sainiks are currently registered.")
		return b.String()
	}
	fmt.Fprintf(&b, " Currently registered sainiks (%d):", len(sainiks))
	for _, inst := range sainiks {
		fmt.Fprintf(&b, "\n- %q (session %s): status=%s", label(inst), inst.SessionID, inst.Status)
		if inst.Phase != "" {
			fmt.Fprintf(&b, ", phase=%q", inst.Phase)
		}
	}
	return b.String()
}
