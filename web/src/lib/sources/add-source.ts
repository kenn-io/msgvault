import { shellQuote } from '../settings/shell-quote';

interface SourceKind {
  id: string;
  name: string;
  description: string;
}

export interface CommandSourceKind extends SourceKind {
  docs: string;
  fields: { id: string; label: string; placeholder: string }[];
  template: string;
}

export type AddSourceKind = CommandSourceKind | (SourceKind & { settings: 'carddav' });

export const ADD_SOURCE_KINDS: readonly AddSourceKind[] = [
  {
    id: 'gmail', name: 'Gmail', description: 'Includes Google Workspace',
    docs: 'https://msgvault.io/docs/setup/#configure-oauth',
    fields: [{ id: 'email', label: 'Email address', placeholder: 'you@gmail.com' }],
    template: 'msgvault add-account -- {email}',
  },
  {
    id: 'microsoft', name: 'Microsoft 365 or Outlook.com', description: 'Includes Hotmail',
    docs: 'https://msgvault.io/docs/guides/oauth-setup/#microsoft-365-outlook-hotmail',
    fields: [{ id: 'email', label: 'Email address', placeholder: 'you@example.com' }],
    template: 'msgvault add-o365 -- {email}',
  },
  {
    id: 'imap', name: 'IMAP mailbox', description: 'Any other mail provider',
    docs: 'https://msgvault.io/docs/setup/#add-an-imap-account',
    fields: [
      { id: 'host', label: 'Server', placeholder: 'imap.example.com' },
      { id: 'username', label: 'Username', placeholder: 'you@example.com' },
    ],
    template: 'msgvault add-imap --host {host} --username={username}',
  },
  {
    id: 'contacts', name: 'Contacts', description: 'Google, Microsoft, or CardDAV address books',
    settings: 'carddav',
  },
];

function arg(value: string, shell: 'posix' | 'cmd'): string {
  return /^[A-Za-z0-9@._+:/-]+$/.test(value) ? value : shellQuote(value, shell);
}

function validImapServer(host: string): boolean {
  return !/^-|\s/.test(host) && (!host.includes(':') || URL.canParse(`http://[${host.replace(/^\[(.*)\]$/, '$1').replace(/%[^\[\]]+$/, '')}]`));
}

export function addSourceCommand(kind: CommandSourceKind, values: Record<string, string>, shell: 'posix' | 'cmd'): string | undefined {
  const value = (id: string) => (id === 'username' ? values[id] : values[id]?.trim());
  if (kind.fields.some(field => !value(field.id))) return undefined;
  if (kind.id === 'imap' && !validImapServer(value('host')!)) return undefined;
  return kind.template.replace(/\{(\w+)\}/g, (_, id: string) => arg(value(id)!, shell));
}
