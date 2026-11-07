# Surfaces: Samrajya is not a kanban board

Status: proposed. It builds on [agents.md](agents.md): the inbox, tasks,
agents and supervisor stay, and the parts tied to Linear are removed.

## Why

Agents mode made parallel work structural, but it kept one assumption from
the start of the project: a piece of work *is* a Linear issue.

- Workers are created per Linear issue (`route`).
- `samrajya_reply` can only post on Linear, and `MayReply` checks issues.
- The "still waiting" notes and the status are Linear calls in the server.
- The Linear webhook is the only way anything enters.

Anything else (a chat with the person, a call, a meeting, something said in
the terminal) has nowhere to go but the main session, so the bottleneck
comes back. There are also two ways to run the main session (single-session
mode and agents mode), and that duplication is a large part of why the
system is hard to follow.

Samrajya should be a few guarantees and tools Claude works with, not an
architecture Claude must fit into.

## Model

```text
 surfaces (adapters)              core                                    agents
 ───────────────────              ────                                    ──────
 Linear webhook  ──┐                                                 ┌─ main   (talks to the person)
 Telegram poller ──┼─► Receive ─► inbox + router ─► per-agent feed ──┼─ worker (one per task)
 (later: voice,    │                    │                            └─ worker …
  meetings, Slack) │                    ▼
 Deliver/Notify ◄──┴── reply (journaled) ◄── agent tools: accept, reply, dispatch, update, report …
                                       │
                           supervisor: launch, fence, replace, stop, cap, limits, stuck
```

### Conversation, not issue

Every message from any surface is an **event** in a **conversation**:

| field          | Linear                        | Telegram                     |
|----------------|-------------------------------|------------------------------|
| `conversation` | `linear:<issue uuid>`         | `telegram:<chat id>`         |
| `title`        | `ENG-1: Fix login`            | the chat's title or name     |
| `thread`       | the comment thread's root id  | the message to reply to      |
| `author`, `authorId`, `trusted`, `body`, `url` | as today | the sender |

`trusted` is decided by the adapter from its own allow-list (Linear user ids,
Telegram user ids), not by the core.

### Work, not tickets

A **task** is Samrajya's own record of a piece of work. It may come from a
Linear issue, a Telegram message, the terminal, or nothing at all. It has:

- `conversations`: conversations **routed** to it. Their new messages go to
  its agent.
- `reportTo`: conversations its agent may **reply in** without taking all
  of their messages. A worker dispatched from the person's Telegram chat
  reports there, but the chat keeps going to the main.
- `links`: free-form references (an issue identifier, a PR URL) for people
  and agents to follow.

Linear is one place a task can live, not where tasks come from.

### Claude decides; code guarantees

**Code guarantees**, whatever any prompt says:

1. Nothing is lost. An event is stored with an owner before it is
   acknowledged to its surface, and stays pending until its owner accepts it.
2. Follow-ups go where the work is. A conversation routed to a task goes to
   that task's agent, reopening a finished task rather than duplicating it.
3. Parallel work runs in parallel. Dispatched work gets a worker of its own,
   started by the supervisor, up to `maxWorkers` at once.
4. The main session stays free. It has no Edit, Write or NotebookEdit, so
   long work can only be dispatched.
5. Sessions are supervised, the main included. A stuck main session is
   reported to the person on their home surface.
6. Usage limits are handled: nothing launches while limited, and the person
   is told why their message waits.

**Claude decides** what a message means: answer it, start new work, or add
it to existing work. It decides through tools (`samrajya_dispatch`,
`samrajya_accept`), not through rules hard-coded per surface.

**Per-surface defaults** cover the common case without asking Claude. Each
adapter declares what a trusted message in a conversation with no task does:

- Linear (`newWork: worker`): a mention on an issue nobody works on starts a
  worker for it at once. An issue is already a unit of work.
- Telegram (`newWork: main`): the chat is one long conversation with the
  person, so it goes to the main, which answers or dispatches.

This is configuration (`linear.newWork`, `telegram.newWork`), not core code.

### Surface adapters

An adapter is the only code that knows its surface:

```go
// Surface is one place messages come from and replies go to.
type Surface interface {
    Name() string                            // "linear", "telegram"
    // Deliver posts body in conv, in thread if given.
    Deliver(ctx, conv, thread, body string) (Posted, error)
    // Received marks an incoming event as seen (Linear: 👀; Telegram: 👀 reaction).
    Received(ctx, e Event) error
    // Notify tells an event's author something about it (the "still waiting"
    // notes): Linear replies in the thread with an @-mention, Telegram
    // replies to the message.
    Notify(ctx, e Event, text string) error
}
```

Adapters push events in with `Server.Receive(Inbound)`. Optional extras
stay with the adapter. For example, Linear also shows Senapati's status,
which the server asks for through a small optional interface.

The core never imports `linear` or `telegram`. Adding voice or meetings
later means adding an adapter, plus a launcher if it needs a different kind
of agent.

### One way to run it

- Agents mode is the only mode. The single-session path, `keepMain`,
  `restart-main.sh`, takeover and directory-based attachment are deleted.
  Every session, the main included, attaches with its agent id and a
  per-launch secret.
- `mode: claude` (the default) runs the supervisor: a main and its workers
  as Claude Code background sessions.
- `mode: opencode` runs only a main, on an opencode server. Mahamantri keeps
  a fixed secret for it, and there are no workers. Dispatch says workers need
  `mode: claude`. This keeps opencode working without a second launcher.
- Restarting everything is `systemctl --user restart samrajya-mahamantri`,
  or `scripts/samrajya restart`. Switching an existing setup over is
  `scripts/samrajya cutover`, and `scripts/samrajya rollback` undoes it.

## Tools (plugin protocol 3)

| tool | who | change |
|---|---|---|
| `samrajya_reply` | all | `conversation`, `thread_id`, `body`. A worker may reply only in its task's conversations and `reportTo`. |
| `samrajya_dispatch` | main | `title`, `brief`, `conversation`, `thread_id`, `route` (the conversation's new messages go to the worker; default: what the surface's `newWork` says), `links`, `project`. |
| `samrajya_create_task` | main | `links` instead of `issue`. |
| `samrajya_take_over` | — | removed. |

The rest are unchanged: accept, update, tasks, status, action intent/result,
memory, agents, message, stop, report.

## Migration

- The store stays at schema 2; the new fields are optional. Old Linear events
  get `conversation` from their issue UUID. `Task.Issue` and
  `Event.Issue*` are read as links.
- Config: `bridge.trustedUserIDs` becomes `linear.trustedUserIDs`.
  `bridge.keepMain` and `agents.enabled` are refused with a message saying
  what replaces them. `telegram:` is new and optional.
- The old plugin (protocol 2) is refused with "update the plugin".

## Acceptance

Go tests, with fake surfaces, a fake launcher and a fake clock:

1. A Linear mention on a new issue starts a worker; a Telegram message goes
   to the main. Both are stored before the surface is acknowledged.
2. The main dispatches from a Telegram message with `route: false`. The
   worker replies in that chat. The chat's next message still goes to the
   main.
3. With `route: true`, the next message goes to the worker.
4. A worker cannot reply in a conversation that is not its own (403).
5. The "still waiting" notes go through `Notify` on the event's own surface.
6. A main stuck past `stuckAfter` is reported on the home conversation.
7. All earlier agents tests, minus the legacy-mode ones.

Live in the sandbox (real Claude Code sessions, fake Linear and fake
Telegram APIs):

A. Two Linear issues and a Telegram "do X" at once: two workers reply on
   Linear, and the main dispatches X and answers on Telegram within a minute.
B. A Telegram follow-up during the work reaches the main; the main relays it
   to the worker with `samrajya_message`.
C. Restarting Mahamantri mid-work: everything reattaches, nothing is
   duplicated, and the Telegram offset is not replayed.
