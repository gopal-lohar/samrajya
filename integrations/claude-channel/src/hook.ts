import { Bridge, type Agent } from './bridge.js';

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
