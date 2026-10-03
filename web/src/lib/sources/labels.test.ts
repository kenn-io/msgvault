import { describe, expect, it } from 'vitest';
import { sourceTypeLabel, syncStatusChip, syncUnavailableLabel } from './labels';

const run = (status: string, errors = 0) => ({
  id: 1, source_id: 1, started_at: '2026-07-19T10:00:00Z', completed_at: null, status,
  messages_processed: 1, messages_added: 1, messages_updated: 0, errors_count: errors, error_message: null
});

describe('source labels', () => {
  it.each([
    ['gmail', 'Gmail'], ['imap', 'IMAP'], ['msmail', 'Microsoft mail'], ['teams', 'Teams'],
    ['discord', 'Discord'], ['meeting_import', 'Meeting import'], ['synctech_sms', 'SMS backup'],
    ['imazing_csv', 'iMazing CSV'], ['circleback', 'Circleback'], ['gcal', 'Google Calendar'],
    ['muesli', 'Muesli'], ['granola', 'Granola'], ['notion_meetings', 'Notion meetings'],
    ['pst', 'PST import'], ['apple-mail', 'Apple Mail'], ['mbox', 'Mbox import'],
    ['beeper', 'Beeper'], ['slack', 'Slack'], ['matrix', 'Matrix'], ['eml', 'EML import'], ['maildir', 'Maildir import'],
    ['whatsapp', 'WhatsApp'], ['apple_messages', 'Apple Messages'],
    ['facebook_messenger', 'Facebook Messenger'], ['google-groups', 'Google Groups'],
    ['', ''], ['future_source', 'Future source'], ['constructor', 'Constructor']
  ])('names source type %j', (code, want) => expect(sourceTypeLabel(code)).toBe(want));

  it.each([
    ['source_not_schedulable', 'Imported file — nothing to sync'],
    ['sync_already_running', 'Sync in progress'],
    ['scheduler_unavailable', 'Scheduler unavailable'],
    ['sync_not_configured', 'Sync not set up'],
    ['sync_unavailable', 'Sync unavailable'],
    ['future_reason', 'Sync unavailable'],
    [undefined, 'Sync unavailable']
  ])('names sync reason %j', (code, want) => expect(syncUnavailableLabel(code)).toBe(want));

  it('maps each latest result to one status chip', () => {
    expect(syncStatusChip({ active_sync: run('running'), latest_sync: run('completed') }))
      .toEqual({ label: 'Syncing', tone: 'info' });
    expect(syncStatusChip({ active_sync: null, latest_sync: run('completed') }))
      .toEqual({ label: 'Completed', tone: 'success' });
    expect(syncStatusChip({ active_sync: null, latest_sync: run('completed', 2) }))
      .toEqual({ label: 'Completed with errors', tone: 'warning' });
    expect(syncStatusChip({ active_sync: null, latest_sync: run('failed') }))
      .toEqual({ label: 'Failed', tone: 'danger' });
    expect(syncStatusChip({ active_sync: null, latest_sync: null }))
      .toEqual({ label: 'Never synced', tone: 'muted' });
  });
});
