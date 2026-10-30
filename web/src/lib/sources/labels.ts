import type { SourceStatus } from '../api/generated/models';
import { shellQuote } from '../settings/shell-quote';
import { sentenceCase } from '../explore/labels';

// Curated friendly labels for daemon source types. Other codes use
// sentenceCase, including twenty → Twenty. An empty type shows no label:
// the identifier stands alone.
const SOURCE_TYPES: Readonly<Record<string, string>> = {
  '': '', gmail: 'Gmail', imap: 'IMAP', msmail: 'Microsoft mail', teams: 'Teams',
  discord: 'Discord', meeting_import: 'Meeting import', synctech_sms: 'SMS backup',
  imazing_csv: 'iMazing CSV', circleback: 'Circleback', plaud: 'Plaud', gcal: 'Google Calendar', muesli: 'Muesli',
  granola: 'Granola', notion_meetings: 'Notion meetings', twilio: 'Twilio', pst: 'PST import',
  'apple-mail': 'Apple Mail', mbox: 'Mbox import', beeper: 'Beeper', slack: 'Slack', inline: 'Inline', matrix: 'Matrix',
  eml: 'EML import', maildir: 'Maildir import', whatsapp: 'WhatsApp',
  apple_messages: 'Apple Messages', facebook_messenger: 'Facebook Messenger',
  'google-groups': 'Google Groups'
};
export function sourceTypeLabel(code: string): string {
  return Object.hasOwn(SOURCE_TYPES, code) ? SOURCE_TYPES[code]! : sentenceCase(code);
}

type StateSource = Pick<SourceStatus, 'active_sync' | 'latest_sync'> & Partial<Pick<SourceStatus, 'scheduler_queued' | 'scheduler_started_at'>>;

export function syncRunning(source: StateSource): boolean {
  return Boolean(source.active_sync || source.scheduler_started_at || source.latest_sync?.status === 'running');
}

/** The run state a source shows, or undefined when it has never synced. */
export function sourceRunState(source: StateSource): string | undefined {
  const latest = source.latest_sync;
  if (syncRunning(source)) return 'running';
  if (source.scheduler_queued && latest?.status !== 'failed' && !(latest?.status === 'completed' && latest.errors_count > 0)) return 'queued';
  if (!latest) return undefined;
  if (latest.status === 'paused') return 'paused';
  if (latest.status === 'cancelled') return 'cancelled';
  if (latest.status === 'failed') return 'failed';
  if (latest.status === 'completed') return latest.errors_count > 0 ? 'partial' : 'succeeded';
  return undefined;
}

/** The stored reason a source last failed. */
export function sourceFailure(
  source: Pick<SourceStatus, 'latest_sync' | 'scheduler_last_error'>,
): string | undefined {
  const message = source.latest_sync?.error_message || source.scheduler_last_error;
  return message || undefined;
}

/** The sync command for an account that has no schedule. Only mail-style identifiers have one. */
export function syncCommand(source: Pick<SourceStatus, 'identifier' | 'sync_unavailable_reason'>, shell: 'posix' | 'cmd' = 'posix'): string | undefined {
  if (source.sync_unavailable_reason !== 'sync_not_configured') return undefined;
  return /^[^\s/@]+@[^\s/@]+$/.test(source.identifier) ? `msgvault sync ${shellQuote(source.identifier, shell)}` : undefined;
}

/** Source types whose schedule, filters, and attachment rules live in the settings catalog. */
export const SOURCE_SETTING_PREFIX: Readonly<Record<string, string>> = {
  beeper: 'beeper',
  slack: 'slack',
  inline: 'inline',
  discord: 'discord',
  teams: 'teams',
};


const SYNC_UNAVAILABLE: Readonly<Record<string, string>> = {
  source_not_schedulable: 'Imported file — nothing to sync',
  sync_already_running: 'Sync in progress',
  scheduler_unavailable: 'Scheduler unavailable',
  sync_not_configured: 'Sync not set up'
};


export function syncUnavailableLabel(code: string | undefined): string {
  return code !== undefined && Object.hasOwn(SYNC_UNAVAILABLE, code) ? SYNC_UNAVAILABLE[code]! : 'Sync unavailable';
}

// The username of an "imaps://user@host" identifier, or the identifier.
export function sourceMailbox(identifier: string): string {
  const scheme = identifier.indexOf('://');
  if (scheme < 0) return identifier.toLowerCase();
  const rest = identifier.slice(scheme + 3);
  const at = rest.lastIndexOf('@');
  if (at <= 0) return identifier.toLowerCase();
  try {
    return decodeURIComponent(rest.slice(0, at)).toLowerCase();
  } catch {
    return rest.slice(0, at).toLowerCase();
  }
}
