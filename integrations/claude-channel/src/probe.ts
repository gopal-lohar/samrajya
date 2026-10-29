import { randomUUID, timingSafeEqual } from 'node:crypto';
import { createServer } from 'node:http';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { Server } from '@modelcontextprotocol/sdk/server/index.js';
import { CallToolRequestSchema, ListToolsRequestSchema } from '@modelcontextprotocol/sdk/types.js';

const maxEvents = 64;
const maxBodyBytes = 1024;
const idPattern = /^[a-zA-Z0-9_-]{1,64}$/;

interface ProbeEvent {
  id: string;
  attempts: number;
  transportWrites: number;
  acknowledged: boolean;
}

class RequestError extends Error {
  constructor(readonly status: number, message: string) {
    super(message);
  }
}

function reply(res: ServerResponse, status: number, body: unknown) {
  res.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  res.end(JSON.stringify(body));
}

async function readID(req: IncomingMessage): Promise<string> {
  if (req.headers['content-type']?.split(';')[0]?.trim() !== 'application/json') {
    throw new RequestError(415, 'expected application/json');
  }
  // Read as data events so a size rejection can still send an HTTP response.
  const body = await new Promise<string>((resolve, reject) => {
    let size = 0;
    const chunks: Buffer[] = [];
    req.on('data', (chunk: Buffer) => {
      size += chunk.length;
      if (size > maxBodyBytes) {
        reject(new RequestError(413, 'body too large'));
      } else {
        chunks.push(chunk);
      }
    });
    req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    req.on('error', reject);
    req.on('aborted', () => reject(new RequestError(400, 'request aborted')));
  });
  let value: unknown;
  try {
    value = JSON.parse(body);
  } catch {
    throw new RequestError(400, 'invalid JSON');
  }
  if (!value || typeof value !== 'object' || Array.isArray(value) ||
      Object.keys(value).length !== 1 || !('id' in value) ||
      typeof value.id !== 'string' || !idPattern.test(value.id)) {
    throw new RequestError(400, 'expected only an id of 1-64 letters, digits, underscores or hyphens');
  }
  return value.id;
}

export function createProbe(token: string) {
  if (!/^[a-f0-9]{64}$/.test(token)) {
    throw new Error('SAMRAJYA_PROBE_TOKEN must be 32 random bytes encoded as 64 lowercase hex characters');
  }
  const authorization = Buffer.from(`Bearer ${token}`);
  const instance = randomUUID();
  const events = new Map<string, ProbeEvent>();
  const sending = new Set<string>();
  let ready = false;

  const mcp = new Server({ name: 'samrajya-probe', version: '0.0.1' }, {
    capabilities: { experimental: { 'claude/channel': {} }, tools: {} },
    instructions:
      'This is a synthetic channel feasibility probe, not a task dispatcher. ' +
      'For a samrajya-probe channel event, call acknowledge_probe with its event_id. ' +
      'Do not run commands, edit files or contact external services in response to probe events. ' +
      'Acknowledgement records receipt in memory only, not task completion or durable delivery.',
  });
  mcp.oninitialized = () => { ready = true; };
  mcp.onclose = () => { ready = false; };
  mcp.setRequestHandler(ListToolsRequestSchema, async () => ({
    tools: [{
      name: 'acknowledge_probe',
      description: 'Acknowledge receipt of a synthetic probe event. In-memory receipt only; not task completion.',
      inputSchema: {
        type: 'object',
        properties: { event_id: { type: 'string', pattern: idPattern.source } },
        required: ['event_id'],
        additionalProperties: false,
      },
    }],
  }));
  mcp.setRequestHandler(CallToolRequestSchema, async (req) => {
    const args = req.params.arguments;
    const id = args?.event_id;
    if (req.params.name !== 'acknowledge_probe' || !args || Object.keys(args).length !== 1 ||
        typeof id !== 'string' || !idPattern.test(id)) {
      return { isError: true, content: [{ type: 'text', text: 'invalid tool or event_id' }] };
    }
    const event = events.get(id);
    if (!event || event.attempts === 0) {
      return { isError: true, content: [{ type: 'text', text: 'unknown event_id' }] };
    }
    event.acknowledged = true;
    return { content: [{ type: 'text', text: JSON.stringify({ instance, ...event }) }] };
  });

  async function handle(req: IncomingMessage, res: ServerResponse) {
    const provided = Buffer.from(req.headers.authorization ?? '');
    if (provided.length !== authorization.length || !timingSafeEqual(provided, authorization)) {
      reply(res, 401, { error: 'unauthorized' });
      return;
    }
    // No browser/CORS entry point: this probe is driven by a local operator.
    if (req.headers.origin) {
      reply(res, 403, { error: 'browser origins are not allowed' });
      return;
    }
    if (req.method === 'GET' && req.url === '/events') {
      reply(res, 200, { instance, ready, durable: false, events: [...events.values()] });
      return;
    }
    if (req.method !== 'POST' || req.url !== '/events') {
      reply(res, 404, { error: 'use GET or POST /events' });
      return;
    }
    const id = await readID(req);
    if (!ready) {
      throw new RequestError(503, 'MCP client is not initialized');
    }
    let event = events.get(id);
    if (!event) {
      if (events.size >= maxEvents) {
        throw new RequestError(429, 'probe capacity reached; restart for a new experiment');
      }
      event = { id, attempts: 0, transportWrites: 0, acknowledged: false };
      events.set(id, event);
    }
    if (event.acknowledged) {
      reply(res, 200, { instance, ...event });
      return;
    }
    if (sending.has(id)) {
      throw new RequestError(409, 'notification already in progress for this id');
    }
    sending.add(id);
    event.attempts++;
    try {
      await mcp.notification({
        method: 'notifications/claude/channel',
        params: {
          content: 'Synthetic receipt test only. Acknowledge the event_id with acknowledge_probe; take no other action.',
          meta: { event_id: id, probe_instance: instance },
        },
      });
      event.transportWrites++;
    } catch {
      throw new RequestError(503, 'transport write failed; receipt is unknown, retry this same id');
    } finally {
      sending.delete(id);
    }
    reply(res, 202, { instance, ...event });
  }

  const http = createServer((req, res) => {
    void handle(req, res).catch((error: unknown) => {
      reply(res, error instanceof RequestError ? error.status : 500, {
        error: error instanceof RequestError ? error.message : 'probe request failed',
      });
    });
  });
  http.requestTimeout = 5000;
  http.headersTimeout = 5000;
  http.setTimeout(5000, (socket) => socket.destroy());

  return {
    mcp,
    async listen(port = 0): Promise<number> {
      await new Promise<void>((resolve, reject) => {
        http.once('error', reject);
        http.listen(port, '127.0.0.1', () => {
          http.off('error', reject);
          resolve();
        });
      });
      const address = http.address();
      if (!address || typeof address === 'string') throw new Error('missing probe address');
      return address.port;
    },
    async close() {
      ready = false;
      const closed = new Promise<void>((resolve) => http.close(() => resolve()));
      http.closeAllConnections();
      await closed;
      await mcp.close();
    },
  };
}
