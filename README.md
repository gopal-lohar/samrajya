# Samrajya

Opinionated Agentic AI framework to manage agentic workflows with Claude Code or opencode, and Linear.

- `mahamantri/` - the Go program that relays Linear pings (mentions of
  Senapati, replies in its threads) and sainik session status to Senapati,
  the manager agent, and gives it a restricted toolset: start, message and
  inspect sainiks, one agent session per issue that does the actual work.
  Sessions run on Claude Code (`harness: claude`) or an opencode server
  (`harness: opencode`). See its README.
- `senapati/` - Senapati's role instructions (`senapati.md`), sent to it as its
  briefing by mahamantri.

## Native mode

Instead of a manager agent routing work to sainiks, the main assistant can be an
ordinary interactive Claude Code session. It runs on your Claude subscription,
you can reach it from the terminal or your phone through Remote Control, and it
does the work itself. In this mode (`mode: native`), mahamantri keeps a durable
inbox of Linear pings, the tasks they become, a journal of external actions,
and the brain. The Samrajya channel plugin
(`integrations/claude-channel/`) feeds pings into the session and gives it tools
for that state. See the plugin's README to set it up, and mahamantri's README
for the configuration. Managed mode stays the default and keeps full Claude
Code and opencode support.
