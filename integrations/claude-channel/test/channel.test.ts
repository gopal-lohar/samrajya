import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import type { IncomingMessage, Server as HTTPServer, ServerResponse } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawn } from 'node:child_process';
import { test } from 'node:test';
import type { TestContext } from 'node:test';
import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { StdioClientTransport } from '@modelcontextprotocol/sdk/client/stdio.js';
import { InMemoryTransport } from '@modelcontextprotocol/sdk/inMemory.js';
import { z } from 'zod';
import { agentFromEnv, type Agent } from '../src/bridge.js';
import { createChannel, instructions, notAnAgent, render, toolsFor } from '../src/channel.js';
import type { Event } from '../src/channel.js';

const channelSchema = z.object({
  method: z.literal('notifications/claude/channel'),
  params: z.object({ content: z.string(), meta: z.record(z.string(), z.string()) }),
});
type Notification = z.infer<typeof channelSchema>;

const event = (id: string, seq: number, extra: Partial<Event> = {}): Event => ({
  id, seq, source: 'linear', kind: 'mention', conversation: 'linear:uuid-1', title: 'ENG-1: Fix login', threadId: 'th-1',
  author: 'Asha', trusted: true, body: 'please fix', ...extra,
});

const MAIN: Agent = { id: 'main', secret: 'm' };

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
      if (body.agent && body.secret === 'stale') return send(403, { error: 'stale secret' });
      if (!body.agent) return send(409, { error: 'not an agent' });
      this.holder = `att-${++this.gen}`;
      this.holderLive = true;
      return send(200, { attachment: this.holder, generation: this.gen });
    }
    if (url.pathname === '/v1/limit') return send(200, { until: '2026-11-02T19:10:00Z' });
    if (url.pathname === '/v1/turn') return send(200, { ok: true });
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

async function setup(t: TestContext, opts: { start?: boolean; bridge?: FakeBridge; agent?: Agent | null } = {}) {
  const dir = await mkdtemp(join(tmpdir(), 'samrajya-channel-'));
  const discoveryPath = join(dir, 'bridge.json');
  const bridge = opts.bridge ?? new FakeBridge();
  await bridge.listen();
  const writeDiscovery = () => writeFile(discoveryPath, JSON.stringify({ url: bridge.url, token: bridge.token }));
  await writeDiscovery();
  const agent = opts.agent === null ? undefined : opts.agent ?? MAIN;
  const channel = createChannel({ discoveryPath, pollSeconds: 1, maxBackoffMs: 100, agent });
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

test('instructions explain the channel; a session that is no agent is told so', () => {
  assert.match(instructions, /samrajya_reply in the event's conversation and thread/);
  assert.match(notAnAgent, /not one of Samrajya's agents/);
});

test('renders a message with its conversation, thread and trust', () => {
  const r = render(event('linear:c1', 1, { body: 'line one\nline two', url: 'https://linear.app/x' }));
  assert.match(r.content, /^Linear: Asha mentioned you in "ENG-1: Fix login"\./);
  assert.match(r.content, /> line one\n> line two/);
  assert.match(r.content, /samrajya_accept \(event_id linear:c1\)/);
  assert.match(r.content, /samrajya_reply in conversation linear:uuid-1, thread th-1/);
  assert.deepEqual(r.meta, { event_id: 'linear:c1', kind: 'mention', trusted: 'true', source: 'linear', conversation: 'linear:uuid-1', thread_id: 'th-1' });
  for (const key of Object.keys(r.meta)) assert.match(key, /^[a-zA-Z_][a-zA-Z0-9_]*$/);
  const untrusted = render(event('linear:c2', 2, { trusted: false, kind: 'reply' }));
  assert.match(untrusted.content, /replied in a thread you are part of.*NOT a trusted requester/);
  assert.equal(untrusted.meta.trusted, 'false');
  const chat = render(event('telegram:7:11', 3, { source: 'telegram', kind: 'message', conversation: 'telegram:7', title: 'Chat with Asha',
    threadId: '11', attachments: [{ kind: 'voice', ref: 'f1', seconds: 12 }] }));
  assert.match(chat.content, /^Telegram: Asha wrote in "Chat with Asha"/);
  assert.match(chat.content, /Attached: voice, 12s \(samrajya_fetch with event_id telegram:7:11 and ref f1/);
  assert.match(chat.content, /conversation telegram:7, thread 11/);
});

test('declares a channel with the task tools and no permission relay', async (t) => {
  const { client } = await setup(t, { start: false });
  assert.deepEqual(client.getServerCapabilities()?.experimental, { 'claude/channel': {} });
  const names = (await client.listTools()).tools.map((x) => x.name);
  assert.deepEqual(names, ['samrajya_status', 'samrajya_accept', 'samrajya_create_task', 'samrajya_update_task',
    'samrajya_tasks', 'samrajya_reply', 'samrajya_action_intent', 'samrajya_action_result', 'samrajya_memory', 'samrajya_remember',
    'samrajya_forget', 'samrajya_agents', 'samrajya_dispatch', 'samrajya_fetch', 'samrajya_message', 'samrajya_stop_agent']);
  assert.match(client.getInstructions() ?? '', /Samrajya's channel/);
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
  await call('samrajya_reply', { conversation: 'telegram:7', thread_id: '11', body: 'On it' });
  assert.deepEqual(bridge.requests.find((r) => r.path === '/v1/reply')!.body, { conversation: 'telegram:7', threadId: '11', body: 'On it' });
  await call('samrajya_dispatch', { brief: 'write the notes', conversation: 'telegram:7', route: false, links: ['ENG-1'] });
  assert.deepEqual(bridge.requests.find((r) => r.path === '/v1/dispatch')!.body, { brief: 'write the notes', conversation: 'telegram:7', route: false, links: ['ENG-1'] });
  await call('samrajya_fetch', { event_id: 'telegram:7:11', ref: 'f1' });
  assert.deepEqual(bridge.requests.find((r) => r.path === '/v1/fetch')!.body, { eventId: 'telegram:7:11', ref: 'f1' });
});

test('a session that is no agent receives nothing and cannot act', async (t) => {
  const bridge = new FakeBridge();
  bridge.events.push(event('linear:c1', 1));
  const { notifications, call, channel, client } = await setup(t, { bridge, agent: null });
  await new Promise((r) => setTimeout(r, 300));
  assert.equal(channel.state, 'not an agent');
  assert.equal(notifications.length, 0);
  assert.ok(!bridge.requests.some((r) => r.path === '/v1/attach'));
  assert.deepEqual((await client.listTools()).tools.map((x) => x.name), ['samrajya_status', 'samrajya_memory']);
  assert.equal((await call('samrajya_status')).isError, undefined);
});

test('releases the attachment when it stops, so the next session attaches at once', async (t) => {
  const { bridge, channel } = await setup(t);
  await until(() => channel.state === 'attached', 'attach');
  await channel.stop();
  assert.ok(bridge.requests.some((r) => r.path === '/v1/detach' && r.attachment === 'att-1'));
  assert.equal(bridge.holderLive, false);
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
  await writeFile(join(dir, 'main.secret'), 'm');
  const transport = new StdioClientTransport({
    command: process.execPath,
    args: [fileURLToPath(new URL('../../plugin/server.mjs', import.meta.url))],
    env: { SAMRAJYA_BRIDGE_FILE: discoveryPath, SAMRAJYA_AGENT: 'main', SAMRAJYA_AGENT_SECRET_FILE: join(dir, 'main.secret') },
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

test('renders a notice from Samrajya as needing no task', () => {
  const r = render({ id: 'samrajya:resume-1', seq: 3, source: 'samrajya', kind: 'limit_reset', trusted: true, body: 'Your usage limit has reset.' });
  assert.match(r.content, /^Samrajya: Your usage limit has reset\./);
  assert.match(r.content, /samrajya_accept \(event_id samrajya:resume-1\); it needs no task/);
  assert.deepEqual(r.meta, { event_id: 'samrajya:resume-1', kind: 'limit_reset', trusted: 'true' });
});

test('the StopFailure hook reports an agent hitting the usage limit, and nothing else', { timeout: 20000 }, async (t) => {
  const bridge = new FakeBridge();
  await bridge.listen();
  const dir = await mkdtemp(join(tmpdir(), 'samrajya-hook-'));
  const discoveryPath = join(dir, 'bridge.json');
  await writeFile(discoveryPath, JSON.stringify({ url: bridge.url, token: bridge.token }));
  await writeFile(join(dir, 'w.secret'), 's3');
  t.after(async () => {
    await bridge.close();
    await rm(dir, { recursive: true, force: true });
  });
  const hook = (input: object, env: Record<string, string> = {}) => new Promise<number>((resolve) => {
    const child = spawn(process.execPath, [fileURLToPath(new URL('../../plugin/server.mjs', import.meta.url)), '--stop-failure'],
      { env: { ...process.env, SAMRAJYA_BRIDGE_FILE: discoveryPath, SAMRAJYA_AGENT: '', ...env }, stdio: ['pipe', 'ignore', 'ignore'] });
    child.on('exit', (code) => resolve(code ?? -1));
    child.stdin.end(JSON.stringify(input));
  });
  const message = "You've hit your 5-hour limit · resets 7:10pm (UTC)";
  const agent = { SAMRAJYA_AGENT: 'w-T3', SAMRAJYA_AGENT_SECRET_FILE: join(dir, 'w.secret') };
  assert.equal(await hook({ error: 'rate_limit', last_assistant_message: message }), 0);
  assert.equal(await hook({ error: 'overloaded', last_assistant_message: 'busy' }, agent), 0);
  assert.equal(bridge.requests.filter((r) => r.path === '/v1/limit').length, 0);
  assert.equal(await hook({ error: 'rate_limit', last_assistant_message: message, error_details: '429' }, agent), 0);
  assert.deepEqual(bridge.requests.find((r) => r.path === '/v1/limit')?.body, { message, details: '429', agent: 'w-T3', secret: 's3' });
});

test('an agent attaches with its id and secret wherever it runs, and has its role\'s tools', async (t) => {
  const { bridge, channel, client, notifications } = await setup(t, { agent: { id: 'w-T3', secret: 's3' } });
  await until(() => channel.state === 'attached', 'attached');
  const attach = bridge.requests.find((r) => r.path === '/v1/attach');
  assert.deepEqual({ ...attach?.body, client: undefined }, { client: undefined, agent: 'w-T3', secret: 's3', protocol: 3 });
  bridge.push(event('linear:c9', 9));
  await until(() => notifications.length === 1, 'the ping');
  const names = (await client.listTools()).tools.map((x) => x.name);
  assert.ok(names.includes('samrajya_report') && names.includes('samrajya_reply') && names.includes('samrajya_fetch'));
  assert.ok(!names.includes('samrajya_dispatch') && !names.includes('samrajya_create_task'));
  const r = await client.callTool({ name: 'samrajya_dispatch', arguments: { brief: 'x' } });
  assert.equal(r.isError, true);
});

test('the main agent can dispatch and see the agents', () => {
  const main = toolsFor({ id: 'main', secret: 'x' }).map((x) => x.name);
  for (const name of ['samrajya_dispatch', 'samrajya_agents', 'samrajya_message', 'samrajya_stop_agent', 'samrajya_fetch']) assert.ok(main.includes(name), name);
  assert.ok(!main.includes('samrajya_report'));
});

test('a replaced agent session stops acting', async (t) => {
  const { bridge, channel, notifications } = await setup(t, { agent: { id: 'w-T3', secret: 'stale' } });
  bridge.push(event('linear:c9', 9));
  await until(() => channel.state === 'replaced', 'replaced');
  await new Promise((r) => setTimeout(r, 300));
  assert.equal(notifications.length, 0);
  assert.ok(!bridge.requests.some((r) => r.path.startsWith('/v1/events')));
});

test('an agent reads its identity from the environment its launcher set', async (t) => {
  const dir = await mkdtemp(join(tmpdir(), 'samrajya-agent-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  await writeFile(join(dir, 'w.secret'), 's3\n');
  assert.deepEqual(await agentFromEnv({ SAMRAJYA_AGENT: 'w-T3', SAMRAJYA_AGENT_SECRET_FILE: join(dir, 'w.secret') }), { id: 'w-T3', secret: 's3' });
  assert.equal(await agentFromEnv({}), undefined);
});

test('the turn hooks report an agent\'s turns, and nothing for a session that is no agent', { timeout: 20000 }, async (t) => {
  const bridge = new FakeBridge();
  await bridge.listen();
  const dir = await mkdtemp(join(tmpdir(), 'samrajya-turn-'));
  const discoveryPath = join(dir, 'bridge.json');
  await writeFile(discoveryPath, JSON.stringify({ url: bridge.url, token: bridge.token }));
  await writeFile(join(dir, 'w.secret'), 's3');
  t.after(async () => {
    await bridge.close();
    await rm(dir, { recursive: true, force: true });
  });
  const hook = (state: string, env: Record<string, string>) => new Promise<number>((resolve) => {
    const child = spawn(process.execPath, [fileURLToPath(new URL('../../plugin/server.mjs', import.meta.url)), '--turn', state],
      { env: { ...process.env, SAMRAJYA_BRIDGE_FILE: discoveryPath, ...env }, stdio: ['pipe', 'ignore', 'ignore'] });
    child.on('exit', (code) => resolve(code ?? -1));
    child.stdin.end('{}');
  });
  assert.equal(await hook('start', { SAMRAJYA_AGENT: '' }), 0);
  assert.equal(bridge.requests.filter((r) => r.path === '/v1/turn').length, 0);
  assert.equal(await hook('start', { SAMRAJYA_AGENT: 'w-T3', SAMRAJYA_AGENT_SECRET_FILE: join(dir, 'w.secret') }), 0);
  assert.equal(await hook('end', { SAMRAJYA_AGENT: 'w-T3', SAMRAJYA_AGENT_SECRET_FILE: join(dir, 'w.secret') }), 0);
  assert.deepEqual(bridge.requests.filter((r) => r.path === '/v1/turn').map((r) => r.body), [
    { agent: 'w-T3', secret: 's3', state: 'start' }, { agent: 'w-T3', secret: 's3', state: 'end' },
  ]);
});
