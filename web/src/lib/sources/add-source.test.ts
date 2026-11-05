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
const microsoft = commandKind('microsoft');
const imap = commandKind('imap');
const mbox = commandKind('mbox');

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
  it('trims Gmail values and keeps hyphen-prefixed arguments positional', () => {
    for (const shell of ['posix', 'cmd'] as const) {
      expect(addSourceCommand(gmail, { email: ' you@gmail.com ' }, shell)).toBe('msgvault add-account -- you@gmail.com');
      expect(addSourceCommand(gmail, { email: '  ' }, shell)).toBeUndefined();
      expect(addSourceCommand(gmail, { email: '-reader@example.com' }, shell)).toBe('msgvault add-account -- -reader@example.com');
      expect(addSourceCommand(microsoft, { email: '-reader@example.com' }, shell)).toBe('msgvault add-o365 -- -reader@example.com');
      expect(addSourceCommand(mbox, { email: '-reader@example.com', path: '-archive.mbox' }, shell)).toBe('msgvault import-mbox -- -reader@example.com -archive.mbox');
      expect(addSourceCommand(imap, { host: 'imap.example.com' }, shell)).toBeUndefined();
    }
  });

  it('preserves a quoted path through the platform shell', () => {
    const values = { email: 'you@example.com', path: "/tmp/O'Brien $notes.mbox" };
    expect(addSourceCommand(mbox, values, 'posix')).toBe("msgvault import-mbox -- you@example.com '/tmp/O'\\''Brien $notes.mbox'");
    expect(addSourceCommand(mbox, values, 'cmd')).toBe('msgvault import-mbox -- you@example.com "/tmp/O\'Brien $notes.mbox"');
    const shell = process.platform === 'win32' ? 'cmd' : 'posix';
    expect(roundTrip(addSourceCommand(mbox, values, shell)!, 'msgvault import-mbox')).toEqual(['--', 'you@example.com', "/tmp/O'Brien $notes.mbox"]);
  });

  it('expands a home path only in POSIX shells', () => {
    const values = { email: 'you@example.com', path: '~/All mail.mbox' };
    expect(addSourceCommand(mbox, values, 'posix')).toBe("msgvault import-mbox -- you@example.com ~/'All mail.mbox'");
    expect(addSourceCommand(mbox, values, 'cmd')).toBe('msgvault import-mbox -- you@example.com "~/All mail.mbox"');
  });

  it('places an option terminator before hyphen-prefixed MBOX arguments', () => {
    const values = { email: '-reader@example.com', path: '-archive.mbox' };
    const shell = process.platform === 'win32' ? 'cmd' : 'posix';
    expect(roundTrip(addSourceCommand(mbox, values, shell)!, 'msgvault import-mbox')).toEqual(['--', '-reader@example.com', '-archive.mbox']);
  });

  it('preserves a tilde-prefixed IMAP username', () => {
    const command = addSourceCommand(imap, { host: 'imap.example.com', username: '~/reader' }, 'posix')!;
    expect(command).toBe("msgvault add-imap --host imap.example.com --username '~/reader'");
    const shell = process.platform === 'win32' ? 'cmd' : 'posix';
    expect(roundTrip(addSourceCommand(imap, { host: 'imap.example.com', username: '~/reader' }, shell)!, 'msgvault add-imap')).toEqual(['--host', 'imap.example.com', '--username', '~/reader']);
  });
});
