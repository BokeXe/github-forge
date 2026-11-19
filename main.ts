// Trial entrypoint shim: the harness runs `node /bot/main.ts` (Node 22).
// The forge itself is the statically linked Go binary committed beside this
// file as ./forge (vendored source, per competition.yaml). This shim only
// spawns it with the trial environment inherited, forwards its output, and
// exits with its code. The Go binary owns the whole lifecycle: bind an
// ephemeral port, POST $MAKEFUN_ENV_URL/register, poll /done, exit 0.

import { spawn } from 'node:child_process';
import { chmodSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const binary = join(here, 'forge');
try { chmodSync(binary, 0o755); } catch {}

const child = spawn(binary, [], { stdio: 'inherit', env: process.env });

child.on('error', (err) => {
  process.stderr.write(`[entry] failed to start forge binary: ${err.message}\n`);
  process.exit(1);
});

child.on('exit', (code, signal) => {
  if (signal) {
    process.stderr.write(`[entry] forge binary killed by ${signal}\n`);
    process.exit(1);
  }
  process.exit(code ?? 1);
});
