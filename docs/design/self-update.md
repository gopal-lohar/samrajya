# Self-update: Samrajya changes itself, with the person's OK

**Status:** built. The approval flow and the script's deploy and rollback are
tested; the first live deploys are the acceptance test.

## What it is for

The person asks Samrajya, on Telegram or Linear, to change how Samrajya works.
A worker does the change and asks to deploy it; the person approves it in the
same conversation, and Samrajya deploys and restarts itself. Nobody needs a
terminal.

## Layout

```
<home>/                    the service's working directory (not a checkout)
  mahamantri.yaml          the config
  mahamantri.db            the store
  brain/                   memory notes
  repos/                   the workspace
    samrajya/              the codebase: a git repository, a project like any other
  releases/<commit>/       built releases: mahamantri/mahamantri, senapati/,
                           scripts/, integrations/ (the plugin), RELEASE
  current -> releases/<commit>
  integrations -> current/integrations    the plugin marketplace's directory
```

The service runs `current/mahamantri/mahamantri mahamantri.yaml`, and the
config's role files point into `current/senapati/`. Editing the codebase
changes nothing that runs until a deploy.

## The flow

1. A session asks with `samrajya_restart`: `deploy` (with a ref), `restart`
   or `restart_main`, and a reason. mahamantri resolves the ref to a commit,
   describes what it changes against the running release, and posts the
   request in the session's conversation (a worker's task conversation; the
   home conversation for the main): *To go ahead, send a message that says
   only: approve R3*.
2. A trusted person's message in that conversation saying only
   `approve R3` (or `refuse R3`) decides it. A quote of the request, an edit,
   an untrusted author, another conversation or a lapsed request (24 hours)
   decides nothing. One change runs at a time.
3. mahamantri hands the change to `scripts/samrajya` in a transient systemd
   unit (`<service>-r3`), which outlives mahamantri's restart.
4. `deploy` builds the commit from a `git archive` into `releases/<commit>`
   (go vet and test, plugin test and bundle), checks the config loads,
   refuses a plugin change without a new plugin version, then switches
   `current`, installs the plugin if its version changed, restarts the
   service and waits for the bridge and the main session. If the role or the
   plugin changed, it replaces the main session. If Samrajya does not come
   up, it switches back, restores the plugin registry and restarts again.
5. The script writes `R3.json` (ok, text) next to its log in
   `<agents.stateDir>/approvals/`. mahamantri, restarted, finds it, and posts
   how it went in the conversation and to the session that asked.

## Limits

- The gate is against mistakes, not malice. Sessions run as the same user as
  mahamantri, so a session determined to restart it could. Launches deny the
  obvious commands (`systemctl --user restart samrajya-…`,
  `scripts/samrajya deploy`, `claude stop`), the roles forbid it, and only a
  surface message can approve. A permission rule does not catch every form
  of a command.
- A rollback runs the release before on the same store. A store change an
  older release cannot read would break that, so changes must stay
  compatible (see the codebase's CLAUDE.md).
- Approval comes only from a surface. A person talking to the main session
  through Remote Control approves on Telegram or Linear.
