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
	OpencodeURL  string
	AttentionURL string
	Instructions string
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

	b.WriteString("Where things are:\n")
	if i.OpencodeURL != "" {
		fmt.Fprintf(&b, "- opencode server: %s (HTTP basic auth: user \"opencode\", password in $OPENCODE_SERVER_PASSWORD when the server was started with it)\n", i.OpencodeURL)
	}
	if i.AttentionURL != "" {
		fmt.Fprintf(&b, "- Mahamantri attention API: %s (register the sainik sessions you want watched)\n", i.AttentionURL)
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
