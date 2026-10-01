package senapati

import (
	"fmt"
	"os"
	"strings"
)

// BriefingInfo is everything Senapati needs to know that only mahamantri
// can tell it: who it is on Linear and where the services it must talk to
// live. Instructions is the free-form role text (senapati.instructionsFile).
type BriefingInfo struct {
	LinearUserID string
	LinearName   string
	LinearHandle string
	// MahamantriURL is the one address Senapati needs: the sainik operations,
	// the attention API, and the gateway to the whole opencode API - with
	// credentials handled, so it never sees or tests a password.
	MahamantriURL string
	DefaultModel  string        // provider/id used when a spawn names no model
	Models        []ModelChoice // what it may pick from, with guidance
	Instructions  string
}

// ModelChoice is one model Senapati may pick for a session, with the
// guidance the person gave for when to use it.
type ModelChoice struct {
	ID      string
	Credits string
	Use     string
}

// Briefing is Senapati's role: installed as a durable instruction on every
// Senapati session (or sent as its first message where the server can't hold
// one). Without it Senapati is a stock opencode coding agent - it reads a
// Linear ping as a person asking it to do the task, and starts doing it.
func Briefing(i BriefingInfo) string {
	var b strings.Builder
	b.WriteString("You are Senapati, the manager in the Samrajya system. You orchestrate; you never do the work yourself. " +
		"Every piece of real work on a Linear issue - investigating, reading code, planning, implementing, testing, reviewing - " +
		"is done by that issue's sainik: one separate opencode session per issue, which you start, brief, steer and report on. " +
		"Your tools are restricted to match: you can call Mahamantri with curl and use Linear, nothing else. " +
		"A \"Permission denied\" means you just tried to do a sainik's job - hand it to the sainik instead.\n\n" +
		"Every message you receive comes from Mahamantri, the relay - never from a person typing to you. " +
		"A [Linear ping] means a person mentioned you or replied in your thread on an issue; a [Sainik notice] means a sainik " +
		"finished, failed, is blocked, or was taken over. Each is a signal to look at the issue and move it forward, " +
		"then end your turn. You never wait for anything: Mahamantri tells you when something happens.\n\n")

	if i.LinearUserID != "" || i.LinearName != "" {
		fmt.Fprintf(&b, "Who you are on Linear: the user %q", firstNonEmpty(i.LinearName, "Senapati"))
		if i.LinearUserID != "" {
			fmt.Fprintf(&b, " (user id %s)", i.LinearUserID)
		}
		b.WriteString(". ")
		if i.LinearHandle != "" {
			fmt.Fprintf(&b, "Comments mention you as @%s - that is you, not another person. ", i.LinearHandle)
		}
		b.WriteString("You only hear about comments that mention you and replies in threads you have commented in - " +
			"not about any other change to an issue.\n\n")
	}

	if i.MahamantriURL != "" {
		fmt.Fprintf(&b, "Mahamantri is at %s - always write that address out in full in your curl commands. It runs sainik "+
			"sessions for you and exposes the whole opencode API at %s/opencode/... (documented at %s/opencode/openapi.json) with "+
			"the credentials already handled - you never need, test or ask for an opencode password.\n\n",
			i.MahamantriURL, i.MahamantriURL, i.MahamantriURL)
	}

	if len(i.Models) > 0 || i.DefaultModel != "" {
		b.WriteString("Models - you choose one for every sainik you start (\"model\":\"provider/id\"):\n")
		if i.DefaultModel != "" {
			fmt.Fprintf(&b, "- Default when you name none: %s\n", i.DefaultModel)
		}
		for _, m := range i.Models {
			fmt.Fprintf(&b, "- %s", m.ID)
			if m.Credits != "" {
				fmt.Fprintf(&b, " [credits: %s]", m.Credits)
			}
			if m.Use != "" {
				fmt.Fprintf(&b, " - %s", m.Use)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if text := strings.TrimSpace(i.Instructions); text != "" {
		// The instructions say $MAHAMANTRI; the permissions allow curl to
		// the literal address, so that is what Senapati must write.
		if i.MahamantriURL != "" {
			text = strings.ReplaceAll(text, "$MAHAMANTRI", i.MahamantriURL)
		}
		b.WriteString(text)
		b.WriteString("\n")
	}
	return b.String()
}

// LoadInstructions reads the role text from path, dropping the YAML
// frontmatter an opencode agent file carries (opencode uses that itself;
// here only the body matters). An empty path means no extra instructions.
func LoadInstructions(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("senapati.instructionsFile: %w", err)
	}
	text := string(data)
	if rest, ok := strings.CutPrefix(text, "---\n"); ok {
		if _, body, found := strings.Cut(rest, "\n---\n"); found {
			text = body
		}
	}
	return strings.TrimSpace(text), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
