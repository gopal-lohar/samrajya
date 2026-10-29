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

Start Mahamantri first, then the session. Run it from your
workspace directory so project instructions apply:

```sh
claude --bg -n samrajya --channels plugin:samrajya@samrajya --remote-control Samrajya
```

To restart it, for example after changing its settings or your login, use
`integrations/claude-channel/restart-main.sh`. It stops every session with
that name in the main directory by id, then starts a new one there. Only one
session holds the main attachment, so a leftover session keeps receiving pings
while the new one stays passive. `claude stop <name>` doesn't reliably find a
session when several share the name.

The main session does real work unattended, so allow the tools it needs in the
workspace's local settings (`.claude/settings.local.json`); otherwise auto mode
may refuse, say, posting a Linear reply and leave the task blocked until you
approve it:

```json
{ "permissions": { "allow": ["mcp__plugin_samrajya_samrajya", "mcp__linear"] } }
```

Remote Control requires Claude Code to talk to `api.anthropic.com` directly.
Don't set `ANTHROPIC_BASE_URL` to a proxy for this session. Mid-turn, events
wait until the turn ends; to stop or steer a turn at once, use Remote Control.

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
