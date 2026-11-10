import { describe, expect, it } from 'vitest';
import { ADD_SOURCE_KINDS, addSourceCommand, type CommandSourceKind } from './add-source';

function commandKind(id: string): CommandSourceKind {
  const kind = ADD_SOURCE_KINDS.find(kind => kind.id === id);
  if (!kind || !('fields' in kind)) throw new Error(`Missing command source kind: ${id}`);
  return kind;
}

const gmail = commandKind('gmail');
const imap = commandKind('imap');

describe('addSourceCommand', () => {
  it('builds account commands and requires every IMAP field', () => {
    expect(addSourceCommand(gmail, { email: ' user@example.org ' }, 'posix')).toBe('msgvault add-account user@example.org');
    expect(addSourceCommand(gmail, { email: '  ' }, 'posix')).toBeUndefined();
    expect(addSourceCommand(imap, { host: 'imap.example.com' }, 'posix')).toBeUndefined();
    expect(addSourceCommand(commandKind('microsoft'), { email: 'user@example.org' }, 'cmd')).toBe('msgvault add-o365 user@example.org');
  });

  it('requires an IMAP server name without a port or spaces', () => {
    expect(addSourceCommand(imap, { host: 'imap.example.com:143', username: 'reader' }, 'posix')).toBeUndefined();
    expect(addSourceCommand(imap, { host: 'imap example.com', username: 'reader' }, 'posix')).toBeUndefined();
    expect(addSourceCommand(imap, { host: '[2001:db8::1]:143', username: 'reader' }, 'posix')).toBeUndefined();
    expect(addSourceCommand(imap, { host: 'imap.example.com:993:99999', username: 'reader' }, 'posix')).toBeUndefined();
  });

  it('accepts IPv6 server addresses and hyphen-leading IMAP usernames', () => {
    expect(addSourceCommand(imap, { host: '2001:db8::1', username: '-reader' }, 'posix')).toBe('msgvault add-imap --host 2001:db8::1 --username=-reader');
    expect(addSourceCommand(imap, { host: '[2001:db8::1]', username: '-reader' }, 'cmd')).toBe('msgvault add-imap --host "[2001:db8::1]" --username=-reader');
    expect(addSourceCommand(imap, { host: 'fe80::1%eth0', username: 'reader' }, 'posix')).toBe("msgvault add-imap --host 'fe80::1%eth0' --username=reader");
  });

  it('quotes spaces and shell characters for each platform', () => {
    expect(addSourceCommand(imap, { host: 'imap.example.org', username: 'A B' }, 'posix')).toBe("msgvault add-imap --host imap.example.org --username='A B'");
    expect(addSourceCommand(imap, { host: 'imap.example.org', username: 'A B' }, 'cmd')).toBe('msgvault add-imap --host imap.example.org --username="A B"');
    expect(addSourceCommand(imap, { host: 'imap.example.com', username: '~/reader' }, 'posix')).toBe("msgvault add-imap --host imap.example.com --username='~/reader'");
  });

  it('keeps an address that starts with a hyphen from reading as an option', () => {
    expect(addSourceCommand(gmail, { email: '-reader@example.org' }, 'posix')).toBeUndefined();
    expect(addSourceCommand(commandKind('microsoft'), { email: '-reader@example.org' }, 'cmd')).toBeUndefined();
    expect(addSourceCommand(imap, { host: 'imap.example.org', username: '-reader' }, 'posix')).toBe('msgvault add-imap --host imap.example.org --username=-reader');
  });
});
