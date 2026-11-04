import { homedir } from 'node:os';
import { join } from 'node:path';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { agentFromEnv, Bridge } from './bridge.js';
import { createChannel } from './channel.js';
import { reportStopFailure, reportTurn } from './hook.js';

// Same default as mahamantri's bridge.discoveryFile.
const discoveryPath = process.env.SAMRAJYA_BRIDGE_FILE ||
  join(process.env.XDG_STATE_HOME || join(homedir(), '.local', 'state'), 'samrajya', 'bridge.json');

const main = process.env.SAMRAJYA_MAIN === '1';
const agent = await agentFromEnv();

if (process.argv[2] === '--stop-failure') {
  // A hook: report and exit, never block the session.
  let raw = '';
  for await (const chunk of process.stdin) raw += chunk;
  try {
    await reportStopFailure(JSON.parse(raw), new Bridge(discoveryPath), main, agent);
  } catch (error) {
    console.error(`samrajya: could not report the usage limit: ${error instanceof Error ? error.message : error}`);
  }
  process.exit(0);
}

if (process.argv[2] === '--turn') {
  // A hook: report and exit at once, never block the session.
  process.stdin.resume().on('error', () => {});
  try {
    await reportTurn(process.argv[3] === 'start' ? 'start' : 'end', new Bridge(discoveryPath), agent);
  } catch {
    // The supervisor copes with a missed turn report.
  }
  process.exit(0);
}

const channel = createChannel({ discoveryPath, main, agent });
channel.mcp.oninitialized = () => channel.start();
let stopping = false;
async function stop() {
  if (stopping) return;
  stopping = true;
  await channel.stop();
}
process.once('SIGINT', () => { void stop(); });
process.once('SIGTERM', () => { void stop(); });
process.stdin.once('end', () => { void stop(); });

channel.mcp.connect(new StdioServerTransport()).catch((error: unknown) => {
  console.error(error instanceof Error ? error.message : 'samrajya channel failed to start');
  process.exitCode = 1;
});
