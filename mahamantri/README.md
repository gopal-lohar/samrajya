# mahamantri

Samrajya's core. It takes messages from every surface into a durable inbox
and routes each to the session that owns its work. It runs those sessions
and supervises them, and it serves them a local, authenticated API for their
tasks, replies, journal and memory. The design is in
[`../docs/design/agents.md`](../docs/design/agents.md) and
[`../docs/design/surfaces.md`](../docs/design/surfaces.md).

## What it guarantees

- **Nothing is lost.** A message is stored, with its owner, before its surface
  is told it arrived. If storing fails, the surface retries. Redeliveries are
  kept once. A message stays pending until its owner accepts it.
- **Follow-ups go where the work is.** A conversation routed to a task goes to
  that task's worker. A reply to something a worker posted goes to that worker.
- **Parallel work runs in parallel.** Each piece of work gets a worker session
  of its own, up to `agents.maxWorkers` at once.
- **The main session stays reachable.** It has no Edit or Write, so it
  dispatches long work instead of doing it.
- **Sessions are supervised.** Lost sessions are replaced, and failed launches
  are retried, then reported. A session that stops listening is relaunched
  with what waits for it. A main session stuck in one step is reported to you
  in your `home` conversation.
- **Usage limits are handled.** Nothing is launched while limited. The people
  waiting are told why, and the work resumes when the limit resets.
- **External actions are journaled.** Every reply is recorded before it is
  posted. One never confirmed is reported as uncertain, to be checked before
  it is retried.

All of it lives in one bbolt file (`bridge.store`), locked so that a second
mahamantri cannot open it.

## Surfaces

A surface is where messages come from and replies go. Each message belongs to
a *conversation* (`linear:<issue>`, `telegram:<chat>`), and each surface has a
policy for new ones:

- **Linear** (`linear:`; on when `signingSecret` is set). A ping is a comment
  that @-mentions Senapati (the bot account) or a reply in a thread it wrote
  in.
  - A trusted person's ping on an issue nobody works on starts a worker for
    it (`startsWork`, default on). An issue is a piece of work.
  - Pings get a 👀, and Senapati's Linear status shows problems.
  - A ping left waiting `nudgeAfter` (15m) is answered with why.
  - Replies are posted as Senapati, whoever the session's connector belongs
    to.
- **Telegram** (`telegram:`; on when `botToken` is set). A bot you write to.
  - Messages are polled, so no public webhook is needed. Each is stored
    together with the read position, so a restart neither loses nor repeats
    one.
  - Only `trustedUserIDs` are heard, in private chats, plus any `chatIDs`
    groups.
  - A chat goes to the main session (`startsWork`, default off), which
    answers or dispatches. The worker reports in the chat, and your replies
    to its messages reach it.
  - Voice notes, photos and documents arrive as attachments a session can
    fetch.
  - Messages waiting `nudgeAfter` (2m) are answered with why.
  - Use a bot of its own for each mahamantri: two programs polling one bot
    steal each other's messages.
  - Telegram keeps undelivered messages for 24 hours. Messages sent while
    mahamantri was down longer than that are lost.

`home:` names the conversation where Samrajya tells you about itself, for
example `telegram:<your chat id>`.

A new surface is an adapter implementing `bridge.Surface` (see
`bridge/surface.go`, and `linear/surface.go` and `telegram/` for examples),
registered in `main.go` with its policy.

## Sessions

`mode: claude` (the default) runs Claude Code background sessions in
`bridge.mainDirectory`, named `<agents.namePrefix>-<agent>-g<generation>`:

- **The main** talks to you through Remote Control (`agents.remoteControl`)
  and keeps the overview. Its role is
  [`../senapati/main.md`](../senapati/main.md).
- **A worker per task**, with role
  [`../senapati/worker.md`](../senapati/worker.md). It acts only on its own
  task and replies only in its task's conversations; the bridge enforces this.
- `agents.mainArgs` and `agents.workerArgs` are added to the launches, for
  example `["--model", "sonnet"]`.

**How messages reach a session.** Each session's plugin attaches it as its
agent, with a per-launch secret: a replaced session can no longer act. Its
first work comes as its launch prompt. Between turns, a waiter hook (Claude
Code's `asyncRewake`) waits for new messages and wakes the session with
them. Claude Code's channels are not used: an account's policy can switch
them off.

`mode: opencode` runs one main session on an `opencode serve` server, prompted
with each message, with no workers. Its plugin attaches as `main` with the
secret in `<agents.stateDir>/main.secret` (see the plugin's README). In this
mode the main does the work itself, so the parallelism guarantees above do not
apply.

## Run

```sh
cp mahamantri.yaml.example mahamantri.yaml   # fill in your surfaces
go build -o mahamantri . && ./mahamantri check mahamantri.yaml
./mahamantri mahamantri.yaml
```

Run it as a systemd user service, then use
[`../scripts/samrajya`](../scripts/samrajya):

- `status`: the service, the sessions and what waits;
- `restart`: sessions reattach by themselves;
- `restart-main`: a new main session;
- `update`: test, build, install the plugin and restart;
- `cutover` and `rollback`: switch a setup from before surfaces to this
  version, and undo it.

Stopping mahamantri never stops the sessions. They reattach when it is back,
and messages that arrive meanwhile are redelivered by their surface.

On each start mahamantri writes the bridge's address and a fresh token to
`bridge.discoveryFile` (default `$XDG_STATE_HOME/samrajya/bridge.json`),
readable only by you.

## The workspace

`workspace:` is the directory that holds the projects, one per directory. How
each project is worked on belongs in that project's own instruction files, not
in Samrajya. This covers worktrees, dependencies, services and branches.

## Memory (the brain)

`brain:` is a directory of short markdown notes, one per fact worth knowing next
time: a person's standing instructions, facts in neither the code nor the
conversations, problems that keep coming back. `INDEX.md` lists them. Sessions
read and write them through the bridge (`samrajya_memory`, `samrajya_remember`,
`samrajya_forget`), and they are plain files you can edit too. It is kept small
on purpose: notes under 1,500 bytes, at most 50.
