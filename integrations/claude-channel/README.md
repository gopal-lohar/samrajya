# Samrajya plugin

The session side of Samrajya. Every session mahamantri runs (the main and
each worker) loads this plugin. It:

- **attaches the session as its agent.** Mahamantri launches each session
  with its agent id and the path of a per-launch secret file, through
  `--settings` env. A session that was replaced can no longer act, and a
  session mahamantri did not launch only reads status.
- **wakes the session when messages arrive.** Whenever the session ends a
  turn, its `Stop` hook starts a waiter in the background (Claude Code's
  `asyncRewake`). The waiter long-polls mahamantri and, when messages arrive,
  exits 2 with them, which wakes the session with them as input. Mahamantri
  hands each message over once. A session whose waiter is gone is relaunched
  by mahamantri, with the waiting messages in its launch brief.
- **gives it its tools.** All agents get:
  - `samrajya_accept`: accept a message into a task, or alone for a message
    answered on the spot;
  - `samrajya_reply`: post in a conversation, on any surface, journaled,
    or (`react`) put an emoji reaction on a message;
  - the task, journal and memory tools;
  - `samrajya_fetch`: save a file a message carried, to read it.

  The main also gets `samrajya_dispatch`, `samrajya_agents`,
  `samrajya_message` and `samrajya_stop_agent`, and workers get
  `samrajya_report`.
- **reports turns and usage limits.** `UserPromptSubmit` marks a turn's start,
  and the waiter marks its end first, so mahamantri can tell a long step from
  a stuck or idle session. `StopFailure` reports the usage limit.

Claude Code's channels are not used. An account's policy can switch them off,
and then their events are dropped without a word.

## How it connects

Mahamantri serves a local, token-authenticated API. On every start it writes
the address and a fresh token to its discovery file, by default
`$XDG_STATE_HOME/samrajya/bridge.json` (`~/.local/state/...`), readable only by
you. The plugin reads that file and rereads it whenever mahamantri restarts.
Mahamantri passes `SAMRAJYA_BRIDGE_FILE` to the sessions it launches.

## Install

Requires Node 24+ on `PATH` and pnpm.

```sh
pnpm --dir integrations/claude-channel install --frozen-lockfile --ignore-scripts
pnpm --dir integrations/claude-channel bundle     # builds plugin/server.mjs
claude plugin marketplace add /absolute/path/to/samrajya/integrations
cd /path/to/your/workspace   # mahamantri's bridge.mainDirectory, where sessions run
claude plugin install samrajya@samrajya --scope local
```

Claude Code copies the plugin when installing it, keyed by version. After a
change, bump the version in `plugin/.claude-plugin/plugin.json`.
`scripts/samrajya update` then installs it for the main directory.
`claude plugin update --scope local` can update another directory's entry
instead of the one it runs in; the script sets the entries itself.

## In opencode mode

In `mode: opencode`, mahamantri prompts the opencode main session with each
message itself. The session still needs the tools, so add the bundled server
to opencode's MCP configuration (for example `opencode.json`), as the main
agent:

```json
{
  "mcp": {
    "samrajya": {
      "type": "local",
      "command": ["node", "/absolute/path/to/samrajya/integrations/claude-channel/plugin/server.mjs"],
      "environment": {
        "SAMRAJYA_AGENT": "main",
        "SAMRAJYA_AGENT_SECRET_FILE": "/home/you/.local/state/samrajya/agents/main.secret"
      }
    }
  }
}
```

Mahamantri writes that secret file on start (in `agents.stateDir`), and the
secret does not change between restarts.

## Development

```sh
pnpm --dir integrations/claude-channel typecheck
pnpm --dir integrations/claude-channel test      # builds, bundles and runs the tests
```

The tests run the plugin against a fake bridge, and run the bundled server as
a real stdio child process and as a hook. The Go side is tested in
`mahamantri/bridge`.
