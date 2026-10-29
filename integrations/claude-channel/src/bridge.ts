import { readFile, realpath } from 'node:fs/promises';

// Client for mahamantri's native bridge API (mahamantri/bridge). Mahamantri
// writes its address and a fresh token to the discovery file on every
// start, so the file is reread whenever the bridge stops answering or
// rejects the token.

export interface Discovery {
  url: string;
  token: string;
  /** Where the main session runs; only a session there attaches by itself. */
  mainDirectory?: string;
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
        throw new BridgeError(0, `mahamantri is not running in native mode (no ${this.discoveryPath})`);
      }
      const d = JSON.parse(raw) as Partial<Discovery>;
      if (typeof d.url !== 'string' || typeof d.token !== 'string') {
        throw new BridgeError(0, `${this.discoveryPath} is not a bridge discovery file`);
      }
      this.discovery = { url: d.url, token: d.token, mainDirectory: typeof d.mainDirectory === 'string' ? d.mainDirectory : undefined };
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
      throw new BridgeError(0, 'cannot reach mahamantri\'s bridge - is it running in native mode?');
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

  /** Whether a session in dir is the main one by mahamantri's configuration (any, if none is set). */
  async isMainDirectory(dir: string): Promise<boolean> {
    const main = (await this.locate()).mainDirectory;
    if (!main) return true;
    const real = async (p: string) => { try { return await realpath(p); } catch { return p; } };
    return (await real(dir)) === (await real(main));
  }

  /** Attach as the main session. Returns false when another session holds it. */
  async attach(client: string, takeover = false): Promise<boolean> {
    try {
      const out = await this.request<{ attachment: string }>('POST', '/v1/attach', { client, takeover });
      this.attachment = out.attachment;
      return true;
    } catch (error) {
      if (error instanceof BridgeError && error.status === 409) return false;
      throw error;
    }
  }
}
