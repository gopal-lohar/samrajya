# mahamantri

The message-queue glue between Linear and opencode in Samrajya: connects
to an already-running `opencode` server, creates/resumes one session as
"Senapati" (the manager agent), and forwards it short natural-language
summaries of (a) Linear webhook deliveries and (b) events on any opencode
session Senapati has asked to be watched ("sainiks", via the attention
API). Senapati's own reasoning, replying to Linear, and spawning sainik
sessions are its own job via its own tool/MCP access - not this program's.

Built from two earlier prototypes, an opencode event bridge (SSE client,
attention registry/broadcaster/HTTP API) and a Linear webhook receiver
(signature/timestamp verification, full-payload logging), whose code now lives
in the `opencode/`, `attention/` and `linear/` packages here.

## Run

```sh
cp mahamantri.yaml.example mahamantri.yaml   # fill in serverURL/password/signingSecret
go run . mahamantri.yaml                     # or: mahamantri /path/to/mahamantri.yaml
```

Does not start the opencode server - point `opencode.serverURL` at one you
already have running. Startup checks everything first (server reachable and
password accepted, both ports free, registry file readable) and exits with a
message saying what to fix, before any session is created.

### Making Senapati act

Senapati is a stock opencode session until it is told otherwise, so
mahamantri briefs every Senapati session with its first message: who it is on
Linear (`linear.botUserID`/`botName`/`botHandle` - so `@handle` in a comment
is recognised as itself), where the opencode server and the attention API are,
and the role text in `senapati.instructionsFile` (`../senapati/senapati.md`).
A session created before this existed is briefed once on the next start.
Events that need action are worded as instructions ("assigned issue SEN-30 to
you ... take it on now"); the rest are informational.

Set `senapati.model`. Sessions created through the API get the opencode
*server's* default model, not the one the opencode TUI shows in its composer
(that is client-side state). The model in use is on the Senapati row in the
TUI. A resumed session is switched to the configured model on start.

Senapati creates sainik sessions by calling the opencode API from its shell,
authenticating with `$OPENCODE_SERVER_PASSWORD` - which is only set in its
shell if the server was started with it. That is one more reason to pin it:

### The opencode password

`opencode serve` picks a **new random password on every start**. Two ways to
live with that:

- Pin it once: `OPENCODE_SERVER_PASSWORD=<fixed> opencode serve --port 4096`,
  and put the same value in `opencode.password`. It never goes stale.
- Or paste the new one into the yaml whenever the server restarts. mahamantri
  watches the file: the new password is applied **while it runs**, the TUI
  shows a red line while the old one is being rejected, and messages for
  Senapati wait in a queue meanwhile and are delivered once it's fixed.

Editing any other setting while running is noticed too, and the TUI tells
you a restart is needed.

## The TUI

Shows the current Senapati session (ID, and when it started), its retired
predecessors, and every registered sainik with live status. It's populated
from the first frame. The footer always shows the exact command that opens
the selected session in the real opencode TUI - including `--server` and the
password, since without them `opencode` talks to its default background
service, which doesn't have these sessions:

```
OPENCODE_SERVER_PASSWORD=... opencode --server http://localhost:4096 --session ses_...
```

`up`/`down` select, `q`/`ctrl+c` quit. Problems (server rejecting the
password, unreachable server, failed deliveries) appear as red lines at the
top instead of only in the log.

## Where things are saved

Next to the config file, whatever directory you launch from:

- `mahamantri-senapati.json` - **the Senapati session IDs**: current, retired
  history, and a counter for `Senapati-<n>` numbering. Restarting resumes the
  current session from here if the server still has it. It's a state file
  rather than part of the yaml because mahamantri rewrites it (on rotation)
  and rewriting your hand-edited config would clobber it.
- `mahamantri-registry.json` - registered sainiks.
- `mahamantri.log` - every Linear delivery in full, plus warnings.

## What it does

- **Attention API** (`POST /instances`, `DELETE /instances/{id}`,
  `GET /instances`, `GET /events`): Senapati registers the sessions it wants
  watched. An ID that doesn't exist on the opencode server is rejected
  (`404`); a missing `label` defaults to the session's own title (so
  `sainik-<issue>-<text>` titles show up); Senapati's own current session is
  always rejected (`403`) - registering it would feed it its own activity.
- **Linear webhook**: verifies `Linear-Signature`/`Linear-Timestamp`, logs the
  full payload, and forwards a short summary saying what changed ("assigned to
  Senapati", `state changed to "Todo"` - from the payload's `updatedFrom`).
  Set `linear.botUserID` to the Linear user Senapati acts as, or everything
  Senapati does on Linear comes straight back to it as a new prompt.
- **Delivery to Senapati** is one ordered queue (`delivery: queue`, tagged
  `metadata.source: "mahamantri"`): messages arrive in the order received,
  never block the webhook, and are retried while the server is unreachable.
- **Sainik attention**: blocking events and turn completions on a registered
  session are forwarded.
- **Manual takeover**: a message on a registered sainik that wasn't sent by
  mahamantri or Senapati (no `metadata.source` tag), or a `reason:"user"`
  interrupt, marks it `manual` until its turn ends and tells Senapati what the
  person did. Only the registered session itself is judged, not its subagents.
- **Rotation**: once Senapati's context usage crosses
  `senapati.rotationThreshold` (from the latest *completed* assistant
  message), a fresh `Senapati-<n>` is created, the old one retired into
  history, and the new one's first message is a handoff summary of the
  registered sainiks. If usage can't be measured (e.g. right after a server
  restart, while its model list reloads), the message is still delivered and
  the TUI shows why.

## Senapati custom agent

`../senapati/senapati.md` is a test fixture: an opencode custom-agent
definition describing Senapati's role, the sainik naming convention
(`sainik-<issueID>-<text>`), and the `metadata.source:"senapati"` tagging
convention Senapati must use if it ever prompts a sainik directly (so it isn't
mistaken for a person). Opencode discovers agents from
`<config-root>/agent/<name>.md` - copy or symlink it into
`~/.config/opencode/agent/` or a project-level `.opencode/agent/` before
pointing `senapati.agent` at it.
