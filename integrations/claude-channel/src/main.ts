import { homedir } from 'node:os';
import { join } from 'node:path';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { agentFromEnv, Bridge } from './bridge.js';
import { createChannel } from './channel.js';
import { reportStopFailure, reportTurn, waitForEvents } from './hook.js';

// Same default as mahamantri's bridge.discoveryFile.
const discoveryPath = process.env.SAMRAJYA_BRIDGE_FILE ||
  join(process.env.XDG_STATE_HOME || join(homedir(), '.local', 'state'), 'samrajya', 'bridge.json');

const agent = await agentFromEnv();

if (process.argv[2] === '--stop-failure') {
  // A hook: report and exit, never block the session.
  let raw = '';
  for await (const chunk of process.stdin) raw += chunk;
  try {
    await reportStopFailure(JSON.parse(raw), new Bridge(discoveryPath), agent);
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

if (process.argv[2] === '--wait') {
  // The waiter (an asyncRewake hook): exit 2 with the new messages to wake
  // the session with them.
  process.stdin.resume().on('error', () => {});
  // The turn that just ended, first: a hand-over below starts the next.
  await reportTurn('end', new Bridge(discoveryPath), agent).catch(() => {});
  const code = await waitForEvents(new Bridge(discoveryPath), agent, (text) => {
    process.stderr.write(`Samrajya: new messages for you.\n\n${text}\n`);
  });
  process.exit(code);
}

const channel = createChannel({ discoveryPath, agent });
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
