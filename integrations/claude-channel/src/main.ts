import { homedir } from 'node:os';
import { join } from 'node:path';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { createChannel } from './channel.js';

// Same default as mahamantri's bridge.discoveryFile.
const discoveryPath = process.env.SAMRAJYA_BRIDGE_FILE ||
  join(process.env.XDG_STATE_HOME || join(homedir(), '.local', 'state'), 'samrajya', 'bridge.json');

const channel = createChannel({ discoveryPath, main: process.env.SAMRAJYA_MAIN === '1' });
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
