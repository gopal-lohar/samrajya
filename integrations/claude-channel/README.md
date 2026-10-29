# Native channel feasibility probe

An isolated experiment, **not the production Linear bridge**. It tests whether
interactive Claude Code receives a custom MCP channel event and calls an explicit
receipt tool. It does not run Claude, modify its configuration, create workers,
access Linear, manage working copies or change Mahamantri.

The probe:

- Uses the official TypeScript MCP SDK over stdio.
- Binds HTTP only to `127.0.0.1`; requires a random bearer token for every request.
- Emits only a fixed synthetic message, never arbitrary HTTP body text.
- Separately records notification attempts, successful transport writes and tool
  acknowledgements. A `202` response is **not** proof Claude received the event.
- Keeps at most 64 event records **in memory**. Restart loses all receipts and
  changes the instance ID. There is no automatic replay or persistence yet.
- Declares no permission relay, changes no native permission rules, and writes
  diagnostics only to stderr (stdout is reserved for MCP).

## Build and automated tests

Requires Node 24+ and the pinned pnpm version in `package.json`.

```sh
pnpm --dir integrations/claude-channel install --frozen-lockfile --ignore-scripts
pnpm --dir integrations/claude-channel typecheck
pnpm --dir integrations/claude-channel test
pnpm --dir integrations/claude-channel build
```

Tests use SDK clients, including a real stdio child process, and ephemeral
loopback listeners. They do not start Claude, authenticate to a provider or make
external writes. They cover concurrent same-ID sends, ambiguous transport failure,
disconnection, actual child-process signal shutdown and restart on the same port.
These are probe-process tests, not native Claude session lifecycle tests. Passing
them does **not** pass the live native integration gate.

## Interactive test setup

Perform this only after approval to start a separate test session. Do not modify
or stop the existing assistant. Use a private, empty test workspace, not a real
project checkout, and the existing Claude subscription login. Do not use `-p`,
the Agent SDK, a provider proxy or an API key as a substitute for the test.

The commands below use Bash syntax. Replace both placeholder paths. Keep the
private test directory outside tracked source. No global MCP/settings changes are
needed. The selected HTTP port must be unused; choose a different port if needed.

```sh
probe=/absolute/path/to/samrajya/integrations/claude-channel/dist/src/server.js
work=/absolute/path/to/private/probe-workspace
mkdir -p "$work"
chmod 700 "$work"
cd "$work"
umask 077
# Create once; do not overwrite an existing token used by an active probe.
test -e probe-token || openssl rand -hex 32 > probe-token
export SAMRAJYA_PROBE_TOKEN="$(cat probe-token)"
export SAMRAJYA_PROBE_PORT=8788
# Write this new file only in the empty test workspace.
node --input-type=module - "$probe" <<'JS' > probe.mcp.json
const config = {
  mcpServers: {
    'samrajya-probe': {
      command: process.execPath,
      args: [process.argv[2]],
      env: {
        SAMRAJYA_PROBE_TOKEN: '${SAMRAJYA_PROBE_TOKEN}',
        SAMRAJYA_PROBE_PORT: '${SAMRAJYA_PROBE_PORT}'
      }
    }
  }
};
console.log(JSON.stringify(config, null, 2));
JS
claude --mcp-config ./probe.mcp.json \
  --dangerously-load-development-channels server:samrajya-probe \
  --remote-control 'Samrajya probe'
```

Accept project trust, MCP consent and the development-channel warning locally.
The development flag bypasses the preview allowlist for this server; it does not
bypass organization policy. Verify `/mcp` reports a connected server and the
session banner shows the channel. If either fails, stop and record the evidence.
Do not weaken global permissions or authentication to make it work.

This configuration adds only the probe; existing MCP connections are not
intentionally disabled. Do **not** add `--strict-mcp-config` for the combined
Linear trial: with a probe-only file, that flag excludes the existing connectors.
It is suitable only for the isolated synthetic receipt test. Check that the
intended Linear MCP connection is actually available before the separately
authorized Linear trial. Do not copy credentials into this repository.

Passing `--remote-control` is not proof that Remote Control connected. In the
interactive terminal, inspect the `/rc active` indicator or open `/remote-control`
to see its status panel and session link. Verify access from an authenticated
browser/app to that same conversation. A local tool approval or a peer-session
message is not evidence of remote access or remote permission approval.

In a **second terminal**, use the same private token file and port:

```sh
work=/absolute/path/to/private/probe-workspace
export SAMRAJYA_PROBE_TOKEN="$(cat "$work/probe-token")"
port=8788
# Feed headers through stdin so the bearer token is not in curl's command line.
printf 'Authorization: Bearer %s\n' "$SAMRAJYA_PROBE_TOKEN" |
  curl --fail-with-body --header @- --header 'Content-Type: application/json' \
    --data '{"id":"receipt-1"}' "http://127.0.0.1:$port/events"

printf 'Authorization: Bearer %s\n' "$SAMRAJYA_PROBE_TOKEN" |
  curl --fail-with-body --header @- "http://127.0.0.1:$port/events"
```

Expect `acknowledged: false` immediately after the transport write, then `true`
after Claude calls `acknowledge_probe`. It may already be true by the time the
first HTTP response arrives. No other action is requested by the event.

POST the same ID again: an acknowledged event returns `200` without re-emitting;
an unacknowledged event is retried with the same identity. Use a new ID for a new
experiment. GET never causes replay. Neither the probe nor the assistant should
poll in a waiting loop; inspect status at the test checkpoints.

## Live evidence checklist

Record actual observations privately, including CLI version, session identity,
probe instance ID, event IDs and whether the result was a transport write or tool
receipt. Never include tokens in evidence.

### Gate A — interactive

- [ ] Channel opt-in, MCP initialization and explicit synthetic receipt work.
- [ ] Claude app/browser connects to this same local conversation.
- [ ] A synthetic event sent during a bounded active turn is eventually received.
- [ ] Direct Remote Control stop/steer is distinguished from queued channel input.
- [ ] A native tool permission request can be approved/denied remotely. Use manual
      permission mode for this test; don't enable blanket approval or relay.
- [ ] With separate approval for a designated test issue/thread, read and reply
      through the existing Linear MCP connection, verifying the resulting comment.
      The synthetic probe itself never asks for this external write.

### Gate B — unattended feasibility

- [ ] `/background`, terminal detach and Remote Control reconnect preserve the
      intended session and channel.
- [ ] Observe documented idle exit behavior; do not claim permanent listening.
- [ ] Restart/resume the test session using supported native controls; record
      whether a new probe instance starts and manually send a fresh event.
- [ ] Record disconnection and limit behavior without exhausting quota or changing
      providers. Mark real limit behavior unverified when only simulated.

This in-memory probe cannot validate durable recovery, owner fencing, task
continuity or exactly-once effects. Those require the later store/bridge work and
its own fault-injection tests. Gate B at this stage assesses lifecycle feasibility,
not completed production recovery guarantees.

If Gate A fails, do not expand the migration. If only Gate B fails, report an
attended-only limitation rather than inventing a supervisor. No tmux, PTY daemon,
headless channel workaround or agent polling loop.

Stop the test session normally when finished. The probe closes its HTTP listener
on stdio EOF, SIGINT or SIGTERM. Do not kill processes merely because they occupy
a port; establish which test process owns it first.

## HTTP contract

All routes require `Authorization: Bearer <token>`. Browser-origin requests are
rejected; no CORS support. Tokens must be 64 lowercase hex characters generated
from 32 random bytes.

| Request | Result |
|---|---|
| `POST /events`, JSON `{"id":"receipt-1"}` | Emit/retry fixed notification; `202` is transport-only |
| Repeat POST after tool acknowledgement | `200`, no repeated notification |
| `GET /events` | Instance ID, initialization state and all in-memory receipts |
| Wrong/missing token | `401` |
| Extra fields, malformed ID/JSON | `400` |
| Payload over 1 KiB / non-JSON | `413` / `415` |
| More than 64 distinct events | `429`; existing records are not evicted |
| Not initialized / transport write failure | `503`; no receipt guarantee |

A transport failure can be ambiguous: retry the same ID, not a newly generated
one. `SAMRAJYA_PROBE_PORT=0` selects an ephemeral port printed to stderr. An unset
port defaults to zero. This endpoint must not be exposed publicly.

## Reference

- [Claude Code channel contract](https://code.claude.com/docs/en/channels-reference)
- [Remote Control](https://code.claude.com/docs/en/remote-control)
- [Native background sessions](https://code.claude.com/docs/en/agent-view)
