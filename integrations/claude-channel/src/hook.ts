import { Bridge, type Agent } from './bridge.js';

// Claude Code's StopFailure hook, run as `server.mjs --stop-failure` with the
// hook's JSON on stdin when a turn ends on the usage limit. It tells
// mahamantri, which says so on Linear and wakes the session once the limit
// resets. Only the main session's limit matters.

export interface StopFailure {
  cwd?: string;
  error?: string;
  error_details?: string;
  last_assistant_message?: string;
}

export async function reportStopFailure(input: StopFailure, bridge: Bridge, main: boolean, agent?: Agent): Promise<boolean> {
  if (input.error !== 'rate_limit') return false;
  // In agents mode every agent's limit is the account's; in legacy mode
  // only the main session's matters.
  if (!agent && !main && !(await bridge.isMainDirectory(input.cwd ?? process.cwd()))) return false;
  await bridge.request('POST', '/v1/limit', {
    message: input.last_assistant_message ?? '', details: input.error_details ?? '',
    ...(agent ? { agent: agent.id, secret: agent.secret } : {}),
  });
  return true;
}

/**
 * Claude Code's UserPromptSubmit and Stop hooks, run as `server.mjs --turn
 * start|end`: tell mahamantri when this agent's turns start and end, so it
 * can tell a long step from a stuck or idle session. Nothing outside agents
 * mode.
 */
export async function reportTurn(state: 'start' | 'end', bridge: Bridge, agent?: Agent): Promise<boolean> {
  if (!agent) return false;
  await bridge.request('POST', '/v1/turn', { agent: agent.id, secret: agent.secret, state }, AbortSignal.timeout(5000));
  return true;
}
