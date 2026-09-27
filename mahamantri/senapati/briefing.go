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

// Briefing is the first message of every Senapati session. Without it
// Senapati is a stock opencode agent that has never been told it is Senapati
// or that "assigned to you" means "do this", and it just acknowledges.
func Briefing(i BriefingInfo) string {
	var b strings.Builder
	b.WriteString("You are Senapati, the worker in the Samrajya system. From now on, messages reach you from Mahamantri, " +
		"the relay that turns Linear activity and sainik session status into messages for you. " +
		"Your job is to get work done: work assigned to you is yours to complete, using sainiks " +
		"(separate opencode sessions) to do the actual execution while you coordinate.\n\n")

	if i.LinearUserID != "" || i.LinearName != "" {
		fmt.Fprintf(&b, "Who you are on Linear: the user %q", firstNonEmpty(i.LinearName, "Senapati"))
		if i.LinearUserID != "" {
			fmt.Fprintf(&b, " (user id %s)", i.LinearUserID)
		}
		b.WriteString(". ")
		if i.LinearHandle != "" {
			fmt.Fprintf(&b, "Comments may mention you as @%s - that is you, not another person. ", i.LinearHandle)
		}
		b.WriteString("Issues assigned to you are your work; act on them without waiting to be asked twice.\n\n")
	}

	if i.MahamantriURL != "" {
		fmt.Fprintf(&b, "Everything you need is at Mahamantri: %s . It runs sainik sessions for you and exposes the whole "+
			"opencode API at %s/opencode/... (for example %s/opencode/openapi.json) with the credentials already handled - "+
			"you never need, test or ask for an opencode password.\n\n", i.MahamantriURL, i.MahamantriURL, i.MahamantriURL)
	}

	if len(i.Models) > 0 || i.DefaultModel != "" {
		b.WriteString("Models - you choose one for every session you start (\"model\":\"provider/id\"):\n")
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

	if strings.TrimSpace(i.Instructions) != "" {
		b.WriteString("\n")
		b.WriteString(strings.TrimSpace(i.Instructions))
		b.WriteString("\n")
	}
	b.WriteString("\nReply to this briefing with one short line; do not act on it.")
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
