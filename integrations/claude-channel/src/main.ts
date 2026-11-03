import { homedir } from 'node:os';
import { join } from 'node:path';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { Bridge } from './bridge.js';
import { createChannel } from './channel.js';
import { reportStopFailure } from './hook.js';

// Same default as mahamantri's bridge.discoveryFile.
const discoveryPath = process.env.SAMRAJYA_BRIDGE_FILE ||
  join(process.env.XDG_STATE_HOME || join(homedir(), '.local', 'state'), 'samrajya', 'bridge.json');

const main = process.env.SAMRAJYA_MAIN === '1';

if (process.argv[2] === '--stop-failure') {
  // A hook: report and exit, never block the session.
  let raw = '';
  for await (const chunk of process.stdin) raw += chunk;
  try {
    await reportStopFailure(JSON.parse(raw), new Bridge(discoveryPath), main);
  } catch (error) {
    console.error(`samrajya: could not report the usage limit: ${error instanceof Error ? error.message : error}`);
  }
  process.exit(0);
}

const channel = createChannel({ discoveryPath, main });
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
