import type { SourceStatus, SyncRunStatus } from '../api/generated/models';
import { sentenceCase } from '../explore/labels';

// Curated friendly labels for daemon source types. Other codes use
// sentenceCase, including twenty → Twenty. An empty type shows no label:
// the identifier stands alone.
const SOURCE_TYPES: Readonly<Record<string, string>> = {
  '': '', gmail: 'Gmail', imap: 'IMAP', msmail: 'Microsoft mail', teams: 'Teams',
  discord: 'Discord', meeting_import: 'Meeting import', synctech_sms: 'SMS backup',
  imazing_csv: 'iMazing CSV', circleback: 'Circleback', plaud: 'Plaud', pocket: 'Pocket', gcal: 'Google Calendar', muesli: 'Muesli',
  granola: 'Granola', notion_meetings: 'Notion meetings', twilio: 'Twilio', pst: 'PST import',
  'apple-mail': 'Apple Mail', mbox: 'Mbox import', beeper: 'Beeper', slack: 'Slack', inline: 'Inline', matrix: 'Matrix',
  eml: 'EML import', maildir: 'Maildir import', whatsapp: 'WhatsApp',
  apple_messages: 'Apple Messages', facebook_messenger: 'Facebook Messenger',
  'google-groups': 'Google Groups'
};

// Why a run could not prove it read its source (sync run `reason`).
const UNMEASURED_REASONS: Readonly<Record<string, string>> = {
  fda_denied: 'Full Disk Access is denied, so the source could not be read',
  source_missing: 'The source file is missing',
  writer_not_running: 'The app that writes this source was not running, so nothing new could have arrived'
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
  // An unmeasured run did not prove it read the source: its zero new messages
  // say nothing, whatever its terminal status.
  if (latest.outcome === 'unmeasured') return 'unmeasured';
  if (latest.status === 'paused') return 'paused';
  if (latest.status === 'cancelled') return 'cancelled';
  if (latest.status === 'failed') return 'failed';
  if (latest.status === 'completed') return latest.errors_count > 0 ? 'partial' : 'succeeded';
  return undefined;
}

/** Why a run did not measure its source, or undefined for a run that did. */
export function unmeasuredReasonLabel(
  run: Pick<SyncRunStatus, 'outcome' | 'reason'> | null | undefined
): string | undefined {
  if (run?.outcome !== 'unmeasured') return undefined;
  const reason = run.reason;
  if (reason !== undefined && Object.hasOwn(UNMEASURED_REASONS, reason)) return UNMEASURED_REASONS[reason]!;
  return reason ? `Not measured: ${sentenceCase(reason).toLowerCase()}` : 'The source was not measured';
}

/** The stored reason a source last failed. */
export function sourceFailure(
  source: Pick<SourceStatus, 'latest_sync' | 'scheduler_last_error'>,
): string | undefined {
  const message = source.latest_sync?.error_message || source.scheduler_last_error;
  return message || undefined;
}

/** The sync command for a source that has no schedule. */
export function syncCommand(source: Pick<SourceStatus, 'id' | 'source_type' | 'sync_unavailable_reason' | 'last_successful_sync'>): string | undefined {
  if (source.sync_unavailable_reason !== 'sync_not_configured') return undefined;
  if (!['gmail', 'imap', 'msmail'].includes(source.source_type)) return undefined;
  return `msgvault ${source.source_type !== 'msmail' && !source.last_successful_sync ? 'sync-full' : 'sync'} --source-id ${source.id}`;
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
