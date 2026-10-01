# Samrajya

Opinionated Agentic AI framework to manage agentic workflows with Opencode and Linear.

- `mahamantri/` - the Go program that relays Linear pings (mentions of
  Senapati, replies in its threads) and sainik session status to Senapati,
  the manager agent, and gives it a restricted toolset: start, message and
  inspect sainiks, one opencode session per issue that does the actual work.
  See its README.
- `senapati/` - Senapati's role instructions (`senapati.md`), sent to it as its
  briefing by mahamantri.
