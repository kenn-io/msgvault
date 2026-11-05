import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { shellQuote } from '../settings/shell-quote';
import { ADD_SOURCE_KINDS, addSourceCommand, type CommandSourceKind } from './add-source';

function commandKind(id: string): CommandSourceKind {
  const kind = ADD_SOURCE_KINDS.find(kind => kind.id === id);
  if (!kind || !('fields' in kind)) throw new Error(`Missing command source kind: ${id}`);
  return kind;
}

const gmail = commandKind('gmail');
const imap = commandKind('imap');

function roundTrip(command: string, prefix: string): string[] {
  const shell = process.platform === 'win32' ? 'cmd' : 'posix';
  const dir = mkdtempSync(join(tmpdir(), 'msgvault-add-source-'));
  try {
    const script = join(dir, 'argv.js');
    writeFileSync(script, 'console.log(JSON.stringify(process.argv.slice(2)));');
    const line = command.replace(prefix, `${shellQuote(process.execPath, shell)} ${shellQuote(script, shell)}`);
    const result = spawnSync(shell === 'cmd' ? 'cmd.exe' : '/bin/sh', shell === 'cmd' ? ['/d', '/v:off', '/s', '/c', `"${line}"`] : ['-c', line], {
      encoding: 'utf8', windowsVerbatimArguments: shell === 'cmd', timeout: 5_000,
    });
    expect(result.error).toBeUndefined();
    expect(result.status, result.stderr).toBe(0);
    return JSON.parse(result.stdout.trim());
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

describe('addSourceCommand', () => {
  it('trims Gmail values and requires every IMAP field', () => {
    for (const shell of ['posix', 'cmd'] as const) {
      expect(addSourceCommand(gmail, { email: ' you@gmail.com ' }, shell)).toBe('msgvault add-account you@gmail.com');
      expect(addSourceCommand(gmail, { email: '  ' }, shell)).toBeUndefined();
      expect(addSourceCommand(imap, { host: 'imap.example.com' }, shell)).toBeUndefined();
    }
  });

  it('preserves a tilde-prefixed IMAP username', () => {
    const command = addSourceCommand(imap, { host: 'imap.example.com', username: '~/reader' }, 'posix')!;
    expect(command).toBe("msgvault add-imap --host imap.example.com --username '~/reader'");
    const shell = process.platform === 'win32' ? 'cmd' : 'posix';
    expect(roundTrip(addSourceCommand(imap, { host: 'imap.example.com', username: '~/reader' }, shell)!, 'msgvault add-imap')).toEqual(['--host', 'imap.example.com', '--username', '~/reader']);
  });
});
