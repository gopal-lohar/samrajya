# Samrajya channel plugin

The Claude Code side of Mahamantri's **native mode**. Your main assistant is an
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

Mahamantri (`mode: native`) serves a local, token-authenticated API. On every
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
claude plugin install samrajya@samrajya
```

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

Start Mahamantri in native mode first, then the session. Run it from your
workspace directory so project instructions apply:

```sh
claude --bg -n samrajya --channels plugin:samrajya@samrajya --remote-control Samrajya
```

Remote Control requires Claude Code to talk to `api.anthropic.com` directly.
Don't set `ANTHROPIC_BASE_URL` to a proxy for this session. Mid-turn, events
wait until the turn ends; to stop or steer a turn at once, use Remote Control.

## Development

```sh
pnpm --dir integrations/claude-channel typecheck
pnpm --dir integrations/claude-channel test      # builds, bundles and runs the tests
```

Tests run the channel against a fake bridge that follows Mahamantri's
attachment rules, and run the bundled server as a real stdio child process.
The Go side is tested in `mahamantri/bridge`.
