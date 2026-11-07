# Agents: many sessions working at once

Status: built; agents mode is now the only mode. [surfaces.md](surfaces.md)
replaces three parts of this design:
- routing is by conversation on any surface, not by Linear issue;
- sessions are woken by a waiter hook, not channels;
- the single-session mode and `agents.enabled` are gone.

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
impossible: every ask is stored and owned before it is acknowledged, replies
are journaled once, and agents are supervised on a server.

## Principles

1. **Parallelism is decided by code.** Which session handles what is a
   routing decision Mahamantri makes and records. Prompts say how to do the
   work, never whether work may run in parallel.
2. **The person can always reach the assistant.** The main session cannot
   do long work itself (its editing tools are withheld); it dispatches.
   When the account is at its usage limit no session can answer, and
   Mahamantri's own notes on Linear are the only voice: it says so.
3. **One owner per piece of work, recorded at once.** A conversation
   belongs to at most one task, keyed by stable ids (a Linear issue's UUID,
   not its identifier), and every event gets exactly one owning agent in the
   same transaction that stores it. Nothing depends on a session remembering
   what it was given.
4. **Enforced where we can, honest where we can't.** The server checks
   every Samrajya tool call against the caller's agent: a worker can only act
   on its own task and reply in conversations linked to it. It cannot police
   what a session does through other tools (git, the Linear connector), so
   workers don't get the connector's write tools, and the rest is in their
   role.
5. **Surfaces and agents are plug-ins to one core.** Linear is the first
   surface and Claude Code the first agent runtime; neither is special in the
   core.
6. **Everything survives restarts**, and nothing is ever started twice.

## Model

```text
 surfaces                       Mahamantri core                        agents
 ────────                       ───────────────                        ──────
 Linear webhook ──┐                                               ┌─ main   (talks to the person)
 (later: chat,    ├─► inbox+router ─► per-agent feeds ─► plugin ──┼─ worker (one per task)
  voice, meeting) │        │                                      └─ worker …
                  │        ▼
  replies ◄───────┴── journal, tasks, agents ◄── agent tools (accept, update, reply, report…)
                           │
                supervisor: launch / fence / replace / stop, cap, limits
```

- **Event**: something that arrived, from a surface or from Samrajya. It has
  a *conversation key* (`linear:<issue uuid>`; later `chat:<id>` …), the
  thread to reply in, and its owning agent.
- **Task**: a unit of work with an owning agent, status, next action,
  checkpoint, result, worktree, and the conversations linked to it (many
  conversations to one task: an issue today; a chat thread or PR later).
- **Agent**: a session Mahamantri runs: `id`, `role` (`main`, `worker`),
  its task, a *generation*, its runtime session id, and its state:
  `wanted` → `starting` → `live` → `stopped` (or `failed`). Separately: is it
  in a turn, since when, and when it last ended one.
- **Router**, inside the ingest transaction:
  1. The conversation is linked to a task → that task's owner. A finished
     task is reopened, not duplicated.
  2. A Linear issue with no task, from a trusted author → a new task and a
     new worker for it.
  3. Anything else (untrusted authors, no issue, Samrajya notices for the
     main) → the main.
  The main can start a worker for anything (`samrajya_dispatch`), and the
  same index makes a dispatch and a ping arriving together land on one task.
- **Supervisor**, a loop in Mahamantri:
  - *Launch*: an agent is recorded `starting` with a new generation before
    its session is started under a unique name (`samrajya-<agent>-g<gen>`).
    It must attach within a few minutes; otherwise it is stopped and retried
    with backoff, and after three failures its task is blocked and the
    person is told. A broken setup (plugin, login, allowlist) therefore
    costs three tries, not a loop.
  - *Reconcile*: at start and every minute, running sessions (`claude agents`)
    are matched to agents by name and generation: matches are adopted,
    anything else named `samrajya-*` is stopped.
  - *Replace* (fence first): bump the generation, so the server refuses the
    old session's calls; stop the old session and confirm its process is
    gone; only then launch.
  - *Has work*: a worker is (re)started only when its task has events it
    has not accepted, or its task is `queued` or `working`. A task waiting
    for approval, blocked, paused or in review waits for a new event.
  - *Idle*: a worker with no work and no turn for 30 minutes is stopped.
    Workers do not last; their context is the task's checkpoint, the branch
    and the Linear thread, and the next worker starts from those.
  - *Stuck*: a turn running for over 90 minutes is reported to the main and
    shown in `samrajya_agents`; nothing is killed automatically.
  - *Cap*: at most `maxWorkers` (default 2) run at once; the rest wait in
    order, and their authors are told they are queued and behind how many.
  - *Main*: kept running, as `keepMain` does today.
- **Identity**: the launcher gives each session its agent id and the path of
  a secret file (0600) through `--settings` env, which reaches the plugin
  process. The plugin attaches with id, generation and secret. With agents
  on, nothing attaches by directory: a session without a valid secret is
  read-only. The plugin sends its protocol version; an incompatible one is
  refused, and the supervisor recycles idle sessions on old versions.

## Roles

Roles are system-prompt files (`--append-system-prompt-file`), so they
outrank Claude Code's generic defaults; MCP instructions only explain the
tools.

**Main**: the person's assistant. Answers direct conversation and anything
not tied to a task; keeps the overview; dispatches work; relays to workers.
Started without Edit, Write and NotebookEdit, so it can investigate and
answer but long work can only be dispatched.

**Worker**: owns one task. Its first piece of work arrives as its launch
prompt (the task, its checkpoint and the pending events), not as a channel
event; follow-ups arrive on the channel. It acknowledges a new ask within
minutes with the plan and where results will appear, works in its own
worktree as the project's rules say and records it on the task, replies in
the task's conversations, keeps the checkpoint current, and reports when it
finishes, is blocked or needs a decision. It takes no other work.

## Tools

Existing tools keep their names and are checked against the caller.
New: for the main, `samrajya_agents`, `samrajya_dispatch`,
`samrajya_message`, `samrajya_stop_agent`; for workers, `samrajya_report`.
Reports are state shown by `samrajya_agents`; only `blocked` and
`needs_decision` reach the main as events, so finishing workers do not keep
the main in turns.

## Limits, status, nudges

The usage limit belongs to the account. A `StopFailure` from any agent sets
it, with the agent's id. While it holds, nothing is launched. It clears at
the parsed reset time, or when a tool call arrives more than a minute after
it was set (proof that turns run again). After it clears, agents with work
are resumed one at a time, main first. Senapati's Linear status shows only
problems. A waiting ping's note says the real reason: queued behind N,
starting, its worker mid-step, at the limit, or offline. A follow-up such as
"stop" waits for the worker's current step; a hard stop is
`samrajya_stop_agent` or Remote Control.

## Expansion seams

- **Surfaces**: an ingest path producing events with a conversation key, and
  a sender for replies. A chat app becomes a surface routed to the main, or
  linked to a task; a meeting a surface a worker owns for its length.
- **Agent kinds and runtimes**: roles and launchers are open sets. A voice
  agent cannot wait for another agent's turn, so task and agent state
  (checkpoint, last activity, current turn) is a read API any agent can
  query, and launchers have an interrupt operation, even where unimplemented.
- **Observers**: every event has exactly one owner; others may subscribe
  read-only later.

## Compatibility and rollout

- `agents.enabled: false` (default) is today's behaviour, with today's tests.
- A plugin session without an agent id behaves as today.
- Store schema 2 (agents, conversations, owners) upgrades schema 1 in place,
  after copying the file.
- Built and tested in a sandbox: a second Mahamantri with its own config,
  store, ports, discovery file, session names and directory, a fake Linear
  API, and real Claude Code sessions on a small model. The live setup is not
  touched.
- Cutover when the person chooses. Rollback: `agents.enabled: false`.

## Acceptance tests

In Go, with a fake launcher and clock:

1. Pings on three issues start three workers; each event reaches only its
   own worker.
2. Follow-ups on the same issue, in any thread, reach the same worker.
3. Two pings for the same new issue ingested concurrently make one task and
   one launch; so do a ping and a dispatch.
4. An issue's identifier changing keeps routing to the same task.
5. An untrusted ping on a new issue starts nothing and goes to the main.
6. With `maxWorkers: 2`, a third issue waits and starts when one frees.
7. A replaced worker's old generation is refused and stopped before the new
   launch.
8. Mahamantri dying between recording `starting` and the launch returning:
   after restart nothing is duplicated; the orphan is adopted or stopped.
9. A launch that keeps failing backs off, blocks the task and tells the
   person after three tries.
10. A long turn is not "gone"; a stuck turn is reported.
11. A worker cannot accept, update or reply for another task.
12. With agents on, a session without a valid secret is read-only, and
    never becomes the main.
13. A worker's tool call right after a limit doesn't clear it; resumes are
    staggered, main first.
14. A worker retired idle with its task in review is not restarted until a
    new event; a new event restarts it.
15. With agents off, behaviour is today's.

Live, in the sandbox:

0. A launched worker attaches with its id and receives channel events.
A. Three issues pinged together get three replies in their own threads,
   while the main answers a direct message within a minute.
B. A follow-up within the idle window reaches the same worker.
C. Killing a worker's process: it is replaced and still answers.
D. A simulated usage limit pauses launches; work resumes after it clears.
