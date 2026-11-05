import { describe, expect, it } from 'vitest';
import { withShellArgv } from '../settings/shell-argv.test-helper';
import { ADD_SOURCE_KINDS, addSourceCommand, type CommandSourceKind } from './add-source';

function commandKind(id: string): CommandSourceKind {
  const kind = ADD_SOURCE_KINDS.find(kind => kind.id === id);
  if (!kind || !('fields' in kind)) throw new Error(`Missing command source kind: ${id}`);
  return kind;
}

const gmail = commandKind('gmail');
const imap = commandKind('imap');

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
    withShellArgv((run, shell) => {
      const args = addSourceCommand(imap, { host: 'imap.example.com', username: '~/reader' }, shell)!.slice('msgvault add-imap '.length);
      expect(run(args)).toEqual(['--host', 'imap.example.com', '--username', '~/reader']);
    });
  });
});
