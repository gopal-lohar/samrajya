# Samrajya

Opinionated Agentic AI framework to manage agentic workflows with Claude Code or opencode, and Linear.

Samrajya's main assistant is one hands-on agent session. It does the work
itself and delegates to background agents only when that is faster. You talk
to it directly or through Linear.

- `mahamantri/`: the Go bridge. It holds a durable inbox of Linear pings (a
  mention of Senapati, the bot account, or a reply in its thread), the tasks
  they become, a journal of external actions, and the brain, Samrajya's memory
  of the work. It serves them to the main session over a local API. The session
  runs on Claude Code (`mode: claude`, on your Claude subscription and reachable
  through Remote Control) or on an opencode server (`mode: opencode`). See its
  README.
- `integrations/claude-channel/`: the Samrajya plugin. It pushes pings into a
  Claude Code session as channel events and gives either kind of session its
  bridge tools.
- `senapati/senapati.md`: the main session's role.
