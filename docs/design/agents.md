# Agents: many sessions working at once

Status: proposal, for review before any code.

## Why

In `mode: claude` there is exactly one Claude Code session behind Samrajya.
Mahamantri lets one session attach, delivers every ping to it, and that
session works one turn at a time. Whether two issues progress at once was
left to the session's own judgement ("give independent work to background
agents"), and Claude Code's built-in prompts steer it the other way: its
agent tool says not to spawn agents unless the user asks, and every channel
event arrives marked "not from your user, decide after your current task".
In practice the session worked through issues strictly one after another.

The managed mode this replaced was parallel by construction: the manager's
code started a worker per issue. The native migration kept the durable
inbox, journal and recovery, and dropped that property. This design brings
it back as structure, in Mahamantri's code, not as advice in a prompt.

It also has to carry what comes next without another redesign: more
surfaces than Linear (a chat app the person writes to all day, voice calls,
meetings an agent joins), more kinds of agents, agents that outlive a
conversation.

## What a capable assistant looks like from outside

We studied a comparable assistant that a team uses all day: a single
long-running Claude session with computer use, no special program around it.
What makes it feel powerful, and where it fails:

- Each request is its own unit of work: an acknowledgement within a minute
  that restates the plan and says where results will appear, then evidence
  (PR, commit, test counts, screenshots, how to test), then "done". Its
  coordinator stays free to answer while its own agents run eight or more
  pieces of work at once.
- Approval lines are explicit and repeated: routine environments on its own
  judgement; production, infrastructure and prompt changes only with a named
  person's OK, often a one-word "go".
- It keeps durable rules and a tracker of every open ask.
- Its failures are what we must not copy: asks it silently misses (its most
  common complaint), duplicate or stray messages, reports left in local
  files nobody can open, background agents doing damage without guards, and
  outages whenever the machine it runs on sleeps.

Our design keeps its strengths and makes its failure modes structurally
impossible: every ask is stored and owned before it is acknowledged, a
sweep finds any ask that is neither answered nor owned, replies are
journaled once, and agents are supervised on a server.

## Principles

1. **Parallelism is decided by code.** Which session handles what is a
   routing decision Mahamantri makes and records. Prompts say how to do the
   work, never whether work may run in parallel.
2. **The person can always reach the assistant.** The main session never
   does long work itself; it stays free to talk.
3. **One owner per piece of work, recorded.** Every task has exactly one
   agent that owns it at a time, and every event goes to exactly one agent.
   Nothing depends on a session remembering what it was given.
4. **Enforced, not requested.** An agent can only act on its own tasks and
   reply where its work is. Mistakes such as replying in another issue's
   thread become impossible rather than discouraged.
5. **Surfaces and agents are plug-ins to one core.** Linear is the first
   surface and Claude Code the first agent runtime; neither is special in the
   core.
6. **Everything survives restarts.** Mahamantri, any session, or the machine
   can restart without losing events, tasks or who owns what.

## Model

```text
 surfaces                       Mahamantri core                        agents
 ────────                       ───────────────                        ──────
 Linear webhook ──┐                                               ┌─ main   (talks to the person)
 (later: chat,    ├─► inbox ─► router ─► per-agent queues ─► feed ┼─ worker (one per task)
  voice, meeting) │     │         │                               └─ worker …
                  │     ▼         ▼
  replies ◄───────┴── journal   tasks ◄─ agent tools (accept, update, reply, report…)
                                  │
                       supervisor: start / stop / replace agents, cap, limits
```

- **Event**: something that arrived, from a surface or from Samrajya
  itself. It carries a *conversation key* that says where it belongs and
  where a reply goes: for Linear, the issue (`linear:<issue id>`) and the
  thread. Later surfaces add their own keys (`chat:<chat id>`, …).
- **Task**: a unit of work with an owner (`agent`), status, next action,
  checkpoint and result, and the conversation(s) it serves.
- **Agent**: a session Mahamantri knows: `id`, `role` (`main` or
  `worker`), the task a worker serves, its runtime session id, state
  (`starting`, `live`, `gone`, `stopped`), and its attachment.
- **Router**: gives every new event to one agent, in code, in one store
  transaction:
  1. The conversation already belongs to an open task → that task's owner.
  2. A Linear issue with no open task → a new task and a new worker for it.
     A finished task on that issue is reopened instead of starting a second.
  3. Anything else (no issue; a direct message; Samrajya notices for the
     main) → the main.
  The main can move a task to another agent or to itself, and start a worker
  for work that came from conversation (`samrajya_dispatch`).
- **Supervisor**: makes the agents the router assigned exist. It starts a
  worker through a *launcher* (first: `claude --bg`), keeps at most
  `maxWorkers` running and queues the rest in order, notices a worker that
  has gone (no feed for the attachment timeout) and replaces it from its
  task's checkpoint when it has work, stops workers that are idle with no
  open work, keeps the main running, and pauses starting anything while the
  account is at its usage limit.
- **Attachments, one per agent.** Each session's plugin attaches as its
  agent id, which the launcher gives it. The single main attachment becomes
  a table. A session without an agent id (someone's own Claude Code session
  with the plugin loaded) stays read-only, as today.

## What each role does

**Main**: the person's assistant. It answers direct conversation and
anything not tied to a task, keeps the overview ("what's running?"),
dispatches work it is asked to do that will take a while, and passes
messages to workers. It does short things itself and nothing long. Its role
is in the system prompt (`--append-system-prompt-file`), so it outranks
Claude Code's generic defaults, not in MCP instructions.

**Worker**: owns one task. It gets that task's pings and nothing else,
works in its own worktree as the project's rules say, replies in the task's
threads, keeps its checkpoint current, and reports to the main when it
finishes or is blocked. It stays reachable for follow-ups on its task; it
does not take other work.

## Agent tools (plugin)

Existing tools keep their names. Changes:

- Every call carries the caller's attachment; the server checks the caller
  owns the task, event or thread it touches (main may act on anything).
- `samrajya_reply` is accepted only on the caller's task's issue.
- New for the main: `samrajya_agents` (who is running what),
  `samrajya_dispatch` (start a worker for a task or new request),
  `samrajya_assign` (move a task), `samrajya_message` (send text to an
  agent; it arrives as an event in that agent's feed), `samrajya_stop_agent`.
- New for workers: `samrajya_report` (a short note to the main: finished,
  blocked, needs a decision). Finishing a task notifies the main
  automatically.

## Limits, status, nudges

The usage limit belongs to the account, so it is one state: any session's
`StopFailure` sets it, the supervisor starts nothing while it holds, and
when it resets every agent with open work gets a resume notice. Senapati's
Linear status still shows only problems (offline, limit, blocked). A ping
waiting because `maxWorkers` is reached gets one note saying it is queued
and behind how many; a ping to a live worker is answered by that worker.

## No ask is missed

Every event is owned by an agent the moment it is stored. A sweep checks
that each one is acted on: an event its agent has not accepted within a few
minutes is re-sent to that agent with a reminder; one still unhandled later
is reported to the main and the person is told why it waits. A worker's
first reply to a new ask restates the plan and says where results will
appear, so the person sees within minutes that the right agent has it.

## Expansion seams

- **Surfaces**: a surface is an ingest path producing events with a
  conversation key, and a sender for replies to that key. A chat app the
  person uses becomes a surface whose conversation routes to the main; a
  meeting becomes a surface a worker owns for the meeting's length.
- **Agent kinds and runtimes**: roles and launchers are open sets. A voice
  agent that needs real-time audio is a different runtime with its own
  launcher speaking the same API; opencode can be a launcher for workers.
- **Placement**: the API is HTTP with a token; a launcher could start
  agents elsewhere later.

None of these is built now. The point is that adding them adds a surface, a
role or a launcher, and does not change routing, ownership or recovery.

## Compatibility and rollout

- `agents.workers: false` (default) routes everything to the main exactly
  as today, and the existing tests keep passing. `mode: opencode` keeps
  working with workers off.
- A plugin session without an agent id behaves exactly as today, so an
  updated plugin is safe with the current main session and vice versa.
- Store schema 2 adds agents and per-event/task owners, upgraded in place
  from 1 on first open, with a backup first.
- Built and tested in a sandbox: a second Mahamantri with its own config,
  store, ports, discovery file, session names and directory, a fake Linear
  API, and real Claude Code sessions. The live setup is not touched.
- Cutover at a time the person chooses: restart Mahamantri, start the main
  afresh. Pings in between wait in the inbox. Rollback: workers off.

## Acceptance tests

Agreed before code; the work is done when these pass.

In Go, with a fake launcher and clock:

1. Pings on three issues at once start three workers; each event reaches
   only its own worker.
2. A follow-up on the same issue, in any thread, reaches the same worker
   and starts nothing.
3. With `maxWorkers: 2`, a third issue waits, its author is told once that
   it is queued, and it starts when a worker frees.
4. A worker that disappears is replaced when it has work; the replacement
   gets its pending events and the task's checkpoint; no duplicate worker.
5. A worker cannot accept, update or reply for another task or issue.
6. Non-issue events and finished/blocked reports reach the main; a message
   from the main reaches the worker.
7. At the usage limit nothing starts; after it resets each agent with open
   work is told to resume.
8. With workers off, behaviour is today's.
9. After Mahamantri restarts, running sessions reattach as the same agents
   and nothing is started twice.
10. A worker idle with no open work is stopped; a new ping on its issue
    reopens the task and starts a worker.
11. An event its agent leaves unaccepted is re-sent with a reminder, then
    reported to the main; none is ever left without an owner.

Live, in the sandbox, with real Claude Code sessions:

A. Three synthetic issues pinged together get three correct replies in
   their own threads, while the main answers a direct question at once.
B. A follow-up reaches the same worker, which still has the context.
C. Killing a worker's process: it is replaced and still answers.
D. A simulated usage limit pauses starting, and work resumes after reset.
