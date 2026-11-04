# Samrajya channel plugin

The session side of Mahamantri's bridge. In `mode: claude` your main assistant is an
ordinary interactive Claude Code session that you start, talk to in the terminal
or from your phone through Remote Control, and that does the work itself. This
plugin runs inside that session and:

- pushes Linear pings from Mahamantri's durable inbox into the session as
  [channel](https://code.claude.com/docs/en/channels-reference) events;
- gives it tools for the state Mahamantri keeps: accepting pings into tasks,
  task checkpoints and results, and a journal of external actions such as
  Linear replies;
- carries the main session's working rules as its MCP instructions.

Mahamantri stores each ping before acknowledging Linear's webhook. A ping stays
pending until the session accepts it, and each new channel connection replays
it. Delivery is at least once and accepting is idempotent, so a redelivered
ping never creates a second task.

## How it connects

Mahamantri serves a local, token-authenticated API. On every
start it writes the address and a fresh token to its discovery file, by default
`$XDG_STATE_HOME/samrajya/bridge.json` (`~/.local/state/...`), readable only by
you. The plugin reads that file and rereads it whenever Mahamantri restarts.
Set `SAMRAJYA_BRIDGE_FILE` if you configured another path.

Only one session at a time is attached as the main one. A second session with
the plugin loaded stays passive; its tools refuse to change anything until you
tell it to take over (`samrajya_take_over`). It also attaches by itself once
the first has been gone for 90 seconds.

## Install

Requires Node 24+ on `PATH` and pnpm.

```sh
pnpm --dir integrations/claude-channel install --frozen-lockfile --ignore-scripts
pnpm --dir integrations/claude-channel bundle     # builds plugin/server.mjs
claude plugin marketplace add /absolute/path/to/samrajya/integrations
cd /path/to/your/workspace   # where the main session will run
claude plugin install samrajya@samrajya --scope local
```

Only a session running in Mahamantri's `bridge.mainDirectory` (default: the
workspace) attaches as the main session by itself. Claude Code doesn't tell a
plugin whether its session enabled the channel, but the plugin runs in its
session's directory, so a session elsewhere stays passive unless you tell it
to take over. Installing with `--scope local` also keeps the plugin out of
unrelated projects.

Claude Code copies the plugin when installing it. After rebuilding, run
`claude plugin update samrajya@samrajya` (bump the version in
`plugin/.claude-plugin/plugin.json` if the update says nothing changed).

### Allow the channel

During the channels research preview, only allowlisted channels deliver
messages. In a background session, the development flag
(`--dangerously-load-development-channels`) is ignored. Add the plugin to the
machine's managed settings, which are root-owned by design:

```json
// /etc/claude-code/managed-settings.json
{ "allowedChannelPlugins": [{ "marketplace": "samrajya", "plugin": "samrajya" }] }
```

On a Team or Enterprise plan, an Owner can set the same list, and must enable
channels, in the organization's Claude Code admin settings. Setting the list
replaces Anthropic's default allowlist of official channel plugins.

### Start the main session

Start Mahamantri first, then the session, with
`integrations/claude-channel/restart-main.sh`. It runs the session in the main
directory, so project instructions apply:

```sh
integrations/claude-channel/restart-main.sh           # stop it and start a new one
integrations/claude-channel/restart-main.sh --resume  # stop it and resume its last conversation
integrations/claude-channel/restart-main.sh --ensure  # start it only if none is running
```

Use it after changing the session's settings or your login too. It stops
every session with that name in the main directory by id first: only one
session holds the main attachment, so a leftover session keeps receiving pings
while a new one stays passive, and `claude stop <name>` doesn't reliably find
a session when several share the name. A new session starts a new
conversation; open tasks with their checkpoints, the brain and the Linear
threads carry the work over. Resuming is opt-in: Claude Code asks before it
resumes a long or long-idle conversation, which nobody can answer in a
background session, and reloading one costs usage.

Claude Code ends a background session after 8 hours idle. Set Mahamantri's
`bridge.keepMain` to the script with `--ensure`, and Mahamantri starts the
session again whenever none has been attached for two minutes.

The main session does real work unattended, so the script starts it with
`--dangerously-skip-permissions`. Otherwise a prompt leaves a task blocked
until someone answers it. Claude Code asks you to accept that mode's
disclaimer once, interactively: `claude --dangerously-skip-permissions`, then
`/exit`.

The session comments on Linear with `samrajya_reply`, which Mahamantri posts
as Senapati with its own API key (`linear.apiKey`). The script disallows the
Linear connector's comment tools, so the connector can be signed in as you:
comments from your other Claude sessions are then yours, and only Senapati's
are Senapati's.

When a turn ends on the usage limit, the plugin's `StopFailure` hook tells
Mahamantri, which shows it on Linear and wakes the session once the limit
resets.

Remote Control requires Claude Code to talk to `api.anthropic.com` directly.
Don't set `ANTHROPIC_BASE_URL` to a proxy for this session. Mid-turn, events
wait until the turn ends; to stop or steer a turn at once, use Remote Control.

## In agents mode

When mahamantri runs with `agents.enabled`, it launches the sessions itself
(see mahamantri's README). Each session's environment names its agent and
secret file, and the plugin attaches as that agent and offers that role's
tools:

- the main gets `samrajya_dispatch`, `samrajya_agents`, `samrajya_message`
  and `samrajya_stop_agent`;
- a worker gets `samrajya_report` and messages only the main.

A session without an agent id stays read-only: no session attaches by
directory, and nothing takes over. A session that was replaced stops acting.
The plugin's `UserPromptSubmit` and `Stop` hooks report each agent's turns, so
mahamantri can tell a long step from a stuck or idle session.

## In opencode mode

In `mode: opencode`, Mahamantri prompts the opencode main session with each
ping itself, so no channel is involved. The session still needs the bridge
tools, so add the bundled server to opencode's MCP configuration (for example
`opencode.json`) and mark it as the main session's:

```json
{
  "mcp": {
    "samrajya": {
      "type": "local",
      "command": ["node", "/absolute/path/to/samrajya/integrations/claude-channel/plugin/server.mjs"],
      "environment": { "SAMRAJYA_MAIN": "1" }
    }
  }
}
```

`SAMRAJYA_MAIN=1` lets it attach as the main session wherever opencode runs it.
Configure it only for the opencode server that hosts the main session.

## Development

```sh
pnpm --dir integrations/claude-channel typecheck
pnpm --dir integrations/claude-channel test      # builds, bundles and runs the tests
```

Tests run the channel against a fake bridge that follows Mahamantri's
attachment rules, and run the bundled server as a real stdio child process.
The Go side is tested in `mahamantri/bridge`.
