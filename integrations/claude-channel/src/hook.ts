import { Bridge, BridgeError, type Agent } from './bridge.js';

// Claude Code's StopFailure hook, run as `server.mjs --stop-failure` with the
// hook's JSON on stdin when a turn ends on the usage limit. It tells
// mahamantri, which tells the people waiting, launches nothing until the
// limit resets, and then resumes the work. Only Samrajya's agents report.

export interface StopFailure {
  cwd?: string;
  error?: string;
  error_details?: string;
  last_assistant_message?: string;
}

export async function reportStopFailure(input: StopFailure, bridge: Bridge, agent?: Agent): Promise<boolean> {
  if (input.error !== 'rate_limit' || !agent) return false;
  await bridge.request('POST', '/v1/limit', {
    message: input.last_assistant_message ?? '', details: input.error_details ?? '', agent: agent.id, secret: agent.secret,
  });
  return true;
}

/**
 * Claude Code's UserPromptSubmit and Stop hooks, run as `server.mjs --turn
 * start|end`: tell mahamantri when this agent's turns start and end, so it
 * can tell a long step from a stuck or idle session. Only Samrajya's agents
 * report.
 */
export async function reportTurn(state: 'start' | 'end', bridge: Bridge, agent?: Agent): Promise<boolean> {
  if (!agent) return false;
  await bridge.request('POST', '/v1/turn', { agent: agent.id, secret: agent.secret, state }, AbortSignal.timeout(5000));
  return true;
}

/**
 * The waiter: Claude Code runs it (`server.mjs --wait`) in the background
 * whenever this agent's session ends a turn, as an asyncRewake hook. It
 * waits for messages mahamantri routed to this agent and, when some arrive,
 * writes them out and returns 2, which wakes the session with them. It
 * returns 0 - waking nobody - when a newer waiter replaced it or this
 * session was replaced. Mahamantri hands each message over once.
 */
export async function waitForEvents(bridge: Bridge, agent: Agent | undefined, write: (text: string) => void,
  opts: { seconds?: number; retryMs?: number; signal?: AbortSignal } = {}): Promise<number> {
  if (!agent) return 0;
  const seconds = opts.seconds ?? 240;
  while (!opts.signal?.aborted) {
    try {
      const out = await bridge.request<{ events?: number; text?: string; superseded?: boolean }>('POST', '/v1/wait',
        { agent: agent.id, secret: agent.secret, seconds }, AbortSignal.timeout((seconds + 20) * 1000));
      if (out.superseded) return 0;
      if (out.events && out.text) {
        write(out.text);
        return 2;
      }
    } catch (error) {
      if (error instanceof BridgeError && error.status === 403) return 0;
      // mahamantri is restarting or unreachable: keep waiting.
      await new Promise((r) => setTimeout(r, opts.retryMs ?? 5000));
    }
  }
  return 0;
}
