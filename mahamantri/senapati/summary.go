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
// for Senapati. Each says what to do about it - Senapati is the worker
// coordinating these sessions, and a bare status line reads as something to
// acknowledge rather than act on.
func SummarizeEvent(ev opencode.Event, inst attention.Instance) string {
	who := fmt.Sprintf("Sainik %q (session %s)", label(inst), inst.SessionID)
	switch {
	case ev.Type == "session.execution.succeeded" || ev.Type == "session.idle":
		return who + " finished its turn. Review what it produced, report progress on Linear, and either give it more work or unregister it."
	case ev.Type == "session.execution.failed":
		return who + " failed. Find out why and decide whether to retry it or take another approach."
	case ev.Type == "session.execution.interrupted":
		return who + " was interrupted."
	case ev.Severity == opencode.Blocking:
		return fmt.Sprintf("%s is blocked: %s. Unblock it or decide how to proceed.", who, ev.Summary)
	}
	return fmt.Sprintf("%s needs attention: %s", who, ev.Summary)
}

// SummarizeManualTakeover formats the "a human took this session over"
// notice sent to Senapati, saying what the person did (the action phrase
// from attention.DetectManualTakeover).
func SummarizeManualTakeover(inst attention.Instance, action string) string {
	return fmt.Sprintf("Sainik %q (session %s) was manually taken over: a person %s. It is no longer purely autonomous.", label(inst), inst.SessionID, action)
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
