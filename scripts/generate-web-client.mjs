import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const webDirectory = path.join(repositoryRoot, 'web');
const generator = path.join(
  webDirectory,
  'node_modules',
  '.bin',
  process.platform === 'win32' ? 'openapi-typescript.cmd' : 'openapi-typescript'
);
const result = spawnSync(
  generator,
  ['../api/openapi.yaml', '--output', 'src/lib/api/generated/schema.d.ts'],
  {
    cwd: webDirectory,
    stdio: 'inherit'
  }
);

if (result.error) {
  throw result.error;
}

process.exitCode = result.status ?? 1;
