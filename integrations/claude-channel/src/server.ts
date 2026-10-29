import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { createProbe } from './probe.js';

async function main() {
  const rawPort = process.env.SAMRAJYA_PROBE_PORT ?? '0';
  if (!/^\d+$/.test(rawPort) || Number(rawPort) > 65535) {
    throw new Error('SAMRAJYA_PROBE_PORT must be an integer from 0 to 65535');
  }
  const probe = createProbe(process.env.SAMRAJYA_PROBE_TOKEN ?? '');
  let stopping = false;
  async function stop() {
    if (stopping) return;
    stopping = true;
    await probe.close();
  }
  process.once('SIGINT', () => { void stop(); });
  process.once('SIGTERM', () => { void stop(); });
  process.stdin.once('end', () => { void stop(); });
  try {
    await probe.mcp.connect(new StdioServerTransport());
    const port = await probe.listen(Number(rawPort));
    console.error(`samrajya-probe listening on 127.0.0.1:${port}; memory only, not production ingress`);
  } catch (error) {
    await stop();
    throw error;
  }
}

void main().catch((error: unknown) => {
  console.error(error instanceof Error ? error.message : 'probe startup failed');
  process.exitCode = 1;
});
