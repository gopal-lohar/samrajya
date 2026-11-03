import { Bridge } from './bridge.js';

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

export async function reportStopFailure(input: StopFailure, bridge: Bridge, main: boolean): Promise<boolean> {
  if (input.error !== 'rate_limit') return false;
  if (!main && !(await bridge.isMainDirectory(input.cwd ?? process.cwd()))) return false;
  await bridge.request('POST', '/v1/limit', { message: input.last_assistant_message ?? '', details: input.error_details ?? '' });
  return true;
}
