import { execFileSync } from 'node:child_process';
import { describe, expect, it } from 'vitest';
import { ADD_SOURCE_KINDS, addSourceCommand, type CommandSourceKind } from './add-source';

function commandKind(id: string): CommandSourceKind {
  const kind = ADD_SOURCE_KINDS.find(kind => kind.id === id);
  if (!kind || !('fields' in kind)) throw new Error(`Missing command source kind: ${id}`);
  return kind;
}

const gmail = commandKind('gmail');
const imap = commandKind('imap');
const mbox = commandKind('mbox');

describe('addSourceCommand', () => {
  it('trims safe Gmail values in both shells and waits for a value', () => {
    for (const shell of ['posix', 'cmd'] as const) {
      expect(addSourceCommand(gmail, { email: ' you@gmail.com ' }, shell)).toBe('msgvault add-account you@gmail.com');
      expect(addSourceCommand(gmail, { email: '  ' }, shell)).toBeUndefined();
    }
  });

  it('quotes apostrophes and dollar signs per shell', () => {
    const values = { email: 'you@example.com', path: "/tmp/O'Brien $notes.mbox" };
    expect(addSourceCommand(mbox, values, 'posix')).toBe("msgvault import-mbox -- you@example.com '/tmp/O'\\''Brien $notes.mbox'");
    expect(addSourceCommand(mbox, values, 'cmd')).toBe('msgvault import-mbox -- you@example.com "/tmp/O\'Brien $notes.mbox"');
  });

  it('expands a home path only in POSIX shells', () => {
    const values = { email: 'you@example.com', path: '~/All mail.mbox' };
    expect(addSourceCommand(mbox, values, 'posix')).toBe("msgvault import-mbox -- you@example.com ~/'All mail.mbox'");
    expect(addSourceCommand(mbox, values, 'cmd')).toBe('msgvault import-mbox -- you@example.com "~/All mail.mbox"');
  });

  it('places an option terminator before hyphen-prefixed MBOX arguments', () => {
    const values = { email: '-reader@example.com', path: '-archive.mbox' };
    for (const shell of ['posix', 'cmd'] as const) {
      expect(addSourceCommand(mbox, values, shell)).toBe('msgvault import-mbox -- -reader@example.com -archive.mbox');
    }
    const command = addSourceCommand(mbox, values, 'posix')!;
    const output = execFileSync('sh', ['-c', command.replace('msgvault import-mbox', 'printf "%s\\n"')], { encoding: 'utf8' });
    expect(output).toBe('--\n-reader@example.com\n-archive.mbox\n');
  });

  it('preserves a tilde-prefixed IMAP username', () => {
    const command = addSourceCommand(imap, { host: 'imap.example.com', username: '~/reader' }, 'posix')!;
    expect(command).toBe("msgvault add-imap --host imap.example.com --username '~/reader'");
    const output = execFileSync('sh', ['-c', command.replace('msgvault add-imap', 'printf "%s\\n"')], { encoding: 'utf8' });
    expect(output).toBe('--host\nimap.example.com\n--username\n~/reader\n');
  });

  it('preserves a quoted path through a real POSIX shell', () => {
    const command = addSourceCommand(mbox, { email: 'you@example.com', path: "/tmp/O'Brien $notes.mbox" }, 'posix')!;
    const output = execFileSync('sh', ['-c', command.replace('msgvault import-mbox', 'printf "%s\\n"')], { encoding: 'utf8' });
    expect(output).toBe("--\nyou@example.com\n/tmp/O'Brien $notes.mbox\n");
  });

  it('requires both IMAP fields', () => {
    expect(addSourceCommand(imap, { host: 'imap.example.com' }, 'posix')).toBeUndefined();
    expect(addSourceCommand(imap, { username: 'you@example.com' }, 'cmd')).toBeUndefined();
    expect(addSourceCommand(imap, { host: 'imap.example.com', username: 'you@example.com' }, 'posix')).toBe('msgvault add-imap --host imap.example.com --username you@example.com');
  });
});
