// Package harness describes, per agent harness (opencode or Claude Code), how
// Senapati drives sainiks: the tools section of its briefing, and the exact
// commands that pings and notices spell out. Senapati left to work out an API
// itself improvised and stalled, so every message carries ready-to-run
// commands - and those depend on which harness the sessions run on.
package harness

import (
	"fmt"
	"strings"
)

// Commands is one harness's command vocabulary, against mahamantri at Base.
type Commands interface {
	// Name is "opencode" or "claude".
	Name() string
	// Base is mahamantri's address.
	Base() string
	// Tools is the "Your tools" section of Senapati's briefing.
	Tools() string
	// Start is the command(s) that start the issue's sainik.
	Start(issue string) string
	// Prompt queues a message for a sainik (after its current turn).
	Prompt(id string) string
	// Interrupt stops a sainik now.
	Interrupt(id string) string
	// Running tells whether a sainik is mid-turn.
	Running(id string) string
	// LastReply fetches a sainik's latest reply.
	LastReply(id string) string
}

// For returns the commands for harness name ("claude" or anything else for
// opencode) against mahamantri at base.
func For(name, base string) Commands {
	base = strings.TrimRight(base, "/")
	if name == "claude" {
		return Claude{base}
	}
	return Opencode{base}
}

const quoting = "The `-d '...'` bodies are single-quoted, so an apostrophe inside the text\n" +
	"ends the quote and breaks the command. Write it as `\\u0027` instead, for\n" +
	"example `{\"text\":\"don\\u0027t change the API\"}`; JSON reads it back as `'`.\n\n" +
	"Every ping and notice also spells out the exact commands for its case. Don't\n" +
	"look for other Mahamantri endpoints: an unknown path just returns this same\n" +
	"list.\n"

const denied = "Everything else is denied: you cannot read files, search code, run builds,\n" +
	"browse, or start subagents. A \"Permission denied\" applies to that one\n" +
	"command. It never means Mahamantri or Linear is unavailable. Rewrite the\n" +
	"command as plain `curl` (piped to `jq` to filter), or, if it was real work,\n" +
	"hand that work to the sainik. When something fails, report the exact command\n" +
	"and the error. Never guess at the cause.\n\n"

const phase = "- **Record where a sainik is.** Its phase survives if you are rotated into a\n" +
	"  fresh session, and appears in every ping:\n" +
	"  ```sh\n" +
	"  curl -s -X PATCH %[1]s/instances/<id> -H 'Content-Type: application/json' -d '{\"phase\":\"<free text>\"}'\n" +
	"  ```\n\n"

// Opencode drives sainiks through the opencode API, proxied by mahamantri.
type Opencode struct{ B string }

func (o Opencode) Name() string { return "opencode" }
func (o Opencode) Base() string { return o.B }
func (o Opencode) api() string  { return o.B + "/opencode/api/session" }

func (o Opencode) Start(issue string) string {
	return fmt.Sprintf("  curl -s -X POST %s -H 'Content-Type: application/json' -d '{\"title\":\"sainik-%s-<short-name>\"}' | jq -r .data.id\n"+
		"  curl -s -X POST %s/<id>/prompt -H 'Content-Type: application/json' -d '{\"text\":\"<self-contained task; tell it to read %s on Linear itself>\"}'",
		o.api(), issue, o.api(), issue)
}
func (o Opencode) Prompt(id string) string {
	return fmt.Sprintf("  curl -s -X POST %s/%s/prompt -H 'Content-Type: application/json' -d '{\"text\":\"<what is new>\"}'", o.api(), id)
}
func (o Opencode) Interrupt(id string) string {
	return fmt.Sprintf("  curl -s -X POST %s/%s/interrupt", o.api(), id)
}
func (o Opencode) Running(id string) string {
	return fmt.Sprintf("  curl -s %s/active | jq '.data[\"%s\"]'", o.api(), id)
}
func (o Opencode) LastReply(id string) string {
	return fmt.Sprintf("curl -s '%s/%s/message?type=assistant&order=desc&limit=1' | jq -r '.data[0].content[] | select(.type==\"text\") | .text'", o.api(), id)
}

func (o Opencode) Tools() string {
	return strings.ReplaceAll(`## Your tools

You have exactly two:

- **The shell, for `+"`curl`"+` to `+"`$MAHAMANTRI`"+` and `+"`jq`"+`.** Run them alone or as
  `+"`curl ... | jq ...`"+`. Every other command is denied, including `+"`head`"+`,
  `+"`grep`"+`, `+"`cat`"+`, `+"`ls`"+`, `+"`sleep`"+`, and `+"`curl`"+` to any other address.
- **Linear, from inside the `+"`execute`"+` tool.** Linear is not a separate tool:
  you call it in code, for example:

  `+"```js"+`
  return await tools.linear.get_issue({id: "ENG-33"})
  return await tools.linear.list_comments({issueId: "ENG-33"})
  return await tools.linear.save_comment({issueId: "ENG-33", body: "..."})
  `+"```"+`

  Use `+"`tools.linear.save_issue`"+` to change an issue's status. Inside
  `+"`execute`"+`, `+"`search({query: \"linear\"})`"+` finds any other Linear tool.

`+denied+`### Sainiks: the opencode API through Mahamantri

`+"`$MAHAMANTRI/opencode/...`"+` is the real opencode API, with the credentials
handled; the full description is at `+"`$MAHAMANTRI/opencode/openapi.json`"+`.
These are the calls you need:

- **Start the issue's sainik.** Create the session, then prompt it with the
  task. The title must be `+"`sainik-<ISSUE>-<slug>`"+`, with a lowercase slug.
  Mahamantri fills in the sainik's working directory, model and permissions,
  and starts watching it, so you hear when it finishes.
  `+"```sh"+`
  curl -s -X POST $MAHAMANTRI/opencode/api/session -H 'Content-Type: application/json' -d '{"title":"sainik-ENG-33-fix-login"}' | jq -r .data.id
  curl -s -X POST $MAHAMANTRI/opencode/api/session/<id>/prompt -H 'Content-Type: application/json' -d '{"text":"<a complete, self-contained task>"}'
  `+"```"+`
  To pick a model other than the default from your briefing, add
  `+"`\"model\":{\"providerID\":\"openai\",\"id\":\"gpt-6-sol\"}`"+` to the create body. If
  the issue already has a sainik, the create call is refused with that
  sainik's session ID; prompt that one instead. Add `+"`?parallel=true`"+` to the
  create URL only when the issue splits into parts that are truly
  independent.

- **Send a sainik work:**
  `+"```sh"+`
  curl -s -X POST $MAHAMANTRI/opencode/api/session/<id>/prompt -H 'Content-Type: application/json' -d '{"text":"<instruction>"}'
  `+"```"+`
  The default `+"`\"delivery\": \"queue\"`"+` delivers the message after the sainik's
  current turn, or right away if it is idle. `+"`\"delivery\": \"steer\"`"+` delivers
  it at the sainik's next step, inside its current turn.

- **Stop a sainik now**, even in the middle of a tool call, then prompt it
  with what to do instead. It keeps its context:
  `+"```sh"+`
  curl -s -X POST $MAHAMANTRI/opencode/api/session/<id>/interrupt
  `+"```"+`

- **Is it running?** A session listed here is running; one that is absent
  is idle:
  `+"```sh"+`
  curl -s $MAHAMANTRI/opencode/api/session/active | jq '.data["<id>"]'
  `+"```"+`

- **Its latest reply:**
  `+"```sh"+`
  curl -s '$MAHAMANTRI/opencode/api/session/<id>/message?type=assistant&order=desc&limit=1' | jq -r '.data[0].content[] | select(.type=="text") | .text'
  `+"```"+`

- **Answer what it is waiting on.** Each notice about a permission request or
  a question gives the exact command.

`+fmt.Sprintf(phase, "$MAHAMANTRI")+`- **When the issue is done:** `+"`curl -s -X DELETE $MAHAMANTRI/instances/<id>`"+`
  stops watching the sainik. `+"`DELETE $MAHAMANTRI/opencode/api/session/<id>`"+`
  deletes it entirely.

`+quoting, "$MAHAMANTRI", o.B)
}

// Claude drives sainiks that are Claude Code sessions run by mahamantri.
type Claude struct{ B string }

func (c Claude) Name() string { return "claude" }
func (c Claude) Base() string { return c.B }
func (c Claude) api() string  { return c.B + "/claude/session" }

func (c Claude) Start(issue string) string {
	return fmt.Sprintf("  curl -s -X POST %s -H 'Content-Type: application/json' -d '{\"title\":\"sainik-%s-<short-name>\",\"text\":\"<self-contained task; tell it to read %s on Linear itself>\"}'",
		c.api(), issue, issue)
}
func (c Claude) Prompt(id string) string {
	return fmt.Sprintf("  curl -s -X POST %s/%s/prompt -H 'Content-Type: application/json' -d '{\"text\":\"<what is new>\"}'", c.api(), id)
}
func (c Claude) Interrupt(id string) string {
	return fmt.Sprintf("  curl -s -X POST %s/%s/interrupt", c.api(), id)
}
func (c Claude) Running(id string) string {
	return fmt.Sprintf("  curl -s %s/%s | jq .state", c.api(), id)
}
func (c Claude) LastReply(id string) string {
	return fmt.Sprintf("curl -s %s/%s | jq -r .lastReply", c.api(), id)
}

func (c Claude) Tools() string {
	return strings.ReplaceAll(`## Your tools

You have exactly two:

- **Bash, for `+"`curl`"+` to `+"`$MAHAMANTRI`"+` and `+"`jq`"+`.** Run them alone or as
  `+"`curl ... | jq ...`"+`. Every other command is denied, including `+"`grep`"+`,
  `+"`cat`"+`, `+"`ls`"+`, `+"`sleep`"+`, and `+"`curl`"+` to any other address.
- **Linear, through its tools** (`+"`mcp__linear__get_issue`"+`,
  `+"`mcp__linear__list_comments`"+`, `+"`mcp__linear__save_comment`"+`, and
  `+"`mcp__linear__save_issue`"+` to change an issue's status).

`+denied+`### Sainiks: Claude Code sessions run by Mahamantri

Each sainik is a Claude Code session that Mahamantri runs for you. These are
the calls you need:

- **Start the issue's sainik and give it its task**, in one call. The title
  must be `+"`sainik-<ISSUE>-<slug>`"+`, with a lowercase slug. Mahamantri fills
  in the sainik's working directory, model and permissions, and starts
  watching it, so you hear when it finishes.
  `+"```sh"+`
  curl -s -X POST $MAHAMANTRI/claude/session -H 'Content-Type: application/json' -d '{"title":"sainik-ENG-33-fix-login","text":"<a complete, self-contained task>"}'
  `+"```"+`
  It answers with the session's `+"`id`"+`. To pick a model other than the
  default from your briefing, add `+"`\"model\":\"<model id>\"`"+` to the body. If
  the issue already has a sainik, the call is refused with that sainik's
  session ID; prompt that one instead. Add `+"`?parallel=true`"+` to the URL
  only when the issue splits into parts that are truly independent.

- **Send a sainik work:**
  `+"```sh"+`
  curl -s -X POST $MAHAMANTRI/claude/session/<id>/prompt -H 'Content-Type: application/json' -d '{"text":"<instruction>"}'
  `+"```"+`
  The default `+"`\"delivery\": \"queue\"`"+` delivers the message after the sainik's
  current turn, or right away if it is idle. `+"`\"delivery\": \"steer\"`"+` delivers
  it at its next step, inside its current turn.

- **Stop a sainik now**, even in the middle of a tool call, then prompt it
  with what to do instead. It keeps its context:
  `+"```sh"+`
  curl -s -X POST $MAHAMANTRI/claude/session/<id>/interrupt
  `+"```"+`

- **Its state and latest reply.** The state is `+"`running`"+`, `+"`idle`"+`, `+"`manual`"+`
  (a person is using it - leave it alone) or `+"`limited`"+` (paused by a usage
  limit; it resumes by itself at `+"`limitedUntil`"+`):
  `+"```sh"+`
  curl -s $MAHAMANTRI/claude/session/<id> | jq '{state, model, limitedUntil, lastReply}'
  `+"```"+`

- **Move it to another model**, for example off a provider whose usage limit
  it hit. It keeps its whole conversation; prompt it afterwards to continue:
  `+"```sh"+`
  curl -s -X POST $MAHAMANTRI/claude/session/<id>/model -H 'Content-Type: application/json' -d '{"model":"<model id>"}'
  `+"```"+`

- **Usage limits.** How much of each subscription window is used, and when it
  resets:
  `+"```sh"+`
  curl -s $MAHAMANTRI/claude/limits
  `+"```"+`

`+fmt.Sprintf(phase, "$MAHAMANTRI")+`- **When the issue is done:** `+"`curl -s -X DELETE $MAHAMANTRI/instances/<id>`"+`
  stops watching the sainik. `+"`curl -s -X DELETE $MAHAMANTRI/claude/session/<id>`"+`
  stops it and forgets it entirely.

`+quoting, "$MAHAMANTRI", c.B)
}
