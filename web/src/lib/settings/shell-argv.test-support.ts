import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { expect } from 'vitest';
import { shellQuote } from './shell-quote';

export function withShellArgv(test: (run: (args: string) => string[], shell: 'posix' | 'cmd') => void) {
  const shell = process.platform === 'win32' ? 'cmd' : 'posix';
  const dir = mkdtempSync(join(tmpdir(), 'msgvault-shell-quote-'));
  try {
    const script = join(dir, 'argv.js');
    writeFileSync(script, 'console.log(JSON.stringify(process.argv.slice(2)));');
    test(args => {
      const line = `${shellQuote(process.execPath, shell)} ${shellQuote(script, shell)} ${args}`;
      const result = spawnSync(shell === 'cmd' ? 'cmd.exe' : '/bin/sh', shell === 'cmd' ? ['/d', '/v:off', '/s', '/c', `"${line}"`] : ['-c', line], {
        env: { ...process.env, QUOTE_PROBE: 'expanded', Budget: 'expanded' },
        windowsVerbatimArguments: shell === 'cmd', encoding: 'utf8', timeout: 5_000,
      });
      expect(result.error).toBeUndefined();
      expect(result.status, result.stderr).toBe(0);
      return JSON.parse(result.stdout.trim());
    }, shell);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}
