import { shellQuote } from '../settings/shell-quote';

interface SourceKind {
  id: string;
  name: string;
  description: string;
  docs: string;
}

export interface CommandSourceKind extends SourceKind {
  fields: { id: string; label: string; placeholder: string }[];
  template: string;
}

export type AddSourceKind = CommandSourceKind | (SourceKind & { settings: 'carddav' });

export const ADD_SOURCE_KINDS: readonly AddSourceKind[] = [
  {
    id: 'gmail', name: 'Gmail', description: 'Sync a Gmail or Google Workspace mailbox.',
    docs: 'https://msgvault.io/docs/setup/#configure-oauth',
    fields: [{ id: 'email', label: 'Email address', placeholder: 'you@gmail.com' }],
    template: 'msgvault add-account {email}',
  },
  {
    id: 'microsoft', name: 'Microsoft 365 or Outlook.com', description: 'Sync Outlook, Hotmail, or Microsoft 365 mail.',
    docs: 'https://msgvault.io/docs/guides/oauth-setup/#microsoft-365-outlook-hotmail',
    fields: [{ id: 'email', label: 'Email address', placeholder: 'you@example.com' }],
    template: 'msgvault add-o365 {email}',
  },
  {
    id: 'imap', name: 'IMAP mailbox', description: 'Sync any mailbox your provider serves over IMAP.',
    docs: 'https://msgvault.io/docs/usage/imap/',
    fields: [
      { id: 'host', label: 'Server', placeholder: 'imap.example.com' },
      { id: 'username', label: 'Username', placeholder: 'you@example.com' },
    ],
    template: 'msgvault add-imap --host {host} --username {username}',
  },
  {
    id: 'mbox', name: 'MBOX file', description: 'Import an export once, such as Google Takeout mail.',
    docs: 'https://msgvault.io/docs/usage/importing/#import-mbox',
    fields: [
      { id: 'email', label: 'Email address', placeholder: 'you@example.com' },
      { id: 'path', label: 'Full path to the file', placeholder: '/Users/you/Downloads/export.mbox' },
    ],
    template: 'msgvault import-mbox {email} {path}',
  },
  {
    id: 'contacts', name: 'Contacts (CardDAV)', description: 'Import address books from Google, Microsoft, or a CardDAV server.',
    docs: 'https://msgvault.io/docs/usage/people-carddav/', settings: 'carddav',
  },
];

function arg(value: string, shell: 'posix' | 'cmd', isPath: boolean): string {
  if (/^[A-Za-z0-9@._+:/-]+$/.test(value)) return value;
  if (isPath && shell === 'posix' && value.startsWith('~/')) return `~/${shellQuote(value.slice(2), shell)}`;
  return shellQuote(value, shell);
}

export function addSourceCommand(kind: CommandSourceKind, values: Record<string, string>, shell: 'posix' | 'cmd'): string | undefined {
  if (kind.fields.some(field => !values[field.id]?.trim())) return undefined;
  return kind.template.replace(/\{(\w+)\}/g, (_, id: string) => arg(values[id]!.trim(), shell, kind.id === 'mbox' && id === 'path'));
}
