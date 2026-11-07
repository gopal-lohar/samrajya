import assert from 'node:assert/strict';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
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
import { agentFromEnv, Bridge, type Agent } from '../src/bridge.js';
import { createChannel, instructions, notAnAgent, toolsFor } from '../src/channel.js';
import { waitForEvents } from '../src/hook.js';

const MAIN: Agent = { id: 'main', secret: 'm' };
const server = fileURLToPath(new URL('../../plugin/server.mjs', import.meta.url));

/** A stand-in for mahamantri's bridge API. */
class FakeBridge {
  token = 'tok-1';
  holder = '';
  gen = 0;
  /** Messages waiting for the agent; a waiter takes them all. */
  waiting: string[] = [];
  superseded = false;
  requests: { method: string; path: string; attachment?: string; body?: any }[] = [];
  private wake: (() => void)[] = [];
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
    for (const w of this.wake) w();
    this.http.closeAllConnections();
    await new Promise<void>((resolve) => this.http.close(() => resolve()));
  }

  say(text: string) {
    this.waiting.push(text);
    for (const w of this.wake.splice(0)) w();
  }

  private sleep(ms: number) {
    return new Promise<void>((resolve) => {
      const t = setTimeout(resolve, ms);
      this.wake.push(() => { clearTimeout(t); resolve(); });
    });
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
    if (req.method === 'POST' && url.pathname === '/v1/attach') {
      if (!body.agent) return send(409, { error: 'not an agent' });
      if (body.secret === 'stale') return send(403, { error: 'stale secret' });
      this.holder = `att-${++this.gen}`;
      return send(200, { attachment: this.holder, generation: this.gen });
    }
    if (url.pathname === '/v1/wait') {
      if (body.secret === 'stale') return send(403, { error: 'stale secret' });
      if (this.superseded) return send(200, { superseded: true });
      if (!this.waiting.length) await this.sleep(Math.min(body.seconds, 2) * 1000);
      const text = this.waiting.splice(0).join('\n\n');
      return send(200, text ? { events: 1, text } : { events: 0 });
    }
    if (url.pathname === '/v1/limit') return send(200, { until: '2026-11-02T19:10:00Z' });
    if (url.pathname === '/v1/turn') return send(200, { ok: true });
    if (url.pathname === '/v1/status') return send(200, { main: { live: !!this.holder }, recovery: { pending: [] } });
    if (!attachment || attachment !== this.holder) return send(409, { error: 'not attached' });
    if (url.pathname === '/v1/events') {
      await this.sleep(500);
      if (attachment !== this.holder) return send(409, { error: 'replaced' });
      return send(200, { events: [] });
    }
    if (url.pathname === '/v1/detach') {
      this.holder = '';
      return send(200, { detached: true });
    }
    if (url.pathname === '/v1/accept') return send(200, { id: 'T1', status: 'queued' });
    if (url.pathname.startsWith('/v1/tasks/') && req.method === 'POST' && !body.result && body.status === 'done') {
      return send(400, { error: 'a task is done only with a result' });
    }
    return send(200, { ok: true, path: url.pathname });
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
  return { dir, discoveryPath, bridge, channel, client, call, writeDiscovery };
}

async function until(cond: () => boolean, what: string, ms = 5000) {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 20));
  }
}

/** Runs the bundled server as a hook, the way Claude Code does. */
function hook(args: string[], env: Record<string, string>, stdin = '{}') {
  return new Promise<{ code: number; stderr: string }>((resolve) => {
    const child = spawn(process.execPath, [server, ...args], { env: { ...process.env, SAMRAJYA_AGENT: '', ...env }, stdio: ['pipe', 'ignore', 'pipe'] });
    let stderr = '';
    child.stderr.on('data', (d) => { stderr += d; });
    child.on('exit', (code) => resolve({ code: code ?? -1, stderr }));
    child.stdin.end(stdin);
  });
}

async function hookDir(t: TestContext, bridge: FakeBridge) {
  await bridge.listen();
  const dir = await mkdtemp(join(tmpdir(), 'samrajya-hook-'));
  const discoveryPath = join(dir, 'bridge.json');
  await writeFile(discoveryPath, JSON.stringify({ url: bridge.url, token: bridge.token }));
  await writeFile(join(dir, 'w.secret'), 's3');
  t.after(async () => {
    await bridge.close();
    await rm(dir, { recursive: true, force: true });
  });
  return { discoveryPath, agent: { SAMRAJYA_BRIDGE_FILE: discoveryPath, SAMRAJYA_AGENT: 'w-T3', SAMRAJYA_AGENT_SECRET_FILE: join(dir, 'w.secret') } };
}

test('instructions explain how messages arrive; a session that is no agent is told so', () => {
  assert.match(instructions, /"Samrajya: new messages" wake-ups/);
  assert.match(instructions, /samrajya_reply in the message's conversation and thread/);
  assert.match(notAnAgent, /not one of Samrajya's agents/);
});

test('the main session has the task, reply and agent tools', async (t) => {
  const { client } = await setup(t, { start: false });
  const names = (await client.listTools()).tools.map((x) => x.name);
  assert.deepEqual(names, ['samrajya_status', 'samrajya_accept', 'samrajya_create_task', 'samrajya_update_task',
    'samrajya_tasks', 'samrajya_reply', 'samrajya_action_intent', 'samrajya_action_result', 'samrajya_memory', 'samrajya_remember',
    'samrajya_forget', 'samrajya_agents', 'samrajya_dispatch', 'samrajya_fetch', 'samrajya_message', 'samrajya_stop_agent']);
  assert.match(client.getInstructions() ?? '', /Samrajya's tools/);
  assert.equal(client.getServerCapabilities()?.experimental, undefined);
});

test('tools send the attachment and map arguments; bridge errors come back as tool errors', async (t) => {
  const { call, bridge, channel } = await setup(t);
  await until(() => channel.state === 'attached', 'attach');
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

test('the attachment is kept alive by polling, and pushes nothing into the session', async (t) => {
  const { bridge, channel } = await setup(t);
  await until(() => bridge.requests.filter((r) => r.path.startsWith('/v1/events')).length >= 2, 'two polls');
  assert.equal(channel.state, 'attached');
  assert.ok(bridge.requests.filter((r) => r.path.startsWith('/v1/events')).every((r) => r.attachment === 'att-1'));
});

test('a session that is no agent does not attach and cannot act', async (t) => {
  const { call, channel, client, bridge } = await setup(t, { agent: null });
  await new Promise((r) => setTimeout(r, 300));
  assert.equal(channel.state, 'not an agent');
  assert.ok(!bridge.requests.some((r) => r.path === '/v1/attach'));
  assert.deepEqual((await client.listTools()).tools.map((x) => x.name), ['samrajya_status', 'samrajya_memory']);
  assert.equal((await call('samrajya_status')).isError, undefined);
});

test('releases the attachment when it stops, so the next session attaches at once', async (t) => {
  const { bridge, channel } = await setup(t);
  await until(() => channel.state === 'attached', 'attach');
  await channel.stop();
  assert.ok(bridge.requests.some((r) => r.path === '/v1/detach' && r.attachment === 'att-1'));
  assert.equal(bridge.holder, '');
});

test('attaches again after mahamantri restarts with a new token', async (t) => {
  const { bridge, channel, writeDiscovery } = await setup(t);
  await until(() => channel.state === 'attached', 'attach');
  bridge.token = 'tok-2'; // mahamantri restarted: new token, attachments gone
  bridge.holder = '';
  await writeDiscovery();
  await until(() => bridge.requests.filter((r) => r.path === '/v1/attach').length >= 2 && channel.state === 'attached', 'reattach', 8000);
});

test('waits for mahamantri to come up and says so meanwhile', async (t) => {
  const { discoveryPath, call, writeDiscovery } = await setup(t, { start: false });
  await rm(discoveryPath);
  const status = await call('samrajya_status');
  assert.equal(status.isError, true);
  assert.match(JSON.stringify(status.content), /mahamantri is not running/);
  await writeDiscovery();
  assert.equal((await call('samrajya_status')).isError, undefined);
});

test('an agent attaches with its id and secret, and has its role\'s tools', async (t) => {
  const { bridge, channel, client } = await setup(t, { agent: { id: 'w-T3', secret: 's3' } });
  await until(() => channel.state === 'attached', 'attached');
  const attach = bridge.requests.find((r) => r.path === '/v1/attach');
  assert.deepEqual({ ...attach?.body, client: undefined }, { client: undefined, agent: 'w-T3', secret: 's3', protocol: 3 });
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
  const { bridge, channel } = await setup(t, { agent: { id: 'w-T3', secret: 'stale' } });
  await until(() => channel.state === 'replaced', 'replaced');
  assert.ok(!bridge.requests.some((r) => r.path.startsWith('/v1/events')));
});

test('an agent reads its identity from the environment its launcher set', async (t) => {
  const dir = await mkdtemp(join(tmpdir(), 'samrajya-agent-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  await writeFile(join(dir, 'w.secret'), 's3\n');
  assert.deepEqual(await agentFromEnv({ SAMRAJYA_AGENT: 'w-T3', SAMRAJYA_AGENT_SECRET_FILE: join(dir, 'w.secret') }), { id: 'w-T3', secret: 's3' });
  assert.equal(await agentFromEnv({}), undefined);
});

test('the waiter wakes the session with new messages, and only then', async (t) => {
  const bridge = new FakeBridge();
  const { discoveryPath } = await hookDir(t, bridge);
  const agent = { id: 'w-T3', secret: 's3' };
  let written = '';
  const waiting = waitForEvents(new Bridge(discoveryPath), agent, (s) => { written = s; }, { seconds: 1, retryMs: 50 });
  await new Promise((r) => setTimeout(r, 1500)); // a poll or two with nothing
  assert.equal(written, '');
  bridge.say('[event telegram:7:11] Telegram: Asha wrote');
  assert.equal(await waiting, 2);
  assert.match(written, /Asha wrote/);
  bridge.superseded = true;
  assert.equal(await waitForEvents(new Bridge(discoveryPath), agent, () => {}, { seconds: 1 }), 0);
  assert.equal(await waitForEvents(new Bridge(discoveryPath), { id: 'w-T3', secret: 'stale' }, () => {}, { seconds: 1 }), 0);
  assert.equal(await waitForEvents(new Bridge(discoveryPath), undefined, () => {}), 0);
});

test('the bundled waiter hook reports the turn\'s end, then exits 2 with the messages', { timeout: 20000 }, async (t) => {
  const bridge = new FakeBridge();
  const { agent } = await hookDir(t, bridge);
  bridge.say('[event chat:1:1] hello there');
  const out = await hook(['--wait'], agent);
  assert.equal(out.code, 2);
  assert.match(out.stderr, /^Samrajya: new messages for you\.\n\n\[event chat:1:1\] hello there/);
  const turn = bridge.requests.findIndex((r) => r.path === '/v1/turn');
  const wait = bridge.requests.findIndex((r) => r.path === '/v1/wait');
  assert.ok(turn >= 0 && turn < wait, 'turn end reported before waiting');
  assert.deepEqual(bridge.requests[turn]?.body, { agent: 'w-T3', secret: 's3', state: 'end' });
  assert.equal((await hook(['--wait'], { ...agent, SAMRAJYA_AGENT: '' })).code, 0); // no agent: nothing to wait for
});

test('the StopFailure hook reports an agent hitting the usage limit, and nothing else', { timeout: 20000 }, async (t) => {
  const bridge = new FakeBridge();
  const { agent, discoveryPath } = await hookDir(t, bridge);
  const message = "You've hit your 5-hour limit · resets 7:10pm (UTC)";
  assert.equal((await hook(['--stop-failure'], { SAMRAJYA_BRIDGE_FILE: discoveryPath }, JSON.stringify({ error: 'rate_limit', last_assistant_message: message }))).code, 0);
  assert.equal((await hook(['--stop-failure'], agent, JSON.stringify({ error: 'overloaded', last_assistant_message: 'busy' }))).code, 0);
  assert.equal(bridge.requests.filter((r) => r.path === '/v1/limit').length, 0);
  assert.equal((await hook(['--stop-failure'], agent, JSON.stringify({ error: 'rate_limit', last_assistant_message: message, error_details: '429' }))).code, 0);
  assert.deepEqual(bridge.requests.find((r) => r.path === '/v1/limit')?.body, { message, details: '429', agent: 'w-T3', secret: 's3' });
});

test('the turn-start hook reports an agent\'s turns, and nothing for a session that is no agent', { timeout: 20000 }, async (t) => {
  const bridge = new FakeBridge();
  const { agent, discoveryPath } = await hookDir(t, bridge);
  assert.equal((await hook(['--turn', 'start'], { SAMRAJYA_BRIDGE_FILE: discoveryPath })).code, 0);
  assert.equal(bridge.requests.filter((r) => r.path === '/v1/turn').length, 0);
  assert.equal((await hook(['--turn', 'start'], agent)).code, 0);
  assert.deepEqual(bridge.requests.filter((r) => r.path === '/v1/turn').map((r) => r.body), [{ agent: 'w-T3', secret: 's3', state: 'start' }]);
});

test('the bundled plugin server starts over stdio and attaches as its agent', { timeout: 20000 }, async (t) => {
  const bridge = new FakeBridge();
  const { agent } = await hookDir(t, bridge);
  const transport = new StdioClientTransport({ command: process.execPath, args: [server], env: agent, stderr: 'pipe' });
  const client = new Client({ name: 'stdio-test', version: '0.0.1' });
  t.after(() => client.close());
  await client.connect(transport);
  await until(() => bridge.holder !== '', 'attach over stdio');
  assert.ok((await client.listTools()).tools.some((x) => x.name === 'samrajya_reply'));
});
