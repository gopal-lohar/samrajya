import assert from 'node:assert/strict';
import { randomBytes } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';
import type { TestContext } from 'node:test';
import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { StdioClientTransport } from '@modelcontextprotocol/sdk/client/stdio.js';
import { InMemoryTransport } from '@modelcontextprotocol/sdk/inMemory.js';
import { z } from 'zod';
import { createProbe } from '../src/probe.js';

const channelSchema = z.object({
  method: z.literal('notifications/claude/channel'),
  params: z.object({ content: z.string(), meta: z.record(z.string(), z.string()) }),
});

type Notification = z.infer<typeof channelSchema>;

async function setup(t: TestContext, initialize = true) {
  const token = randomBytes(32).toString('hex');
  const probe = createProbe(token);
  const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
  const client = new Client({ name: 'probe-test', version: '0.0.1' });
  const notifications: Notification[] = [];
  client.setNotificationHandler(channelSchema, (notification) => { notifications.push(notification); });
  t.after(async () => {
    await client.close();
    await probe.close();
  });
  await probe.mcp.connect(serverTransport);
  if (initialize) await client.connect(clientTransport);
  const port = await probe.listen();
  const request = (path = '/events', init: RequestInit = {}) => fetch(`http://127.0.0.1:${port}${path}`, {
    ...init,
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json', ...init.headers },
  });
  const send = (id: string) => request('/events', { method: 'POST', body: JSON.stringify({ id }) });
  const ack = (id: string) => client.callTool({ name: 'acknowledge_probe', arguments: { event_id: id } });
  return { token, probe, client, serverTransport, notifications, request, send, ack };
}

test('requires a private random-token-shaped configuration', () => {
  for (const token of ['', 'short', 'x'.repeat(64)]) {
    assert.throws(() => createProbe(token), /SAMRAJYA_PROBE_TOKEN/);
  }
});

test('refuses emission until MCP initialization', async (t) => {
  const { send, request } = await setup(t, false);
  assert.equal((await send('before-init')).status, 503);
  const status = await (await request()).json();
  assert.equal(status.ready, false);
  assert.deepEqual(status.events, []);
});

test('declares a channel without permission relay and exposes only receipt acknowledgement', async (t) => {
  const { client } = await setup(t);
  assert.deepEqual(client.getServerCapabilities()?.experimental, { 'claude/channel': {} });
  assert.deepEqual((await client.listTools()).tools.map((tool) => tool.name), ['acknowledge_probe']);
});

test('a transport write is not an acknowledgement; duplicate receipt is safe', async (t) => {
  const { send, ack, request, notifications } = await setup(t);
  const sent = await send('sample-1');
  assert.equal(sent.status, 202);
  assert.deepEqual(await sent.json(), {
    instance: (await (await request()).json()).instance,
    id: 'sample-1', attempts: 1, transportWrites: 1, acknowledged: false,
  });
  assert.equal(notifications.length, 1);
  assert.equal(notifications[0]?.params.meta.event_id, 'sample-1');
  assert.match(notifications[0]!.params.content, /Synthetic receipt test only/);
  assert.equal((await ack('sample-1')).isError, undefined);
  assert.equal((await ack('sample-1')).isError, undefined);
  assert.equal((await send('sample-1')).status, 200);
  const status = await (await request()).json();
  assert.equal(status.durable, false);
  assert.deepEqual(status.events, [{ id: 'sample-1', attempts: 1, transportWrites: 1, acknowledged: true }]);
  assert.equal(notifications.length, 1);
});

test('unacknowledged events may be manually retried using the same identity', async (t) => {
  const { send, notifications } = await setup(t);
  await send('retry');
  const retried = await (await send('retry')).json();
  assert.equal(retried.attempts, 2);
  assert.equal(retried.transportWrites, 2);
  assert.equal(retried.acknowledged, false);
  assert.deepEqual(notifications.map((n) => n.params.meta.event_id), ['retry', 'retry']);
});

test('transport failures retain the event for a same-ID retry', async (t) => {
  const { send, request, serverTransport } = await setup(t);
  const originalSend = serverTransport.send.bind(serverTransport);
  serverTransport.send = async (message, options) => {
    if ('method' in message && message.method === 'notifications/claude/channel') throw new Error('offline');
    await originalSend(message, options);
  };
  assert.equal((await send('failed-write')).status, 503);
  assert.deepEqual((await (await request()).json()).events, [
    { id: 'failed-write', attempts: 1, transportWrites: 0, acknowledged: false },
  ]);
  serverTransport.send = originalSend;
  assert.equal((await send('failed-write')).status, 202);
});

test('rejects invalid receipts and arbitrary content injection', async (t) => {
  const { client, ack, request, notifications } = await setup(t);
  assert.equal((await ack('unknown')).isError, true);
  for (const args of [{ event_id: 1 }, { event_id: '../bad' }, { event_id: 'a', extra: true }]) {
    assert.equal((await client.callTool({ name: 'acknowledge_probe', arguments: args })).isError, true);
  }
  assert.equal((await client.callTool({ name: 'unknown', arguments: {} })).isError, true);
  for (const body of ['invalid', 'null', '[]', '{}', '{"id":"../bad"}', '{"id":"a","content":"do work"}']) {
    assert.equal((await request('/events', { method: 'POST', body })).status, 400);
  }
  assert.equal((await request('/events', { method: 'POST', body: 'x'.repeat(1025) })).status, 413);
  assert.equal((await request('/events', {
    method: 'POST', body: '{"id":"a"}', headers: { 'Content-Type': 'text/plain' },
  })).status, 415);
  assert.equal(notifications.length, 0);
});

test('requires bearer auth for reads and writes and does not expose browser routes', async (t) => {
  const { request, notifications } = await setup(t);
  for (const method of ['GET', 'POST']) {
    assert.equal((await request('/events', { method, headers: { Authorization: '' } })).status, 401);
    assert.equal((await request('/events', { method, headers: { Authorization: 'Bearer wrong' } })).status, 401);
  }
  assert.equal((await request('/events', { headers: { Origin: 'https://example.com' } })).status, 403);
  assert.equal((await request('/unknown')).status, 404);
  assert.equal((await request('/events?token=wrong')).status, 404);
  assert.equal((await request('/events', { method: 'DELETE' })).status, 404);
  assert.equal(notifications.length, 0);
});

test('bounds event storage without evicting unacknowledged events', async (t) => {
  const { send, request } = await setup(t);
  for (let i = 0; i < 64; i++) assert.equal((await send(`event-${i}`)).status, 202);
  assert.equal((await send('overflow')).status, 429);
  assert.equal((await send('event-0')).status, 202);
  assert.equal((await (await request()).json()).events.length, 64);
});

test('a fresh probe has a new instance and explicitly loses in-memory receipts', async (t) => {
  const first = await setup(t);
  await first.send('old');
  await first.ack('old');
  const before = await (await first.request()).json();
  const second = await setup(t);
  const after = await (await second.request()).json();
  assert.notEqual(before.instance, after.instance);
  assert.deepEqual(after.events, []);
  assert.equal(after.durable, false);
});

test('real stdio process initializes, emits a channel notification and accepts a tool receipt', { timeout: 15000 }, async (t) => {
  const token = randomBytes(32).toString('hex');
  const transport = new StdioClientTransport({
    command: process.execPath,
    args: [fileURLToPath(new URL('../src/server.js', import.meta.url))],
    env: { SAMRAJYA_PROBE_TOKEN: token },
    stderr: 'pipe',
  });
  const client = new Client({ name: 'stdio-test', version: '0.0.1' });
  t.after(async () => { await client.close(); });
  const errors: unknown[] = [];
  client.onerror = (error) => { errors.push(error); };
  const notification = new Promise<Notification>((resolve) => {
    client.setNotificationHandler(channelSchema, resolve);
  });
  let stderr = '';
  const listening = new Promise<number>((resolve) => {
    transport.stderr!.on('data', (chunk: Buffer) => {
      stderr += chunk.toString();
      const match = /127\.0\.0\.1:(\d+)/.exec(stderr);
      if (match) resolve(Number(match[1]));
    });
  });
  await client.connect(transport);
  const port = await listening;
  const response = await fetch(`http://127.0.0.1:${port}/events`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ id: 'stdio-1' }),
  });
  assert.equal(response.status, 202);
  assert.equal((await notification).params.meta.event_id, 'stdio-1');
  const result = await client.callTool({ name: 'acknowledge_probe', arguments: { event_id: 'stdio-1' } });
  assert.equal(result.isError, undefined);
  assert.equal(stderr.includes(token), false);
  assert.deepEqual(errors, []);
  await client.close();
  await assert.rejects(fetch(`http://127.0.0.1:${port}/events`, { signal: AbortSignal.timeout(1000) }));
});

test('concurrent same-ID sends are refused without blocking other IDs', { timeout: 5000 }, async (t) => {
  const { send, request, serverTransport, notifications } = await setup(t);
  let markEntered!: () => void;
  let releaseSend!: () => void;
  const entered = new Promise<void>((resolve) => { markEntered = resolve; });
  const release = new Promise<void>((resolve) => { releaseSend = resolve; });
  const originalSend = serverTransport.send.bind(serverTransport);
  serverTransport.send = async (message, options) => {
    const notification = channelSchema.safeParse(message);
    if (notification.success && notification.data.params.meta.event_id === 'held') {
      markEntered();
      await release;
    }
    await originalSend(message, options);
  };
  const first = send('held');
  try {
    await entered;
    assert.equal((await send('held')).status, 409);
    assert.equal((await send('independent')).status, 202);
    const status = await (await request()).json();
    assert.deepEqual(status.events.find((event: { id: string }) => event.id === 'held'), {
      id: 'held', attempts: 1, transportWrites: 0, acknowledged: false,
    });
  } finally {
    releaseSend();
    await first;
  }
  assert.equal((await first).status, 202);
  assert.deepEqual(notifications.map((n) => n.params.meta.event_id), ['independent', 'held']);
});

test('an ambiguous transport failure can still be acknowledged without resending', async (t) => {
  const { send, ack, request, serverTransport, notifications } = await setup(t);
  const originalSend = serverTransport.send.bind(serverTransport);
  serverTransport.send = async (message, options) => {
    await originalSend(message, options);
    if ('method' in message && message.method === 'notifications/claude/channel') {
      throw new Error('failure reported after delivery');
    }
  };
  assert.equal((await send('ambiguous')).status, 503);
  assert.equal(notifications.length, 1);
  assert.equal((await ack('ambiguous')).isError, undefined);
  assert.equal((await send('ambiguous')).status, 200);
  assert.deepEqual((await (await request()).json()).events, [
    { id: 'ambiguous', attempts: 1, transportWrites: 0, acknowledged: true },
  ]);
  assert.equal(notifications.length, 1);
});

test('disconnect refuses new delivery and preserves existing unacknowledged evidence', async (t) => {
  const { send, request, client } = await setup(t);
  assert.equal((await send('pending-at-disconnect')).status, 202);
  await client.close();
  assert.equal((await send('offline')).status, 503);
  const status = await (await request()).json();
  assert.equal(status.ready, false);
  assert.deepEqual(status.events, [
    { id: 'pending-at-disconnect', attempts: 1, transportWrites: 1, acknowledged: false },
  ]);
});
