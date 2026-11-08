# Working on Samrajya

This is Samrajya's own codebase. A Samrajya session working on it is changing
the system it runs in, so these rules come first.

## The repository may be public

- Never commit anything private: people's names, private project names or
  paths, issue keys, Linear or Telegram ids, tokens, chat contents. Examples
  and tests use made-up names (Asha, ENG-1, example.com).
- Never commit runtime files: configs other than `*.example`, stores
  (`*.db`), logs, `brain/`, `repos/`. Check `git status` before every commit
  and add files by name, never a whole directory.
- Push only when the person asks.

## How to work

- Work in a worktree of your own under `.worktrees/` (git-ignored), on a
  branch: `git worktree add .worktrees/<name> -b <name>`. Never work in the
  main checkout.
- Go (`mahamantri/`): `go vet ./... && go test -race ./...`. Never run
  `go build` without `-o` somewhere outside the tree.
- The plugin (`integrations/claude-channel/`): `pnpm install`, then
  `pnpm test`. A change to the plugin needs a new version in
  `plugin/.claude-plugin/plugin.json`, or sessions keep loading the old one;
  a deploy refuses it otherwise.
- Roles (`senapati/*.md`) are what sessions are told; keep them short and
  structural. Prefer enforcing a rule in code over writing it in a role.
- A deploy may be rolled back to the release before it, which then reads the
  same store: never make a store change an older release cannot read.
- Keep the docs (`README.md`, `mahamantri/README.md`, `docs/design/`) true to
  what you changed.

## Putting a change into service

Samrajya runs from a release built from a commit, not from this checkout.
Once your commit is tested, ask with `samrajya_restart` (kind `deploy`, `ref`
your branch or commit, and a reason saying what it changes and how you
verified it). The person approves it in the conversation; mahamantri then
builds the commit, runs the tests, switches to it and restarts, and puts the
running release back if Samrajya does not come up. Never restart, stop or
redeploy Samrajya yourself, and never touch its config, store, releases or
sessions. After a successful deploy, merge the branch into `main`
(fast-forward) so the codebase matches what runs.
