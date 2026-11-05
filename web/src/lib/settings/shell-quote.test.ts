import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

import { shellQuote } from './shell-quote';

const roundTripValues = [
  'you@example.com',
  "o'brien@example.com",
  'Work App',
  'Work & Home',
  'Work $Budget',
  '$(echo LEAKED)',
  'a&b|c<d>e^f(g)',
  'a"b@example.com',
  'say "hi" & leave',
  'Work %QUOTE_PROBE% App',
  '100%',
  'Work!',
  'Hey!QUOTE_PROBE!',
  '!undefined!',
  'a^b%QUOTE_PROBE%',
  '%%!!',
  'x\\"y\\%z!',
  'trailing\\',
  'C:\\Users\\me\\"quoted"\\',
  '',
];

describe('shellQuote', () => {
  it('round-trips arguments through the platform shell', () => {
    const shell = process.platform === 'win32' ? 'cmd' : 'posix';
    const dir = mkdtempSync(join(tmpdir(), 'msgvault-shell-quote-'));
    try {
      const script = join(dir, 'argv.js');
      writeFileSync(script, 'console.log(JSON.stringify(process.argv.slice(2)));');
      const pairs = [
        ...roundTripValues.map((value) => [value, 'Work & echo LEAKED']),
        ['a^b@example.com', 'Work!'],
        ['a^b@example.com', '!QUOTE_PROBE!'],
        ['a!b@example.com', 'Work ^Budget'],
        ['a!b@example.com', 'Work ^!Budget'],
        ['you@example.com', 'Work $Budget'],
      ];
      for (const [email, app] of pairs) {
        const line = `${shellQuote(process.execPath, shell)} ${shellQuote(script, shell)} ${shellQuote(email, shell)} --oauth-app ${shellQuote(app, shell)}`;
        const result = spawnSync(shell === 'cmd' ? 'cmd.exe' : '/bin/sh', shell === 'cmd' ? ['/d', '/v:off', '/s', '/c', `"${line}"`] : ['-c', line], {
          env: { ...process.env, QUOTE_PROBE: 'expanded', Budget: 'expanded' },
          windowsVerbatimArguments: shell === 'cmd',
          encoding: 'utf8',
          timeout: 5_000,
        });
        expect(result.error).toBeUndefined();
        expect(result.status, result.stderr).toBe(0);
        expect(JSON.parse(result.stdout.trim()), `${email} ${app}`).toEqual([email, '--oauth-app', app]);
      }
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  }, 60_000);
});
