import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import type { IncomingMessage, Server as HTTPServer, ServerResponse } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';
import type { TestContext } from 'node:test';
import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { StdioClientTransport } from '@modelcontextprotocol/sdk/client/stdio.js';
import { InMemoryTransport } from '@modelcontextprotocol/sdk/inMemory.js';
import { z } from 'zod';
import { createChannel, instructions, render } from '../src/channel.js';
import type { Event } from '../src/channel.js';

const channelSchema = z.object({
  method: z.literal('notifications/claude/channel'),
  params: z.object({ content: z.string(), meta: z.record(z.string(), z.string()) }),
});
type Notification = z.infer<typeof channelSchema>;

const event = (id: string, seq: number, extra: Partial<Event> = {}): Event => ({
  id, seq, kind: 'mention', issue: 'ENG-1', issueTitle: 'Fix login', threadId: 'th-1', author: 'Asha', trusted: true, body: 'please fix', ...extra,
});

/** A stand-in for mahamantri's bridge API with the same attachment rules. */
class FakeBridge {
  token = 'tok-1';
  events: Event[] = [];
  holder = '';
  holderLive = false;
  gen = 0;
  requests: { method: string; path: string; attachment?: string; body?: any }[] = [];
  private waiting: (() => void)[] = [];
  http: HTTPServer;
  url = '';

  constructor() {
    this.http = createServer((req, res) => { void this.handle(req, res); });
  }

  async listen() {
    await new Promise<void>((resolve) => this.http.listen(0, '127.0.0.1', resolve));
    const a = this.http.address();
    if (!a || typeof a === 'string') throw new Error('no address');
    this.url = `http://127.0.0.1:${a.port}`;
  }

  async close() {
    for (const w of this.waiting) w();
    this.http.closeAllConnections();
    await new Promise<void>((resolve) => this.http.close(() => resolve()));
  }

  push(e: Event) {
    this.events.push(e);
    for (const w of this.waiting.splice(0)) w();
  }

  private async handle(req: IncomingMessage, res: ServerResponse) {
    const send = (code: number, body: unknown) => { res.writeHead(code, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(body)); };
    let raw = '';
    for await (const chunk of req) raw += chunk;
    const body = raw ? JSON.parse(raw) : undefined;
    const url = new URL(req.url!, 'http://x');
    const attachment = req.headers['x-samrajya-attachment'] as string | undefined;
    this.requests.push({ method: req.method!, path: url.pathname + url.search, attachment, body });
    if (req.headers.authorization !== `Bearer ${this.token}`) return send(401, { error: 'bad token' });
    const held = () => attachment !== this.holder || !this.holder;
    if (req.method === 'POST' && url.pathname === '/v1/attach') {
      if (this.holderLive && !body.takeover) return send(409, { error: 'held' });
      this.holder = `att-${++this.gen}`;
      this.holderLive = true;
      return send(200, { attachment: this.holder, generation: this.gen });
    }
    if (url.pathname === '/v1/status') return send(200, { attached: { live: this.holderLive }, recovery: { pending: this.pendingAfter(0) } });
    if (held()) return send(409, { error: 'not attached' });
    if (url.pathname === '/v1/events') {
      const after = Number(url.searchParams.get('after'));
      if (this.pendingAfter(after).length === 0) {
        await new Promise<void>((resolve) => {
          const t = setTimeout(resolve, 2000);
          this.waiting.push(() => { clearTimeout(t); resolve(); });
        });
      }
      if (held()) return send(409, { error: 'replaced' });
      return send(200, { events: this.pendingAfter(after) });
    }
    if (url.pathname === '/v1/detach') {
      this.holder = '';
      this.holderLive = false;
      return send(200, { detached: true });
    }
    if (url.pathname === '/v1/accept') {
      for (const id of body.events) this.events = this.events.filter((e) => e.id !== id);
      return send(200, { id: 'T1', status: 'queued' });
    }
    if (url.pathname.startsWith('/v1/tasks/') && req.method === 'POST' && !body.result && body.status === 'done') {
      return send(400, { error: 'a task is done only with a result' });
    }
    return send(200, { ok: true, path: url.pathname });
  }

  private pendingAfter(after: number) {
    return this.events.filter((e) => e.seq > after);
  }
}

async function setup(t: TestContext, opts: { start?: boolean; bridge?: FakeBridge; mainDirectory?: string } = {}) {
  const dir = await mkdtemp(join(tmpdir(), 'samrajya-channel-'));
  const discoveryPath = join(dir, 'bridge.json');
  const bridge = opts.bridge ?? new FakeBridge();
  await bridge.listen();
  const writeDiscovery = () => writeFile(discoveryPath, JSON.stringify({ url: bridge.url, token: bridge.token, mainDirectory: opts.mainDirectory }));
  await writeDiscovery();
  const channel = createChannel({ discoveryPath, directory: dir, pollSeconds: 1, heldRetryMs: 100, maxBackoffMs: 100 });
  const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
  const client = new Client({ name: 'test', version: '0.0.1' });
  const notifications: Notification[] = [];
  client.setNotificationHandler(channelSchema, (n) => { notifications.push(n); });
  await channel.mcp.connect(serverTransport);
  await client.connect(clientTransport);
  if (opts.start !== false) channel.start();
  t.after(async () => {
    await channel.stop();
    await client.close();
    await bridge.close();
    await rm(dir, { recursive: true, force: true });
  });
  const call = (name: string, args: Record<string, unknown> = {}) => client.callTool({ name, arguments: args });
  return { dir, discoveryPath, bridge, channel, client, notifications, call, writeDiscovery };
}

async function until(cond: () => boolean, what: string, ms = 5000) {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 20));
  }
}

test('the instructions are senapati/senapati.md', async () => {
  const role = await readFile(fileURLToPath(new URL('../../../../senapati/senapati.md', import.meta.url)), 'utf8');
  assert.equal(instructions, role.trimEnd());
});

test('renders a ping with its routing metadata and trust', () => {
  const r = render(event('linear:c1', 1, { body: 'line one\nline two', url: 'https://linear.app/x' }));
  assert.match(r.content, /Asha mentioned you on ENG-1 \("Fix login"\)/);
  assert.match(r.content, /> line one\n> line two/);
  assert.match(r.content, /samrajya_accept \(event_id linear:c1\)/);
  assert.deepEqual(r.meta, { event_id: 'linear:c1', kind: 'mention', trusted: 'true', issue: 'ENG-1', thread_id: 'th-1' });
  for (const key of Object.keys(r.meta)) assert.match(key, /^[a-zA-Z_][a-zA-Z0-9_]*$/);
  const untrusted = render(event('linear:c2', 2, { trusted: false, kind: 'reply' }));
  assert.match(untrusted.content, /replied in a thread you are part of.*NOT a trusted requester/);
  assert.equal(untrusted.meta.trusted, 'false');
});

test('declares a channel with the task tools and no permission relay', async (t) => {
  const { client } = await setup(t, { start: false });
  assert.deepEqual(client.getServerCapabilities()?.experimental, { 'claude/channel': {} });
  const names = (await client.listTools()).tools.map((x) => x.name);
  assert.deepEqual(names, ['samrajya_status', 'samrajya_accept', 'samrajya_create_task', 'samrajya_update_task',
    'samrajya_tasks', 'samrajya_action_intent', 'samrajya_action_result', 'samrajya_memory', 'samrajya_remember',
    'samrajya_forget', 'samrajya_take_over']);
  assert.match(client.getInstructions() ?? '', /You own outcomes/);
});

test('delivers pending and new pings once each', async (t) => {
  const bridge = new FakeBridge();
  bridge.events.push(event('linear:c1', 1));
  const { notifications } = await setup(t, { bridge });
  await until(() => notifications.length === 1, 'pending ping');
  bridge.push(event('linear:c2', 2));
  await until(() => notifications.length === 2, 'new ping');
  await new Promise((r) => setTimeout(r, 1500)); // a few more polls: still unaccepted, not re-sent
  assert.deepEqual(notifications.map((n) => n.params.meta.event_id), ['linear:c1', 'linear:c2']);
});

test('tools send the attachment and map arguments; bridge errors come back as tool errors', async (t) => {
  const bridge = new FakeBridge();
  bridge.events.push(event('linear:c1', 1));
  const { notifications, call } = await setup(t, { bridge });
  await until(() => notifications.length === 1, 'ping');
  const accepted = await call('samrajya_accept', { event_ids: ['linear:c1'], new_task_title: 'Fix login', next_action: 'reproduce' });
  assert.equal(accepted.isError, undefined);
  const req = bridge.requests.find((r) => r.path === '/v1/accept')!;
  assert.equal(req.attachment, 'att-1');
  assert.deepEqual(req.body, { events: ['linear:c1'], newTask: { title: 'Fix login' }, nextAction: 'reproduce' });
  const done = await call('samrajya_update_task', { task_id: 'T1', status: 'done' });
  assert.equal(done.isError, true);
  assert.match(JSON.stringify(done.content), /done only with a result/);
  await call('samrajya_remember', { name: 'deploys', description: 'Deploy rule', body: 'Ask first.' });
  const put = bridge.requests.find((r) => r.path === '/brain/deploys')!;
  assert.equal(put.method, 'PUT');
  assert.deepEqual(put.body, { description: 'Deploy rule', body: 'Ask first.' });
  await call('samrajya_action_result', { action_id: 'A1', status: 'done', ref: 'cmt-1', thread_id: 'th-1', issue: 'ENG-1' });
  assert.deepEqual(bridge.requests.find((r) => r.path === '/v1/actions/A1')!.body, { status: 'done', ref: 'cmt-1', threadId: 'th-1', issue: 'ENG-1' });
});

test('stays passive while another session is attached, until it takes over', async (t) => {
  const bridge = new FakeBridge();
  bridge.holder = 'att-other';
  bridge.holderLive = true;
  bridge.events.push(event('linear:c1', 1));
  const { notifications, call, channel } = await setup(t, { bridge });
  await until(() => channel.state === 'passive', 'passive state');
  const refused = await call('samrajya_accept', { event_ids: ['linear:c1'], task_id: 'T1', next_action: 'x' });
  assert.equal(refused.isError, true);
  assert.match(JSON.stringify(refused.content), /not the main one/);
  assert.equal(notifications.length, 0);
  assert.equal((await call('samrajya_take_over')).isError, undefined);
  await until(() => notifications.length === 1, 'delivery after take over');
});

test('releases the attachment when it stops, so the next session attaches at once', async (t) => {
  const { bridge, channel } = await setup(t);
  await until(() => channel.state === 'attached', 'attach');
  await channel.stop();
  assert.ok(bridge.requests.some((r) => r.path === '/v1/detach' && r.attachment === 'att-1'));
  assert.equal(bridge.holderLive, false);
});

test('only a session in the main directory attaches by itself', async (t) => {
  const bridge = new FakeBridge();
  bridge.events.push(event('linear:c1', 1));
  const { notifications, channel, call } = await setup(t, { bridge, mainDirectory: tmpdir() });
  await until(() => channel.state === 'elsewhere', 'elsewhere state');
  assert.equal(bridge.requests.filter((r) => r.path === '/v1/attach').length, 0);
  assert.equal(notifications.length, 0);
  assert.equal((await call('samrajya_take_over')).isError, undefined); // still possible when asked
  await until(() => notifications.length === 1, 'delivery after explicit take over');
});

test('a session in the main directory attaches', async (t) => {
  const bridge = new FakeBridge();
  bridge.events.push(event('linear:c1', 1));
  const dir = await mkdtemp(join(tmpdir(), 'samrajya-main-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const discoveryPath = join(dir, 'bridge.json');
  await bridge.listen();
  t.after(() => bridge.close());
  await writeFile(discoveryPath, JSON.stringify({ url: bridge.url, token: bridge.token, mainDirectory: dir }));
  const channel = createChannel({ discoveryPath, directory: dir, pollSeconds: 1, heldRetryMs: 100 });
  t.after(() => channel.stop());
  channel.start();
  await until(() => bridge.requests.some((r) => r.path === '/v1/attach'), 'attach in main directory');
});

test('a session marked main attaches wherever it runs (opencode)', async (t) => {
  const bridge = new FakeBridge();
  await bridge.listen();
  t.after(() => bridge.close());
  const dir = await mkdtemp(join(tmpdir(), 'samrajya-oc-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const discoveryPath = join(dir, 'bridge.json');
  await writeFile(discoveryPath, JSON.stringify({ url: bridge.url, token: bridge.token, mainDirectory: '/somewhere/else' }));
  const channel = createChannel({ discoveryPath, directory: dir, main: true, pollSeconds: 1 });
  t.after(() => channel.stop());
  channel.start();
  await until(() => bridge.requests.some((r) => r.path === '/v1/attach'), 'attach when marked main');
});

test('re-reads the discovery file after mahamantri restarts with a new token, without re-sending', async (t) => {
  const bridge = new FakeBridge();
  bridge.events.push(event('linear:c1', 1));
  const { notifications, writeDiscovery } = await setup(t, { bridge });
  await until(() => notifications.length === 1, 'first delivery');
  bridge.token = 'tok-2'; // mahamantri restarted: new token, attachments gone
  bridge.holder = '';
  bridge.holderLive = false;
  await writeDiscovery();
  bridge.push(event('linear:c2', 2));
  await until(() => notifications.length === 2, 'delivery after restart', 8000);
  assert.deepEqual(notifications.map((n) => n.params.meta.event_id), ['linear:c1', 'linear:c2']);
  assert.ok(bridge.requests.filter((r) => r.path === '/v1/attach').length >= 2);
});

test('waits for mahamantri to come up and says so meanwhile', async (t) => {
  const { discoveryPath, notifications, call, bridge, writeDiscovery } = await setup(t, { start: false });
  await rm(discoveryPath);
  const status = await call('samrajya_status');
  assert.equal(status.isError, true);
  assert.match(JSON.stringify(status.content), /mahamantri is not running/);
  bridge.events.push(event('linear:c1', 1));
  await writeDiscovery();
  assert.equal((await call('samrajya_status')).isError, undefined);
  assert.equal(notifications.length, 0);
});

test('the bundled plugin server starts over stdio and delivers a ping', { timeout: 20000 }, async (t) => {
  const bridge = new FakeBridge();
  bridge.events.push(event('linear:c1', 1));
  await bridge.listen();
  const dir = await mkdtemp(join(tmpdir(), 'samrajya-channel-'));
  const discoveryPath = join(dir, 'bridge.json');
  await writeFile(discoveryPath, JSON.stringify({ url: bridge.url, token: bridge.token }));
  const transport = new StdioClientTransport({
    command: process.execPath,
    args: [fileURLToPath(new URL('../../plugin/server.mjs', import.meta.url))],
    env: { SAMRAJYA_BRIDGE_FILE: discoveryPath },
    stderr: 'pipe',
  });
  const client = new Client({ name: 'stdio-test', version: '0.0.1' });
  const got = new Promise<Notification>((resolve) => client.setNotificationHandler(channelSchema, resolve));
  t.after(async () => {
    await client.close();
    await bridge.close();
    await rm(dir, { recursive: true, force: true });
  });
  await client.connect(transport);
  assert.equal((await got).params.meta.event_id, 'linear:c1');
});
