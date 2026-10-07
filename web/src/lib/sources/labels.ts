import type { ChipTone } from '@kenn-io/kit-ui';
import type { SourceStatus, SyncRunStatus } from '../api/generated/models';
import { sentenceCase } from '../explore/labels';

// Source types the daemon stores (internal/api/scheduler_jobs.go and the
// importers). An empty type shows no label: the identifier stands alone.
const SOURCE_TYPES: Readonly<Record<string, string>> = {
  '': '', gmail: 'Gmail', imap: 'IMAP', msmail: 'Microsoft mail', teams: 'Teams',
  discord: 'Discord', meeting_import: 'Meeting import', synctech_sms: 'SMS backup',
  imazing_csv: 'iMazing CSV', circleback: 'Circleback', plaud: 'Plaud', gcal: 'Google Calendar', muesli: 'Muesli',
  granola: 'Granola', notion_meetings: 'Notion meetings', pst: 'PST import',
  'apple-mail': 'Apple Mail', mbox: 'Mbox import', beeper: 'Beeper', slack: 'Slack', matrix: 'Matrix',
  eml: 'EML import', maildir: 'Maildir import', whatsapp: 'WhatsApp',
  apple_messages: 'Apple Messages', facebook_messenger: 'Facebook Messenger',
  'google-groups': 'Google Groups'
};
const SYNC_UNAVAILABLE: Readonly<Record<string, string>> = {
  source_not_schedulable: 'Imported file — nothing to sync',
  sync_already_running: 'Sync in progress',
  scheduler_unavailable: 'Scheduler unavailable',
  sync_not_configured: 'Sync not set up'
};

// Why a run could not prove it read its source (sync run `reason`).
const UNMEASURED_REASONS: Readonly<Record<string, string>> = {
  fda_denied: 'Full Disk Access is denied, so the source could not be read',
  source_missing: 'The source file is missing',
  writer_not_running: 'The app that writes this source was not running, so nothing new could have arrived',
  daemon_shutting_down: 'The daemon was shutting down before the source was read'
};

export function sourceTypeLabel(code: string): string {
  return Object.hasOwn(SOURCE_TYPES, code) ? SOURCE_TYPES[code]! : sentenceCase(code);
}

export function syncUnavailableLabel(code: string | undefined): string {
  return code !== undefined && Object.hasOwn(SYNC_UNAVAILABLE, code) ? SYNC_UNAVAILABLE[code]! : 'Sync unavailable';
}

// unmeasuredReasonLabel explains an unmeasured run, or returns undefined for a
// run that measured its source.
export function unmeasuredReasonLabel(
  run: Pick<SyncRunStatus, 'status' | 'outcome' | 'reason'> | null | undefined
): string | undefined {
  if (run?.outcome !== 'unmeasured') return undefined;
  const reason = run.reason;
  if (reason !== undefined && Object.hasOwn(UNMEASURED_REASONS, reason)) return UNMEASURED_REASONS[reason]!;
  return reason ? `Not measured: ${sentenceCase(reason).toLowerCase()}` : 'The source was not measured';
}

export function syncStatusChip(
  source: Pick<SourceStatus, 'active_sync' | 'latest_sync'>
): { label: string; tone: ChipTone } {
  const latest = source.latest_sync;
  if (source.active_sync || latest?.status === 'running') return { label: 'Syncing', tone: 'info' };
  if (!latest) return { label: 'Never synced', tone: 'muted' };
  // An unmeasured run did not prove it read the source: its zero new messages
  // say nothing, whatever its terminal status.
  if (latest.outcome === 'unmeasured') return { label: 'Not measured', tone: 'warning' };
  if (latest.status === 'completed') {
    return latest.errors_count > 0
      ? { label: 'Completed with errors', tone: 'warning' }
      : { label: 'Completed', tone: 'success' };
  }
  if (latest.status === 'failed') return { label: 'Failed', tone: 'danger' };
  return { label: sentenceCase(latest.status), tone: 'neutral' };
}
