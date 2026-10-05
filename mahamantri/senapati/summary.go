package senapati

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gopal-lohar/samrajya/mahamantri/attention"
	"github.com/gopal-lohar/samrajya/mahamantri/linear"
	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

func label(inst attention.Instance) string {
	if inst.Label != "" {
		return inst.Label
	}
	return inst.SessionID
}

// SummarizeEvent formats an attention-required event on a registered sainik
// for Senapati. reply is the sainik's final reply when the event ends its
// turn ("" if unknown), so Senapati has its report without another call.
// Every notice ends with the exact commands for what it calls for, against
// mahamantri at base - Senapati left to work out the API improvised and
// stalled.
func SummarizeEvent(ev opencode.Event, inst attention.Instance, reply, base string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Sainik notice] %q (session %s)", label(inst), inst.SessionID)
	if inst.Phase != "" {
		fmt.Fprintf(&b, ", phase %q,", inst.Phase)
	}
	lastReply := fmt.Sprintf("curl -s '%s/opencode/api/session/%s/message?type=assistant&order=desc&limit=1' | jq -r '.data[0].content[] | select(.type==\"text\") | .text'", base, inst.SessionID)

	switch {
	case ev.Type == "session.execution.succeeded" || ev.Type == "session.idle":
		b.WriteString(" finished its turn and is idle.\n")
		writeReply(&b, reply, lastReply)
		b.WriteString("\nDo the issue's next step: post the result on Linear, prompt it with its next task, or close it out.")
	case ev.Type == "session.execution.failed":
		b.WriteString(" failed - its turn ended with an error.\n")
		writeReply(&b, reply, lastReply)
		b.WriteString("\nRetry it with a sharper instruction, on the default model if it ran out of credits or quota, " +
			"or report the problem on Linear.")
	case ev.Type == "session.execution.interrupted":
		b.WriteString(" was interrupted and is idle. Prompt it if it should continue.")
	case ev.Type == "permission.asked":
		var p struct {
			ID        string   `json:"id"`
			Action    string   `json:"action"`
			Resources []string `json:"resources"`
		}
		json.Unmarshal(ev.Raw, &p)
		fmt.Fprintf(&b, " is waiting for permission to %s %v (request %s, session %s).\n", p.Action, p.Resources, p.ID, ev.SessionID)
		fmt.Fprintf(&b, "Decide it if it is within the issue's scope, otherwise ask on Linear. Answer with:\n"+
			"  curl -s -X POST %s/opencode/api/session/%s/permission/%s/reply -H 'Content-Type: application/json' -d '{\"decision\":\"once\"}'\n"+
			"(\"once\", \"always\" or \"reject\"; add \"message\" to tell it why)", base, ev.SessionID, p.ID)
	case ev.Type == "form.created":
		var f struct {
			Form struct {
				ID     string `json:"id"`
				Fields []struct {
					Key         string `json:"key"`
					Title       string `json:"title"`
					Description string `json:"description"`
					Options     []struct {
						Value string `json:"value"`
					} `json:"options"`
				} `json:"fields"`
			} `json:"form"`
		}
		json.Unmarshal(ev.Raw, &f)
		fmt.Fprintf(&b, " asked a question and is waiting for the answer (form %s, session %s):\n", f.Form.ID, ev.SessionID)
		answer := map[string]string{}
		for _, fl := range f.Form.Fields {
			fmt.Fprintf(&b, "- %s: %s", fl.Key, strings.TrimSpace(fl.Title+" - "+fl.Description))
			if len(fl.Options) > 0 {
				var opts []string
				for _, o := range fl.Options {
					opts = append(opts, fmt.Sprintf("%q", o.Value))
				}
				fmt.Fprintf(&b, " (options: %s)", strings.Join(opts, ", "))
			}
			b.WriteString("\n")
			answer[fl.Key] = "<answer>"
		}
		var example bytes.Buffer
		enc := json.NewEncoder(&example)
		enc.SetEscapeHTML(false) // keep <answer> readable
		enc.Encode(map[string]any{"answer": answer})
		fmt.Fprintf(&b, "Answer it if you can from the issue, otherwise ask on Linear. Reply with:\n"+
			"  curl -s -X POST %s/opencode/api/session/%s/form/%s/reply -H 'Content-Type: application/json' -d '%s'",
			base, ev.SessionID, f.Form.ID, strings.TrimSpace(example.String()))
	default:
		fmt.Fprintf(&b, " needs attention: %s", ev.Summary)
	}
	return b.String()
}

func writeReply(b *strings.Builder, reply, command string) {
	reply = strings.TrimSpace(reply)
	if reply == "" {
		fmt.Fprintf(b, "Its final reply could not be read; fetch it with:\n  %s\n", command)
		return
	}
	const max = 4000
	if len(reply) > max {
		fmt.Fprintf(b, "Its final reply (cut at %d characters; all of it: %s):\n", max, command)
		reply = reply[:max] + "..."
	} else {
		b.WriteString("Its final reply:\n")
	}
	b.WriteString("> " + strings.ReplaceAll(reply, "\n", "\n> ") + "\n")
}

// SummarizeManualTakeover formats the "a human took this session over"
// notice sent to Senapati, saying what the person did (the action phrase
// from attention.DetectManualTakeover).
func SummarizeManualTakeover(inst attention.Instance, action string) string {
	return fmt.Sprintf("[Sainik notice] %q (session %s) was manually taken over: a person %s. "+
		"Leave it alone until they are done - do not message or interrupt it - and note it on the issue if it matters.",
		label(inst), inst.SessionID, action)
}

// IssueSainiks lists, for a Linear ping about issue, the sainiks working on
// it as mahamantri last saw them.
func IssueSainiks(reg interface {
	ForIssue(issue string) []attention.Instance
}) func(issue string) []linear.Sainik {
	return func(issue string) []linear.Sainik {
		var out []linear.Sainik
		for _, inst := range reg.ForIssue(issue) {
			out = append(out, linear.Sainik{Label: label(inst), SessionID: inst.SessionID, Status: inst.Status, Phase: inst.Phase})
		}
		return out
	}
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
