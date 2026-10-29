import assert from 'node:assert/strict';
import { randomBytes } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';
import type { TestContext } from 'node:test';
import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { StdioClientTransport } from '@modelcontextprotocol/sdk/client/stdio.js';

async function start(t: TestContext, token: string, port = 0) {
  const transport = new StdioClientTransport({
    command: process.execPath,
    args: [fileURLToPath(new URL('../src/server.js', import.meta.url))],
    env: { SAMRAJYA_PROBE_TOKEN: token, SAMRAJYA_PROBE_PORT: String(port) },
    stderr: 'pipe',
  });
  const client = new Client({ name: 'lifecycle-test', version: '0.0.1' });
  t.after(async () => { await client.close(); });
  let stderr = '';
  let listeningFailed!: (reason: Error) => void;
  const listening = new Promise<number>((resolve, reject) => {
    listeningFailed = reject;
    transport.stderr!.on('data', (chunk: Buffer) => {
      stderr += chunk.toString();
      const match = /127\.0\.0\.1:(\d+)/.exec(stderr);
      if (match) resolve(Number(match[1]));
    });
  });
  const closed = new Promise<void>((resolve) => {
    client.onclose = () => {
      listeningFailed(new Error('probe closed before listening'));
      resolve();
    };
  });
  // Attach both handlers before starting the process so an early exit cannot
  // leave an unhandled rejection or make the test wait for a readiness line.
  const connected = client.connect(transport);
  const [, actualPort] = await Promise.all([connected, listening]);
  const request = (id?: string) => fetch(`http://127.0.0.1:${actualPort}/events`, {
    method: id === undefined ? 'GET' : 'POST',
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    body: id === undefined ? undefined : JSON.stringify({ id }),
    signal: AbortSignal.timeout(2000),
  });
  return { client, transport, closed, port: actualPort, request };
}

for (const signal of ['SIGINT', 'SIGTERM'] as const) {
  test(`${signal} closes the actual probe process and its HTTP listener`, { timeout: 10000 }, async (t) => {
    const probe = await start(t, randomBytes(32).toString('hex'));
    assert.equal((await probe.request('before-stop')).status, 202);
    const pid = probe.transport.pid;
    assert.ok(pid, 'SDK must identify the test child before signalling it');
    process.kill(pid, signal);
    await probe.closed;
    await assert.rejects(probe.request());
  });
}

test('actual process restart on the same port loses receipts and requires a fresh acknowledgement', { timeout: 15000 }, async (t) => {
  const token = randomBytes(32).toString('hex');
  const first = await start(t, token);
  assert.equal((await first.request('restart-id')).status, 202);
  assert.equal((await first.client.callTool({
    name: 'acknowledge_probe', arguments: { event_id: 'restart-id' },
  })).isError, undefined);
  const before = await (await first.request()).json();
  assert.equal(before.events[0].acknowledged, true);
  await first.client.close();
  await first.closed;
  await assert.rejects(first.request());

  const second = await start(t, token, first.port);
  const after = await (await second.request()).json();
  assert.notEqual(after.instance, before.instance);
  assert.equal(after.ready, true);
  assert.equal(after.durable, false);
  assert.deepEqual(after.events, []);
  assert.equal((await second.client.callTool({
    name: 'acknowledge_probe', arguments: { event_id: 'restart-id' },
  })).isError, true);
  assert.equal((await second.request('restart-id')).status, 202);
  assert.equal((await second.client.callTool({
    name: 'acknowledge_probe', arguments: { event_id: 'restart-id' },
  })).isError, undefined);
  assert.deepEqual((await (await second.request()).json()).events, [
    { id: 'restart-id', attempts: 1, transportWrites: 1, acknowledged: true },
  ]);
});
