import type { ChipTone } from '@kenn-io/kit-ui';
import type { SourceStatus } from '../api/generated/models';
import { sentenceCase } from '../explore/labels';

// Curated friendly labels for daemon source types. Other codes use
// sentenceCase, including twenty → Twenty. An empty type shows no label:
// the identifier stands alone.
const SOURCE_TYPES: Readonly<Record<string, string>> = {
  '': '', gmail: 'Gmail', imap: 'IMAP', msmail: 'Microsoft mail', teams: 'Teams',
  discord: 'Discord', meeting_import: 'Meeting import', synctech_sms: 'SMS backup',
  imazing_csv: 'iMazing CSV', circleback: 'Circleback', gcal: 'Google Calendar', muesli: 'Muesli',
  granola: 'Granola', notion_meetings: 'Notion meetings', pst: 'PST import',
  'apple-mail': 'Apple Mail', mbox: 'Mbox import', beeper: 'Beeper', slack: 'Slack',
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

export function sourceTypeLabel(code: string): string {
  return Object.hasOwn(SOURCE_TYPES, code) ? SOURCE_TYPES[code]! : sentenceCase(code);
}

export function syncUnavailableLabel(code: string | undefined): string {
  return code !== undefined && Object.hasOwn(SYNC_UNAVAILABLE, code) ? SYNC_UNAVAILABLE[code]! : 'Sync unavailable';
}

export function syncStatusChip(
  source: Pick<SourceStatus, 'active_sync' | 'latest_sync'>
): { label: string; tone: ChipTone } {
  const latest = source.latest_sync;
  if (source.active_sync || latest?.status === 'running') return { label: 'Syncing', tone: 'info' };
  if (!latest) return { label: 'Never synced', tone: 'muted' };
  if (latest.status === 'completed') {
    return latest.errors_count > 0
      ? { label: 'Completed with errors', tone: 'warning' }
      : { label: 'Completed', tone: 'success' };
  }
  if (latest.status === 'failed') return { label: 'Failed', tone: 'danger' };
  return { label: sentenceCase(latest.status), tone: 'neutral' };
}
