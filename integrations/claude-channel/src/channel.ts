import { Server } from '@modelcontextprotocol/sdk/server/index.js';
import { CallToolRequestSchema, ListToolsRequestSchema } from '@modelcontextprotocol/sdk/types.js';
import { Bridge, BridgeError, type Agent } from './bridge.js';

// The Samrajya plugin's MCP server: runs inside each of Samrajya's sessions
// (the main and every worker), keeps the session attached to mahamantri as
// its agent, and gives it tools for its tasks, replies and the action
// journal. New messages do not come through here: the plugin's waiter hook
// (hook.ts, waitForEvents) hands them to the session when they arrive.

export const instructions = `Samrajya's tools. Messages routed to you arrive as "Samrajya: new messages" wake-ups: from people on Samrajya's surfaces (Linear, Telegram, ...), from other agents, and notices from Samrajya itself. Each names its conversation and thread. Accept each with samrajya_accept before acting on it (a worker: with its task_id). Reply with samrajya_reply in the message's conversation and thread - it posts where the person wrote, journaled. Keep your task current with samrajya_update_task. After a restart or compaction, call samrajya_status first: it lists what is pending for you, your open tasks and actions whose outcome was never recorded (check before redoing one). Your role, in your system prompt, says how to work.`;

export const notAnAgent = `Samrajya's tools. This session is not one of Samrajya's agents (it was not launched by mahamantri), so it receives nothing and cannot act for Samrajya. samrajya_status shows where things stand.`;

const str = (description: string) => ({ type: 'string', description });

const tools = [
  {
    name: 'samrajya_status',
    description: 'What is pending for you, open tasks, unresolved external actions, the surfaces you can reply on, and whether the main session is attached. Call after a restart or compaction.',
    inputSchema: { type: 'object', properties: {}, additionalProperties: false },
  },
  {
    name: 'samrajya_accept',
    description: 'Accept messages so they are not handed to you again: into a task for work that spans time - an existing one (task_id) or a new one (new_task_title, the main only), with next_action - or with the event ids alone for a message you answer on the spot, or a notice. Idempotent.',
    inputSchema: {
      type: 'object',
      properties: {
        event_ids: { type: 'array', items: { type: 'string' }, minItems: 1 },
        task_id: str('Existing task this follows up on'),
        new_task_title: str('Title for a new task'),
        project: str('Workspace project the new task is in, if known'),
        next_action: str('What happens next'),
      },
      required: ['event_ids'],
      additionalProperties: false,
    },
  },
  {
    name: 'samrajya_create_task',
    description: 'Create a task for work not started by a message, e.g. something the person asked in the terminal.',
    inputSchema: {
      type: 'object',
      properties: {
        title: str('What the task is'),
        links: { type: 'array', items: { type: 'string' }, description: 'References: an issue key, a PR or doc URL' },
        project: str('Workspace project'),
      },
      required: ['title'],
      additionalProperties: false,
    },
  },
  {
    name: 'samrajya_update_task',
    description: 'Update a task: status (queued, working, waiting_approval, blocked, paused, done, cancelled), next action, checkpoint, result. done requires a result.',
    inputSchema: {
      type: 'object',
      properties: {
        task_id: str('Task id'),
        status: { type: 'string', enum: ['queued', 'working', 'waiting_approval', 'blocked', 'paused', 'done', 'cancelled'] },
        next_action: str('What happens next'),
        checkpoint: str('Where things stand, enough to resume from'),
        result: str('What was achieved and how it was verified'),
      },
      required: ['task_id'],
      additionalProperties: false,
    },
  },
  {
    name: 'samrajya_tasks',
    description: 'List tasks (open ones unless all), or get one by id.',
    inputSchema: { type: 'object', properties: { all: { type: 'boolean' }, task_id: str('One task to show') }, additionalProperties: false },
  },
  {
    name: 'samrajya_reply',
    description: 'Post in a conversation as Samrajya, journaled, with files if you like: a Linear issue (a comment, in a thread), a Telegram chat (a message, replying to one). The way to answer anyone on a surface. Give body, files or both - or react alone, to put an emoji reaction on the message thread_id names (a plain acknowledgement: "good", "ok", "thanks"). A worker may reply only in its task\'s conversations, and by default in the first of them.',
    inputSchema: {
      type: 'object',
      properties: {
        conversation: str('Where: the event\'s conversation, e.g. linear:<issue uuid or key> or telegram:<chat id>. A worker may omit it.'),
        thread_id: str('Within it: the event\'s thread (a Linear comment thread, the Telegram message to reply to). Omit to start a new one.'),
        body: str('The message (Markdown on Linear, plain text on Telegram)'),
        files: { type: 'array', items: { type: 'string' }, description: 'Absolute paths of files to send with it (screenshots, documents; up to 10, 50 MB each): photos or documents on Telegram, attachments linked in the comment on Linear' },
        react: str('Instead of a message: an emoji to put on the message thread_id names (a Telegram message id, a Linear comment id), e.g. 👍. Telegram takes only its own reaction emoji.'),
        task_id: str('Task it is for'),
      },
      additionalProperties: false,
    },
  },
  {
    name: 'samrajya_action_intent',
    description: 'Record a significant external action, such as an issue status change or a deploy, before doing it. Replies go through samrajya_reply instead.',
    inputSchema: {
      type: 'object',
      properties: { task_id: str('Task it is for'), kind: str('e.g. linear_comment, linear_status'), target: str('Exactly where, e.g. issue and thread'), intent: str('What it will say or change') },
      required: ['kind', 'target', 'intent'],
      additionalProperties: false,
    },
  },
  {
    name: 'samrajya_action_result',
    description: 'Record how an intended action turned out: done or failed, with the id of what it created (ref).',
    inputSchema: {
      type: 'object',
      properties: {
        action_id: str('From samrajya_action_intent'),
        status: { type: 'string', enum: ['done', 'failed'] },
        ref: str('Id of what was created, e.g. the comment id'),
        note: str('Anything worth knowing'),
        thread_id: str('Thread the comment is in'),
        issue: str('Issue identifier'),
      },
      required: ['action_id', 'status'],
      additionalProperties: false,
    },
  },
  {
    name: 'samrajya_memory',
    description: "Samrajya's memory: without a name, list the notes; with one, read it.",
    inputSchema: { type: 'object', properties: { name: str('Note to read') }, additionalProperties: false },
  },
  {
    name: 'samrajya_remember',
    description: 'Save or update a memory note (short: a standing instruction or a fact in neither the code nor Linear).',
    inputSchema: {
      type: 'object',
      properties: {
        name: str('kebab-case note name'),
        description: str('One line saying what the note is about'),
        project: str('Workspace project it applies to; omit for everywhere'),
        body: str('The note'),
      },
      required: ['name', 'description', 'body'],
      additionalProperties: false,
    },
  },
  {
    name: 'samrajya_forget',
    description: 'Delete a memory note that is wrong or no longer useful.',
    inputSchema: { type: 'object', properties: { name: str('Note to delete') }, required: ['name'], additionalProperties: false },
  },
  {
    name: 'samrajya_agents',
    description: 'Every agent (the main and a worker per task): its task, state (idle, starting, live, failed), whether it is mid-step, and its last report.',
    inputSchema: { type: 'object', properties: {}, additionalProperties: false },
  },
  {
    name: 'samrajya_dispatch',
    description: 'Give work to a worker session of its own, which starts at once (or when a slot frees), works in parallel with everything else and reports back. For anything that takes more than a few minutes or changes files. With a conversation the worker reports there; with route, that conversation\'s new messages go to the worker too (the default for a Linear issue, not for a chat).',
    inputSchema: {
      type: 'object',
      properties: {
        title: str('Short title of the task'),
        brief: str('What to do, what done looks like, any limits'),
        conversation: str('Where the worker reports, e.g. the event\'s conversation'),
        thread_id: str('Thread within it to report in'),
        route: { type: 'boolean', description: 'The conversation\'s new messages go to the worker (default: true for Linear issues, false for chats)' },
        links: { type: 'array', items: { type: 'string' }, description: 'References for the worker: an issue key, a PR or doc URL' },
        event_ids: { type: 'array', items: { type: 'string' }, description: 'The messages that asked for this work: they go into the worker\'s task' },
        project: str('Workspace project'),
      },
      required: ['brief'],
      additionalProperties: false,
    },
  },
  {
    name: 'samrajya_fetch',
    description: 'Save a file a message carried (a voice note, a photo, a document) to a local path you can read.',
    inputSchema: {
      type: 'object',
      properties: { event_id: str('The event it came with'), ref: str('The attachment ref the event gave') },
      required: ['event_id', 'ref'],
      additionalProperties: false,
    },
  },
  {
    name: 'samrajya_restart',
    description:
      'Ask the person to approve a change to the running Samrajya: deploy a commit of its codebase (kind deploy, with ref), ' +
      'restart Samrajya (restart) or replace the main session (restart_main). The request is posted in the conversation; ' +
      'mahamantri carries it out only once a trusted person answers "approve R<n>" there, builds and tests a deploy first, ' +
      'puts the running release back if Samrajya does not come up, and tells you how it went. Never restart or redeploy Samrajya yourself.',
    inputSchema: {
      type: 'object',
      properties: {
        kind: { type: 'string', enum: ['deploy', 'restart', 'restart_main'] },
        ref: str('deploy: the commit or branch of the codebase, committed and tested'),
        reason: str('What it changes and why, how you verified it - the person decides on this'),
        conversation: str("Where to ask (default: your task's conversation; for the main, the home conversation)"),
        thread_id: str('The thread to ask in'),
        task_id: str('The task it belongs to'),
      },
      required: ['kind', 'reason'],
      additionalProperties: false,
    },
  },
  {
    name: 'samrajya_message',
    description: 'Send text to another agent; it arrives on their Samrajya channel when their current step ends. A worker can message only the main.',
    inputSchema: { type: 'object', properties: { to: str('Agent id (default: main)'), body: str('The message') }, required: ['body'], additionalProperties: false },
  },
  {
    name: 'samrajya_stop_agent',
    description: 'Stop a worker at once; its task is paused until a new event or dispatch.',
    inputSchema: { type: 'object', properties: { agent: str('Agent id, e.g. w-T3') }, required: ['agent'], additionalProperties: false },
  },
  {
    name: 'samrajya_report',
    description: 'Report on your task to the main session: progress, done, blocked or needs_decision. Blocked and needs_decision reach it at once; the rest show in samrajya_agents.',
    inputSchema: {
      type: 'object',
      properties: { kind: { type: 'string', enum: ['progress', 'done', 'blocked', 'needs_decision'] }, body: str('What happened, with evidence') },
      required: ['kind', 'body'],
      additionalProperties: false,
    },
  },
];

const workerTools = new Set(['samrajya_status', 'samrajya_accept', 'samrajya_update_task', 'samrajya_tasks', 'samrajya_reply',
  'samrajya_action_intent', 'samrajya_action_result', 'samrajya_memory', 'samrajya_remember', 'samrajya_forget', 'samrajya_report',
  'samrajya_message', 'samrajya_fetch', 'samrajya_restart']);

/** The tools a session has: the main agent's, a worker's, or (no agent) status alone. */
export function toolsFor(agent?: Agent) {
  if (!agent) return tools.filter((t) => t.name === 'samrajya_status' || t.name === 'samrajya_memory');
  if (agent.id === 'main') return tools.filter((t) => t.name !== 'samrajya_report');
  return tools.filter((t) => workerTools.has(t.name));
}

export interface ChannelOptions {
  /** This session's agent identity; none for a session mahamantri did not launch. */
  agent?: Agent;
  discoveryPath: string;
  clientName?: string;
  pollSeconds?: number;
  maxBackoffMs?: number;
}

export function createChannel(opts: ChannelOptions) {
  const bridge = new Bridge(opts.discoveryPath);
  const client = opts.clientName ?? `claude-channel pid ${process.pid}`;
  let state: 'starting' | 'attached' | 'not an agent' | 'disconnected' | 'replaced' = opts.agent ? 'starting' : 'not an agent';
  let lastError = '';
  let stopped = false;
  let running: Promise<void> | undefined;
  const abort = new AbortController();

  const agent = opts.agent;
  const mcp = new Server({ name: 'samrajya', version: '0.6.0' }, {
    capabilities: { tools: {} },
    instructions: agent ? instructions : notAnAgent,
  });

  let wake = () => {};
  const sleep = (ms: number) => new Promise<void>((resolve) => {
    const done = () => { clearTimeout(t); wake = () => {}; resolve(); };
    const t = setTimeout(done, ms);
    wake = done;
    abort.signal.addEventListener('abort', done, { once: true });
  });

  // attachment keeps this session attached as its agent: mahamantri tells a
  // live session from a lost one by these requests, and only an attached
  // session's tool calls are accepted.
  async function attachment() {
    if (!agent) return; // nothing is routed to a session that is no agent
    let backoff = 1000;
    while (!stopped) {
      let used = bridge.attachment;
      try {
        if (!bridge.attachment) {
          await bridge.attach(client, agent);
          state = 'attached';
          used = bridge.attachment;
        }
        // A long poll for nothing: it keeps the attachment fresh.
        await bridge.request('GET', `/v1/events?after=${Number.MAX_SAFE_INTEGER}&wait=${opts.pollSeconds ?? 25}`, undefined,
          AbortSignal.any([abort.signal, AbortSignal.timeout(((opts.pollSeconds ?? 25) + 10) * 1000)]));
        backoff = 1000;
      } catch (error) {
        if (stopped) return;
        if (error instanceof BridgeError && (error.status === 403 || error.status === 426) && !bridge.attachment) {
          // This launch was replaced or stopped (or the plugin is too old):
          // it must not act. Check again now and then, quietly.
          state = 'replaced';
          lastError = error.message;
          await sleep(opts.maxBackoffMs ?? 60000);
          continue;
        }
        if (error instanceof BridgeError && error.status === 409) {
          // The attachment lapsed (mahamantri restarted, or a long pause):
          // attach again, which a replaced launch's stale secret cannot.
          if (bridge.attachment === used) bridge.attachment = '';
          continue;
        }
        state = 'disconnected';
        lastError = error instanceof Error ? error.message : String(error);
        await sleep(backoff);
        backoff = Math.min(backoff * 2, opts.maxBackoffMs ?? 30000);
      }
    }
  }

  const text = (v: unknown) => ({ content: [{ type: 'text' as const, text: typeof v === 'string' ? v : JSON.stringify(v, null, 2) }] });
  const fail = (msg: string) => ({ isError: true, content: [{ type: 'text' as const, text: msg }] });

  async function call(name: string, a: Record<string, any>) {
    switch (name) {
      case 'samrajya_status':
        return { channel: { state, lastError: lastError || undefined }, ...(await bridge.request('GET', '/v1/status')) };
      case 'samrajya_accept':
        return bridge.request('POST', '/v1/accept', {
          events: a.event_ids,
          taskId: a.task_id,
          newTask: a.new_task_title ? { title: a.new_task_title, project: a.project } : undefined,
          nextAction: a.next_action,
        });
      case 'samrajya_create_task':
        return bridge.request('POST', '/v1/tasks', { title: a.title, links: a.links, project: a.project });
      case 'samrajya_update_task':
        return bridge.request('POST', `/v1/tasks/${encodeURIComponent(a.task_id)}`, { status: a.status, nextAction: a.next_action, checkpoint: a.checkpoint, result: a.result });
      case 'samrajya_tasks':
        return a.task_id ? bridge.request('GET', `/v1/tasks/${encodeURIComponent(a.task_id)}`) : bridge.request('GET', `/v1/tasks${a.all ? '?all=1' : ''}`);
      case 'samrajya_reply':
        return bridge.request('POST', '/v1/reply', { conversation: a.conversation, threadId: a.thread_id, body: a.body, files: a.files, react: a.react, taskId: a.task_id });
      case 'samrajya_action_intent':
        return bridge.request('POST', '/v1/actions', { taskId: a.task_id, kind: a.kind, target: a.target, intent: a.intent });
      case 'samrajya_action_result':
        return bridge.request('POST', `/v1/actions/${encodeURIComponent(a.action_id)}`, { status: a.status, ref: a.ref, note: a.note, threadId: a.thread_id, issue: a.issue });
      case 'samrajya_memory':
        return a.name ? bridge.request('GET', `/brain/${encodeURIComponent(a.name)}`) : bridge.request('GET', '/brain');
      case 'samrajya_remember':
        return bridge.request('PUT', `/brain/${encodeURIComponent(a.name)}`, { description: a.description, project: a.project, body: a.body });
      case 'samrajya_forget':
        await bridge.request('DELETE', `/brain/${encodeURIComponent(a.name)}`);
        return `Forgot ${a.name}.`;
      case 'samrajya_agents':
        return bridge.request('GET', '/v1/agents');
      case 'samrajya_dispatch':
        return bridge.request('POST', '/v1/dispatch', {
          title: a.title, brief: a.brief, project: a.project, conversation: a.conversation, threadId: a.thread_id, route: a.route, links: a.links, events: a.event_ids,
        });
      case 'samrajya_fetch':
        return bridge.request('POST', '/v1/fetch', { eventId: a.event_id, ref: a.ref });
      case 'samrajya_restart':
        return bridge.request('POST', '/v1/restart', { kind: a.kind, ref: a.ref, reason: a.reason, conversation: a.conversation, threadId: a.thread_id, taskId: a.task_id });
      case 'samrajya_message':
        return bridge.request('POST', '/v1/message', { to: a.to, body: a.body });
      case 'samrajya_stop_agent':
        return bridge.request('POST', `/v1/agents/${encodeURIComponent(a.agent)}/stop`);
      case 'samrajya_report':
        return bridge.request('POST', '/v1/report', { kind: a.kind, body: a.body });
      default:
        throw new Error(`unknown tool ${name}`);
    }
  }

  const mine = toolsFor(agent);
  mcp.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: mine }));
  mcp.setRequestHandler(CallToolRequestSchema, async (req) => {
    if (!mine.some((t) => t.name === req.params.name)) return fail(`unknown tool ${req.params.name}`);
    try {
      return text(await call(req.params.name, req.params.arguments ?? {}));
    } catch (error) {
      if (error instanceof BridgeError && error.status === 409) {
        return fail(agent
          ? 'This session is not attached to Samrajya (it may have been replaced or stopped, or mahamantri just restarted), so it cannot act. Try again in a moment.'
          : 'This session is not one of Samrajya\'s agents, so it cannot act for Samrajya.');
      }
      return fail(error instanceof Error ? error.message : String(error));
    }
  });

  return {
    mcp,
    get state() { return state; },
    start() { running ??= attachment(); },
    async stop() {
      stopped = true;
      abort.abort();
      await running;
      if (bridge.attachment) {
        // Let the next session attach at once instead of after the bridge's timeout.
        await bridge.request('POST', '/v1/detach', undefined, AbortSignal.timeout(2000)).catch(() => {});
      }
      await mcp.close();
    },
  };
}
