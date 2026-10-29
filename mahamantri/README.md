# mahamantri

The bridge between Linear and Samrajya's main assistant: one hands-on agent
session that does the work itself. Mahamantri keeps what must outlive any one
session and serves it to the session over a local, authenticated API:

- **Durable Linear ingress.** A ping is a comment that @-mentions Senapati
  (the bot account) or a reply in a thread Senapati wrote in. It is stored
  before Linear's webhook is acknowledged. If storing fails, the webhook
  answers 503 and Linear redelivers. Redeliveries are kept once. Issue edits,
  integrations' comments and Senapati's own comments are not pings.
- **Tasks.** Pings stay pending until the session accepts them into a task,
  and every new connection replays them, so a restart neither loses nor
  duplicates work. A task is done only with a result.
- **Action journal.** Significant external actions, such as Linear replies, are
  recorded before they are attempted. One never resolved is reported as
  uncertain, to be checked before it is retried.
- **The brain:** Samrajya's memory of the work (see below).

All of this lives in one bbolt file (`bridge.store`), locked so a second
mahamantri cannot open it.

## Modes

`mode:` picks what the main session runs on:

- **`claude`** (the default) is an interactive Claude Code session that you
  start, on your Claude subscription, reachable from your phone through Remote
  Control. The Samrajya channel plugin
  ([`../integrations/claude-channel`](../integrations/claude-channel)) pulls
  pings from the bridge into it and gives it the bridge tools.
- **`opencode`** is a session on an `opencode serve` server. Mahamantri creates
  it once and remembers it, installs the main role
  (`opencode.instructionsFile`, default `../senapati/senapati.md`) as its
  instructions, and prompts it with each ping, queued behind its current turn.
  It reaches the bridge through the same plugin's MCP server, configured in
  opencode (see the plugin's README).

Either way, the session's role is [`../senapati/senapati.md`](../senapati/senapati.md):
own the outcome, do the work instead of routing it, never wait or poll, accept a
ping before working on it, and reply in its thread.

## Run

```sh
cp mahamantri.yaml.example mahamantri.yaml   # fill in the Linear settings
go build -o mahamantri . && ./mahamantri mahamantri.yaml
```

Startup checks everything first: config, store lock, both ports free, and, in
opencode mode, the server and the main session. It exits with a message saying
what to fix. Run it as a service, for example a systemd user unit with
`Restart=on-failure`. Stopping it never stops the main session. Pings that
arrive while it is down are redelivered by Linear; pings already stored wait in
the inbox.

On each start mahamantri writes the bridge's address, a fresh token and the
main session's directory to `bridge.discoveryFile` (default
`$XDG_STATE_HOME/samrajya/bridge.json`), readable only by you. The plugin reads
it. Only a session running in `bridge.mainDirectory` (default: the workspace)
attaches as the main session by itself. A second session stays passive until it
is told to take over. `bridge.trustedUserIDs` lists the Linear users who may ask
for work. Pings from anyone else arrive marked untrusted.

## The workspace

`workspace:` is the directory that holds the projects, one per directory
(`../repos` here, which git ignores). How each project is worked on, including
worktrees, dependencies, services and branches, belongs in that project's own
instruction files, not in Samrajya.

## Memory (the brain)

`brain:` is a directory of short markdown notes, one per fact worth knowing next
time: a person's standing instructions, facts in neither the code nor Linear,
problems that keep coming back. `INDEX.md` lists them. The session reads and
writes them through the bridge (`samrajya_memory`, `samrajya_remember`,
`samrajya_forget`), and they are plain files you can edit too. It is kept small
on purpose: notes under 1,500 bytes, at most 50.
