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
    template: 'msgvault add-account {email}',
  },
  {
    id: 'microsoft', name: 'Microsoft 365 or Outlook.com', description: 'Includes Hotmail',
    docs: 'https://msgvault.io/docs/guides/oauth-setup/#microsoft-365-outlook-hotmail',
    fields: [{ id: 'email', label: 'Email address', placeholder: 'you@example.com' }],
    template: 'msgvault add-o365 {email}',
  },
  {
    id: 'imap', name: 'IMAP mailbox', description: 'Any other mail provider',
    docs: 'https://msgvault.io/docs/usage/imap/',
    fields: [
      { id: 'host', label: 'Server', placeholder: 'imap.example.com' },
      { id: 'username', label: 'Username', placeholder: 'you@example.com' },
    ],
    template: 'msgvault add-imap --host {host} --username {username}',
  },
  {
    id: 'contacts', name: 'Contacts', description: 'Google, Microsoft, or CardDAV address books',
    settings: 'carddav',
  },
];

// A bare value starting with "-" needs "--" before it, or the CLI reads it as an option.
function arg(value: string, shell: 'posix' | 'cmd', optionValue: boolean): string {
  const separator = value.startsWith('-') && !optionValue ? '-- ' : '';
  return separator + (/^[A-Za-z0-9@._+:/-]+$/.test(value) ? value : shellQuote(value, shell));
}

export function imapServer(value: string): { host: string; port?: string } | undefined {
  const server = value.trim();
  if (!server) return undefined;
  const match = /^(\[[^\]]+\]|[^:]+):([^:]*)$/.exec(server);
  if (!match) return { host: server };
  const port = match[2]!;
  if (!/^\d+$/.test(port) || Number(port) < 1 || Number(port) > 65535) return undefined;
  return { host: match[1]!, port: String(Number(port)) };
}

export function addSourceCommand(kind: CommandSourceKind, values: Record<string, string>, shell: 'posix' | 'cmd'): string | undefined {
  if (kind.fields.some(field => !values[field.id]?.trim())) return undefined;
  if (kind.id === 'imap') {
    const server = imapServer(values.host ?? '');
    if (!server) return undefined;
    const port = server.port ? ` --port ${server.port}` : '';
    return `msgvault add-imap --host ${arg(server.host, shell, true)}${port} --username ${arg(values.username!.trim(), shell, true)}`;
  }
  return kind.template.replace(/(\S+) \{(\w+)\}/g, (_, before: string, id: string) => `${before} ${arg(values[id]!.trim(), shell, before.startsWith('--'))}`);
}
