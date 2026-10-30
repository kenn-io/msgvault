import type { ChipTone } from '@kenn-io/kit-ui';
import type { SourceStatus } from '../api/generated/models';
import type { RunState } from '../components/common/run-state';
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

const DAY_MS = 24 * 60 * 60 * 1000;

type StateSource = Pick<SourceStatus, 'active_sync' | 'latest_sync'> & Partial<Pick<SourceStatus, 'scheduler_queued'>>;

export function syncRunning(source: StateSource): boolean {
  return Boolean(source.active_sync || source.latest_sync?.status === 'running');
}

/** The run state a source shows, or undefined when it has never synced. */
export function sourceRunState(source: StateSource): RunState | undefined {
  const latest = source.latest_sync;
  if (syncRunning(source)) return 'running';
  if (source.scheduler_queued && (!latest || latest.status === 'cancelled')) return 'queued';
  if (!latest) return undefined;
  if (latest.status === 'cancelled') return 'cancelled';
  if (latest.status === 'failed') return 'failed';
  if (latest.status === 'completed') return latest.errors_count > 0 ? 'partial' : 'succeeded';
  return undefined;
}

/** Whether a failed or partial result is recent enough to raise the status dot. */
export function recentFailure(source: StateSource, now: number, windowDays = 7): boolean {
  const state = sourceRunState(source);
  if (state !== 'failed') return false;
  const at = Date.parse(source.latest_sync?.completed_at ?? source.latest_sync?.started_at ?? '');
  return Number.isFinite(at) && now - at <= windowDays * DAY_MS;
}

export interface SourceFailure {
  /** One plain sentence. */
  reason: string;
  /** A command that fixes it, shown with a copy button. */
  command?: string;
}

/** The stored reason a source last failed. */
export function sourceFailure(
  source: Pick<SourceStatus, 'latest_sync' | 'identifier' | 'source_type' | 'scheduler_last_error'>,
): SourceFailure | undefined {
  const message = (source.latest_sync?.status === 'failed' ? source.latest_sync.error_message : null) || source.scheduler_last_error;
  return message ? { reason: message } : undefined;
}

/** The sync command for an account that has no schedule. Only mail-style identifiers have one. */
export function syncCommand(source: Pick<SourceStatus, 'identifier' | 'sync_unavailable_reason'>): string | undefined {
  if (source.sync_unavailable_reason !== 'sync_not_configured') return undefined;
  return /^[^\s/@]+@[^\s/@]+$/.test(source.identifier) ? `msgvault sync ${source.identifier}` : undefined;
}

/** Source types whose schedule, filters, and attachment rules live in the settings catalog. */
export const SOURCE_SETTING_PREFIX: Readonly<Record<string, string>> = {
  beeper: 'beeper',
  slack: 'slack',
  inline: 'inline',
  discord: 'discord',
  teams: 'teams',
};


export function syncStatusChip(
  source: Pick<SourceStatus, 'active_sync' | 'latest_sync' | 'scheduler_queued'>
): { label: string; tone: ChipTone } {
  const latest = source.latest_sync;
  if (syncRunning(source)) return { label: 'Syncing', tone: 'info' };
  if (source.scheduler_queued && (!latest || latest.status === 'cancelled')) return { label: 'Queued', tone: 'info' };
  if (!latest) return { label: 'Never synced', tone: 'muted' };
  if (latest.status === 'completed') {
    return latest.errors_count > 0
      ? { label: 'Completed with errors', tone: 'warning' }
      : { label: 'Completed', tone: 'success' };
  }
  if (latest.status === 'failed') return { label: 'Failed', tone: 'danger' };
  return { label: sentenceCase(latest.status), tone: 'neutral' };
}

const SYNC_UNAVAILABLE: Readonly<Record<string, string>> = {
  source_not_schedulable: 'Imported file — nothing to sync',
  sync_already_running: 'Sync in progress',
  scheduler_unavailable: 'Scheduler unavailable',
  sync_not_configured: 'Sync not set up'
};


export function syncUnavailableLabel(code: string | undefined): string {
  return code !== undefined && Object.hasOwn(SYNC_UNAVAILABLE, code) ? SYNC_UNAVAILABLE[code]! : 'Sync unavailable';
}
