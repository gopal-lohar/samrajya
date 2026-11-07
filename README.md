# Samrajya

An assistant that works for you on several things at once, built on Claude
Code (or opencode).

You talk to its main session in the terminal, through Remote Control, or on
any of Samrajya's surfaces: Linear today, Telegram, and whatever is added
next. The main session answers and keeps the overview. Work that takes time
goes to worker sessions that run in parallel, each owning one piece of work
and reporting where it was asked. Nothing has to be a ticket. A question in a
chat is answered, and a request from anywhere becomes a piece of work.

- `mahamantri/`: Samrajya's core, in Go. It takes messages from every surface
  into a durable inbox and routes each to the session that owns its work. It
  runs and supervises the sessions, and keeps the tasks, the journal of
  external actions and the brain (Samrajya's memory). See its README.
- `integrations/claude-channel/`: the Samrajya plugin each session loads. It
  attaches the session as its agent, wakes it when messages arrive, and gives
  it its tools.
- `senapati/`: the roles (`main.md`, `worker.md`; `senapati.md` for opencode).
- `scripts/samrajya`: one command to run it (status, restart, update, cutover,
  rollback).
- `docs/design/`: how it works and why ([agents](docs/design/agents.md),
  [surfaces](docs/design/surfaces.md)).
