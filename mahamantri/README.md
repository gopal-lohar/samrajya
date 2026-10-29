# mahamantri

The relay between Linear and the agent sessions that work in Samrajya. It
runs one session as "Senapati" (the manager agent) and sends it short
notifications: (a) **Linear pings**, meaning a comment that mentions Senapati
or a reply in a thread it wrote in, and (b) **sainik notices**, meaning events
on the sessions ("sainiks") that do the work, one per issue. It also gives
Senapati its tools: one local API to start, message and inspect sainiks.

Sessions run on one of two **harnesses** (`harness:` in the config):

- **`claude`**: Claude Code, run by mahamantri itself. This is the way to use
  a Claude subscription, plus anything your Claude Code setup routes to (for
  example Codex models through a proxy). See
  [The Claude Code harness](#the-claude-code-harness).
- **`opencode`** (the default): an already-running `opencode serve`, driven
  through its API.

Built from two earlier prototypes, an opencode event bridge (SSE client,
attention registry/broadcaster/HTTP API) and a Linear webhook receiver
(signature/timestamp verification, full-payload logging), whose code now lives
in the `opencode/`, `attention/` and `linear/` packages here.

## Native mode

With `mode: native`, mahamantri runs no Senapati session, sainiks or TUI. Your
main assistant is an interactive Claude Code session that you start yourself,
reachable through Remote Control, with the Samrajya channel plugin
(`../integrations/claude-channel`). Mahamantri is its bridge:

- **Durable Linear ingress.** A ping is acknowledged to Linear only after it is
  stored, in `native.store`, a bbolt file locked against a second mahamantri.
  Otherwise the webhook answers 503 and Linear redelivers. Redeliveries are
  kept once.
- **A local bridge API** on `native.listenAddr` (loopback only), authenticated
  with a token mahamantri writes, fresh on every start, to `native.discoveryFile`.
  Through it the plugin attaches as the main session, receives pending pings,
  which are replayed until accepted, accepts them into tasks, keeps tasks and
  the action journal up to date, and reads and writes the brain.
- **Trust.** `native.trustedUserIDs` lists the Linear users who may ask for
  work. Pings from anyone else still arrive, marked untrusted.

```yaml
mode: native
workspace: "../repos"
brain: "../brain"
linear:
  signingSecret: ""      # or LINEAR_SIGNING_SECRET
  listenAddr: ":8080"
  botUserID: "<the bot user's Linear id>"
  botHandle: "<what comments @-mention it as>"
native:
  trustedUserIDs: ["<your Linear user id>"]
```

Native mode ignores the managed settings (`harness`, `opencode`, `attention`,
`senapati`, models). Threads known from managed mode's `state.linearThreadsFile`
are imported once. Ctrl+C stops mahamantri only, not your session; pings that
arrive meanwhile wait for Linear's redelivery, and pings already stored wait
in the inbox.

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

## The workspace

`workspace:` names the directory holding the projects this system works on,
one directory per project. In this repo that's `repos/`, which git ignores:
the projects are private and separate from samrajya, and nothing about them
belongs in this repository.

- **Senapati** is told the workspace and the projects in it. The list is read
  whenever its briefing is built. It starts each sainik in the directory of
  the project its issue is about.
- **Sainiks** can't be started with a working directory outside the
  workspace; the gateway refuses it on both harnesses. Every sainik also gets
  standing rules from mahamantri: an appended system prompt on Claude Code,
  an instruction entry on opencode (`samrajya.sainik`). The rules are:
  - work only inside the workspace;
  - never touch another copy of these projects elsewhere on the machine,
    even if a file points to one;
  - read and follow the project's own `AGENTS.md` and `CLAUDE.md` first;
  - never write to Linear;
  - end every turn with a report, including any decision it needs.
- **Limits.** Confinement is by instruction plus the refused directories.
  Sainiks still run with permissions bypassed, so a sainik that ignores its
  rules can reach the rest of the machine. Hard confinement would need
  Claude Code's sandbox (`bubblewrap` on Linux).
- **Self-contained projects.** A project copied into the workspace should not
  point outside it: lock files, notes or git hooks naming absolute paths of
  another copy will send agents there.

## Memory (the brain)

`brain:` names the directory holding Samrajya's memory of the work. In this
repo that's `brain/`, which git ignores. It holds short notes worth knowing
next time, kept by Senapati: a person's standing instructions, facts that are
in neither the code nor Linear, and problems that keep coming back. It isn't
meant to hold everything.

- **Format.** Each note is a markdown file, `brain/<name>.md`, with a
  one-line `description`, an optional `project` (a workspace project), and a
  body. `brain/INDEX.md` is rewritten on every change. You can read, edit or
  delete notes by hand; a file without the header still counts, and its first
  line becomes the description.
- **Kept small.** Notes stay under 1,500 bytes and there are at most 50. At
  the cap, a new note is refused until Senapati merges or deletes old ones.
  `senapati.md` says what is worth a note and what isn't.
- **Senapati** keeps it through `GET/PUT/DELETE /brain/<name>` and
  `GET /brain`. The index is in its briefing, so memory survives rotations
  and restarts. You can also tell it on Linear to remember or forget
  something.
- **Sainiks** started in a project get that project's notes appended to their
  standing rules automatically, with no need for Senapati to pass them on.
  A note's `project` must be one of the workspace's projects.

## The Claude Code harness

Claude Code has no server, so mahamantri runs it. A session is a transcript
on disk (`~/.claude/projects/...`), and mahamantri gives it a process only
while it has work: `claude -p --input-format stream-json --output-format
stream-json`, with `--resume <id>` after its first turn. A process that has
been idle for `claude.idleExit` (default 10m) exits, and the next message
starts a new one. Everything below was verified live against Claude Code
2.1.29x; `claudecode/harness_test.go` replays the same protocol with a fake
`claude`.

- **Setup.** Claude Code must be installed and logged in. Your own
  `~/.claude` settings apply, including `env` (e.g. an
  `ANTHROPIC_BASE_URL` proxy that routes GPT model IDs to Codex) and your MCP
  servers (Senapati needs the `linear` one). No `opencode:` section is
  needed. Models are named by ID alone (`claude-opus-5`, `gpt-6-sol`), not
  `provider/id`.
- **Sainiks** run with permissions bypassed and `AskUserQuestion` disabled,
  so nothing ever waits for an answer. A sainik that needs a decision ends its
  turn and asks in its reply, which reaches Senapati in the finished notice.
  Add flags with `claude.sainikArgs`.
- **Senapati** runs with `--permission-mode dontAsk --tools Bash` and an
  allowlist: `curl` to mahamantri, `jq`, and the Linear MCP tools
  (`mcp__linear__*`). Everything else is denied rather than asked. Adjust it
  with `claude.senapatiAllowedTools` and `claude.senapatiDeniedTools`; a
  denial wins. Its role goes in with `--append-system-prompt`, rendered fresh
  on every request (`--system-prompt-snapshot off`), so editing
  `senapati.md` reaches a resumed Senapati.
- **The API Senapati uses** is `/claude/...` on mahamantri:
  - create a sainik and send its task in one call
  - prompt it, with `queue` (after its current turn, the default) or
    `steer` (into it)
  - interrupt it
  - read its state and latest reply
  - move it to another model
  - delete it
  - read usage limits

  The same rules as the opencode gateway apply: titles are
  `sainik-<ISSUE>-<slug>`, one sainik per issue, and Senapati's own session
  can't be driven through the API. Any unknown path answers with the list.
- **Queue vs steer.** A message written to a running `claude` process is
  folded into its current turn, so `queue` is done by mahamantri: the message
  is held and sent when the turn's result arrives. The queued work runs in
  the same execution, so one finished notice covers both.
- **Interrupt** sends Claude Code's own interrupt request on stdin. The
  running tool call stops, nothing is orphaned, and the process stays up for
  the next message. If the request isn't honored within 10s, the process gets
  SIGINT and anything it started that outlives it is killed.
- **Usage limits.** Every turn reports subscription usage per window (5-hour,
  7-day), shown at `/claude/limits`; the TUI warns at `claude.usageWarnAt`
  (default 90%). A turn stopped by a limit (`"error":"rate_limit"`, "You've
  hit your session limit · resets 2pm (UTC)") pauses the session (state
  `limited`) until the reset. Messages sent to it meanwhile are held. At the
  reset mahamantri tells it to continue, then delivers what was held; nobody
  waits. Senapati gets a notice saying so, with the command to move the
  sainik to another provider's model if it can't wait; the session keeps its
  whole conversation across the switch. `senapati.fallbackModel` does that
  automatically for Senapati itself.
- **Stepping in.** The TUI footer shows `cd <dir> && claude --resume <id>` for
  the selected session. If the session is mid-turn, it shows the interrupt
  command to run first. mahamantri lists open sessions with `claude agents
  --json` every few seconds. A session open in a process it didn't start is
  yours: it is marked `manual`, its idle process is let go, messages for it
  are held, and Senapati is told. When you close it, Senapati gets it back
  with your last reply, read from the transcript, and the held messages run.
- **Restarts.** Sessions are remembered in `mahamantri-claude.json`. On
  shutdown, running turns are interrupted. On the next start, Senapati is
  told which sainiks were cut off mid-turn so it can prompt them again.
- **Context use** for Senapati's rotation is measured from a turn's last API
  call: the usage on its last assistant message. Its totals are summed over
  every call in the turn, and for models behind a proxy there is no other
  breakdown. Claude Code assumes a 200k window for proxied models; set the
  real one in `claude.contextWindows` (e.g. `gpt-6-sol: 272000`). About
  70-95k of every session is fixed (tools, system prompt, MCP servers), so
  use `senapati.rotationThreshold: 0.75` on this harness.

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
- `mahamantri-claude.json` - Claude Code harness only: the sessions it runs
  (directory, model, flags, latest reply, usage-limit pauses).
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
