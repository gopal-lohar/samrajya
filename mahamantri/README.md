# mahamantri

The relay between Linear and opencode in Samrajya. It connects to an
already-running `opencode` server, creates or resumes one session as
"Senapati" (the manager agent), and sends it short notifications:
(a) **Linear pings**, meaning a comment that mentions Senapati or a reply in a
thread it wrote in, and (b) **sainik notices**, meaning events on the opencode
sessions ("sainiks") that do the work, one per issue. It also gives Senapati
its tools: one local API to start, message and inspect sainiks, plus a
credential-holding gateway to the whole opencode API.

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

Senapati is a manager: every piece of real work on an issue happens in that
issue's sainik. Three things hold it to that:

- **Its tools are restricted.** Every Senapati session is created (or, when
  resumed, patched) with an opencode permission ruleset that denies
  everything except `curl` to mahamantri, `jq`, and the Linear MCP tools
  (`linear_*`). It cannot read, grep, edit, build, browse or start subagents,
  so the work has nowhere to go but a sainik. Opencode's session-wait endpoint
  and `sleep` are denied too, because Senapati is told when a sainik
  finishes and never waits. `senapati.extraPermissions` appends rules, for
  example another MCP server. See `senapati/guardrails.go`.
- **Its role is durable.** The briefing covers who it is on Linear
  (`linear.botUserID`/`botName`/`botHandle`), where mahamantri is, the models
  it may pick, and the role text in `senapati.instructionsFile`
  (`../senapati/senapati.md`). It is installed as a session *instruction
  entry*, refreshed on every start, so it survives context compaction; a
  first message would be summarised away. A short activation message
  follows. Servers without instruction entries get the briefing as the first
  message instead.
- **Its messages say what they are.** `[Linear ping]` and `[Sainik notice]`
  messages say they are notifications rather than a person chatting. A ping
  carries the issue's sainik and its state, and says how to route the ping:
  queue it for a busy sainik, interrupt only if urgent, start a sainik if
  there is none, or answer on Linear.

Set `senapati.model`. Sessions created through the API get the opencode
*server's* default model, not the one the opencode TUI shows in its composer
(that is client-side state). The model in use is on the Senapati row in the
TUI. A resumed session is switched to the configured model on start.

A Senapati session created before these guardrails existed gets the
permissions and the role when it is resumed, but its old context still shows
it doing work itself. To start clean, remove `current` from
`mahamantri-senapati.json` (or delete the file) before starting.

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
- `mahamantri-linear-threads.json` - Linear comment threads Senapati has
  written in, so replies in them reach it.
- `mahamantri.log` - every Linear delivery in full, plus warnings.

## What it does

- **Attention API** (`POST /instances`, `DELETE /instances/{id}`,
  `GET /instances`, `GET /events`): Senapati registers the sessions it wants
  watched. An ID that doesn't exist on the opencode server is rejected
  (`404`); a missing `label` defaults to the session's own title (so
  `sainik-<issue>-<text>` titles show up); Senapati's own current session is
  always rejected (`403`) - registering it would feed it its own activity.
- **Linear webhook**: verifies `Linear-Signature`/`Linear-Timestamp` and
  logs every delivery in full. Only two things reach Senapati: a person's new
  comment that @-mentions `linear.botHandle`, and a person's new reply in a
  comment thread Senapati has written in. Everything else is dropped with a
  logged reason: issue edits (status, labels, assignee, priority...), comment
  edits, integrations' comments, and Senapati's own comments. Linear says
  which thread a reply belongs to but not who wrote that thread, so the
  threads Senapati writes in are remembered as its own comment webhooks
  arrive (`mahamantri-linear-threads.json`).
- **opencode gateway** (`/opencode/...`): the whole opencode API, with
  credentials added. Senapati drives sainiks with it directly (create,
  prompt, interrupt, read messages, answer permission requests and
  questions). The gateway adds only what must hold for every call:
  - `POST /api/session` makes a sainik. The title must be
    `sainik-<ISSUE>-<slug>`, and a second sainik for an issue is refused
    (`409`, naming the first) unless `?parallel=true`. Anything the request
    leaves out is filled in from `sainik.directory`, `sainik.defaultModel`,
    `sainik.agent`, and permissions that allow everything: a sainik runs
    unattended, and an "ask" nobody can answer only stalls it. The new
    session is registered before the response returns.
  - Prompts are tagged `metadata.source: "senapati"` and default to
    `delivery: "queue"`, which is delivered after the session's current
    turn. This was verified live: the queued message runs in the same
    execution, so one completion notice covers both.
  - An interrupt is recorded as Senapati's own, so it isn't reported back as
    a person taking the sainik over.
  - Deleting a session unregisters it.
  - Waiting on a session, and changing Senapati's own session, are refused.

  Any other path on mahamantri answers with a list of these calls.
- **Sainik notices**: a sainik's own turn ending (including its final
  reply), and a permission request or question anywhere in its session
  tree (including the exact command to answer it). A subagent finishing is
  not the sainik finishing. Status (`running`/`idle`/`blocked`/`failed`/
  `manual`) changes only on lifecycle events, and is seeded at startup from
  opencode's list of running sessions.
- **Delivery to Senapati** is one ordered queue (`delivery: queue`, tagged
  `metadata.source: "mahamantri"`): messages arrive in the order received,
  never block the webhook, and are retried while the server is unreachable.
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

## Senapati's instructions

`../senapati/senapati.md` is Senapati's role: how to handle a Linear ping and
a sainik notice, the issue workflow (plan, approval, execute, review), and
its tools. It is sent as part of the briefing (its frontmatter is ignored),
with `$MAHAMANTRI` replaced by mahamantri's real address. The permissions
allow `curl` to that literal address, so the address has to appear in the
commands. The file is also a valid opencode agent definition, if you want to
point `senapati.agent` at it: copy or symlink it into
`~/.config/opencode/agent/`.
