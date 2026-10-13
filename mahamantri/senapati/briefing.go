package senapati

import (
	"fmt"
	"os"
	"strings"

	"github.com/gopal-lohar/samrajya/mahamantri/brain"
	"github.com/gopal-lohar/samrajya/mahamantri/harness"
)

// BriefingInfo is everything Senapati needs to know that only mahamantri
// can tell it: who it is on Linear, where mahamantri is, and how to drive
// sainiks on the harness in use. Instructions is the free-form role text
// (senapati.instructionsFile).
type BriefingInfo struct {
	LinearUserID string
	LinearName   string
	LinearHandle string
	// MahamantriURL is the one address Senapati needs - with credentials
	// handled, so it never sees or tests a password.
	MahamantriURL string
	// Harness is "opencode" or "claude": what sainik sessions are, and so
	// which commands drive them (see harness.Commands).
	Harness string
	// Workspace is the directory holding the projects, and Projects the
	// directories in it - read when the briefing is built, so they are never
	// part of this program.
	Workspace string
	Projects  []string
	// Memory is whether the brain is on; MemoryIndex its notes, one line
	// each, as they are when the briefing is built.
	Memory       bool
	MemoryIndex  string
	DefaultModel string        // used when a spawn names no model
	Models       []ModelChoice // what it may pick from, with guidance
	Instructions string
}

// ModelChoice is one model Senapati may pick for a session, with the
// guidance the person gave for when to use it.
type ModelChoice struct {
	ID      string
	Credits string
	Use     string
}

// Briefing is Senapati's role: installed as a durable instruction on every
// Senapati session (or sent as its first message where that can't be held).
// Without it Senapati is a stock coding agent - it reads a Linear ping as a
// person asking it to do the task, and starts doing it.
func Briefing(i BriefingInfo) string {
	cmds := harness.For(i.Harness, i.MahamantriURL)
	kind := "opencode session"
	linear := "Linear is not a separate tool: you call it from inside the `execute` tool, " +
		"e.g. `return await tools.linear.get_issue({id:\"ENG-33\"})`, `tools.linear.list_comments({issueId:\"ENG-33\"})`, " +
		"`tools.linear.save_comment({issueId:\"ENG-33\", body:\"...\"})`, `tools.linear.save_issue(...)` to change its status; " +
		"use `search({query:\"linear\"})` inside `execute` to look up any other Linear tool. "
	if cmds.Name() == "claude" {
		kind = "Claude Code session"
		linear = "Linear's tools are `mcp__linear__*` - e.g. `mcp__linear__get_issue`, `mcp__linear__list_comments`, " +
			"`mcp__linear__save_comment`, and `mcp__linear__save_issue` to change its status. "
	}

	var b strings.Builder
	b.WriteString("You are Senapati, the manager in the Samrajya system. You orchestrate; you never do the work yourself. " +
		"Every piece of real work on a Linear issue - investigating, reading code, planning, implementing, testing, reviewing - " +
		"is done by that issue's sainik: one separate " + kind + " per issue, which you start, brief, steer and report on. " +
		"Your tools are restricted to match. The only shell commands that run are `curl` to Mahamantri's address and `jq` " +
		"(alone or as `curl ... | jq ...`); every other command - grep, cat, ls, sleep, a curl anywhere else - is denied, " +
		"and so is every tool except Linear's. " + linear +
		"A \"Permission denied\" is about that one command, never a sign that Mahamantri " +
		"or Linear is down: rewrite it as plain curl (pipe to jq if you need to filter), or if it was real work, hand it to a sainik. " +
		"The calls for driving sainiks are in \"Your tools\" below, and every ping and notice spells out the exact commands for " +
		"its case. When something fails, report the exact command and error; never guess at the cause.\n\n" +
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

	if i.MahamantriURL != "" && cmds.Name() == "claude" {
		fmt.Fprintf(&b, "Mahamantri is at %s - always write that address out in full in your curl commands. It runs "+
			"your sainiks as Claude Code sessions, with logins and permissions already handled - you never need, test or "+
			"ask for credentials. A session you create there is a sainik: Mahamantri sets it up and tells you when it "+
			"finishes or needs you.\n\n", i.MahamantriURL)
	} else if i.MahamantriURL != "" {
		fmt.Fprintf(&b, "Mahamantri is at %s - always write that address out in full in your curl commands. The whole "+
			"opencode API is at %s/opencode/api/... (described at %s/opencode/openapi.json) with the credentials already "+
			"handled - you never need, test or ask for an opencode password. A session you create there is a sainik: "+
			"Mahamantri sets it up and tells you when it finishes or needs an answer.\n\n",
			i.MahamantriURL, i.MahamantriURL, i.MahamantriURL)
	}

	if i.Workspace != "" {
		dirField := `"location":{"directory":"<project>"}`
		if cmds.Name() == "claude" {
			dirField = `"directory":"<project>"`
		}
		fmt.Fprintf(&b, "Your workspace is %s: the projects you work on, one directory each", i.Workspace)
		if len(i.Projects) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(i.Projects, ", "))
		}
		fmt.Fprintf(&b, ". Start every sainik in the directory of the project its issue is about - add %s to the "+
			"create call, relative to the workspace. Sainiks cannot be started outside the workspace, and Mahamantri gives every "+
			"sainik standing rules: stay inside the workspace, never touch copies of these projects elsewhere on the machine, "+
			"follow the project's own AGENTS.md / CLAUDE.md, never write to Linear. You don't need to repeat those; do name the "+
			"project in the task. If you can't tell which project an issue is about, ask on the issue.\n\n", dirField)
	}

	if i.Memory && i.MahamantriURL != "" {
		b.WriteString("Your memory (Samrajya's brain) - what earlier work taught that is worth knowing now; see " +
			"\"Memory\" in your instructions for what belongs there. Notes for a project reach every sainik started in " +
			"that project by themselves; pass a general note on in a task when it matters there.\n")
		if strings.TrimSpace(i.MemoryIndex) == "" {
			b.WriteString("(no notes yet)\n")
		} else {
			b.WriteString(i.MemoryIndex)
		}
		fmt.Fprintf(&b, "Commands (at most %d notes, each under %d bytes; the -d body follows the same quoting rule as your other commands):\n"+
			"  curl -s %[3]s/brain/<name>                     read a note\n"+
			"  curl -s -X PUT %[3]s/brain/<name> -H 'Content-Type: application/json' -d '{\"description\":\"<one line>\",\"project\":\"<project, or omit for everywhere>\",\"body\":\"<the note>\"}'   add or replace a note\n"+
			"  curl -s -X DELETE %[3]s/brain/<name>           forget a note\n"+
			"  curl -s %[3]s/brain                            list notes\n\n",
			brain.MaxNotes, brain.MaxBody, i.MahamantriURL)
	}

	if len(i.Models) > 0 || i.DefaultModel != "" {
		if cmds.Name() == "claude" {
			b.WriteString("Models - you choose one for every sainik you start (\"model\":\"<model id>\"):\n")
		} else {
			b.WriteString("Models - you choose one for every sainik you start (\"model\":\"provider/id\"):\n")
		}
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
	if i.MahamantriURL != "" {
		// The calls for driving sainiks depend on the harness, so they are
		// written here rather than in the instructions file.
		b.WriteString("\n" + cmds.Tools())
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
