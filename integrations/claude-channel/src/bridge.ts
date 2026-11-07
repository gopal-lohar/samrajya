import { readFile } from 'node:fs/promises';

// Client for mahamantri's bridge API (mahamantri/bridge). Mahamantri
// writes its address and a fresh token to the discovery file on every
// start, so the file is reread whenever the bridge stops answering or
// rejects the token.

export interface Discovery {
  url: string;
  token: string;
}

/** The bridge protocol this plugin speaks (mahamantri's bridge.ProtocolVersion). */
export const PROTOCOL = 3;

/** This session's identity as one of Samrajya's agents. */
export interface Agent {
  id: string;
  secret: string;
}

/** The agent this session was launched as, from the environment its launcher set; undefined for any other session. */
export async function agentFromEnv(env: NodeJS.ProcessEnv = process.env): Promise<Agent | undefined> {
  const id = env.SAMRAJYA_AGENT;
  const file = env.SAMRAJYA_AGENT_SECRET_FILE;
  if (!id || !file) return undefined;
  try {
    return { id, secret: (await readFile(file, 'utf8')).trim() };
  } catch {
    return { id, secret: '' };
  }
}

export class BridgeError extends Error {
  constructor(readonly status: number, message: string, readonly body?: unknown) {
    super(message);
  }
}

export class Bridge {
  attachment = '';
  private discovery: Discovery | undefined;

  constructor(private readonly discoveryPath: string) {}

  private async locate(): Promise<Discovery> {
    if (!this.discovery) {
      let raw: string;
      try {
        raw = await readFile(this.discoveryPath, 'utf8');
      } catch {
        throw new BridgeError(0, `mahamantri is not running (no ${this.discoveryPath})`);
      }
      const d = JSON.parse(raw) as Partial<Discovery>;
      if (typeof d.url !== 'string' || typeof d.token !== 'string') {
        throw new BridgeError(0, `${this.discoveryPath} is not a bridge discovery file`);
      }
      this.discovery = { url: d.url, token: d.token };
    }
    return this.discovery;
  }

  /** Forget the discovery file (and so the attachment), e.g. after mahamantri restarted. */
  reset() {
    this.discovery = undefined;
    this.attachment = '';
  }

  async request<T = any>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
    const d = await this.locate();
    let res: Response;
    try {
      res = await fetch(d.url + path, {
        method,
        signal: signal ?? AbortSignal.timeout(15000),
        headers: {
          Authorization: `Bearer ${d.token}`,
          'Content-Type': 'application/json',
          ...(this.attachment ? { 'X-Samrajya-Attachment': this.attachment } : {}),
        },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
    } catch (error) {
      if (signal?.aborted) throw error;
      this.reset();
      throw new BridgeError(0, 'cannot reach mahamantri\'s bridge - is it running?');
    }
    const text = await res.text();
    let json: any;
    try {
      json = text ? JSON.parse(text) : {};
    } catch {
      json = { error: text };
    }
    if (res.status === 401) this.reset();
    if (!res.ok) throw new BridgeError(res.status, json?.error ?? `bridge answered ${res.status}`, json);
    return json as T;
  }

  /**
   * Attach as an agent, with the agent id and the secret its launcher gave
   * it. Throws BridgeError 403 when the secret is stale (this session has
   * been replaced or stopped) and 426 when this plugin is too old.
   */
  async attach(client: string, agent: Agent): Promise<void> {
    const out = await this.request<{ attachment: string }>('POST', '/v1/attach', { client, agent: agent.id, secret: agent.secret, protocol: PROTOCOL });
    this.attachment = out.attachment;
  }
}
