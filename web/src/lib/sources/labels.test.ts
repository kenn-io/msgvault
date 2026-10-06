import { describe, expect, it } from 'vitest';
import { sourceTypeLabel, syncStatusChip, syncUnavailableLabel, unmeasuredReasonLabel } from './labels';

const run = (status: string, errors = 0, extra: Record<string, unknown> = {}) => ({
  id: 1, source_id: 1, started_at: '2026-07-19T10:00:00Z', completed_at: null, status,
  messages_processed: 1, messages_added: 1, messages_updated: 0, errors_count: errors, error_message: null,
  ...extra
});

describe('source labels', () => {
  it.each([
    ['gmail', 'Gmail'], ['imap', 'IMAP'], ['msmail', 'Microsoft mail'], ['teams', 'Teams'],
    ['discord', 'Discord'], ['meeting_import', 'Meeting import'], ['synctech_sms', 'SMS backup'],
    ['imazing_csv', 'iMazing CSV'], ['circleback', 'Circleback'], ['gcal', 'Google Calendar'],
    ['plaud', 'Plaud'], ['muesli', 'Muesli'], ['granola', 'Granola'], ['notion_meetings', 'Notion meetings'],
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

  it('never shows an unmeasured run as completed', () => {
    const unmeasured = run('completed', 0, { outcome: 'unmeasured', reason: 'writer_not_running' });
    expect(syncStatusChip({ active_sync: null, latest_sync: unmeasured }))
      .toEqual({ label: 'Not measured', tone: 'warning' });
    expect(syncStatusChip({ active_sync: null, latest_sync: { ...unmeasured, status: 'failed' } }))
      .toEqual({ label: 'Not measured', tone: 'warning' });
    expect(syncStatusChip({ active_sync: null, latest_sync: run('completed', 0, { outcome: 'completed' }) }))
      .toEqual({ label: 'Completed', tone: 'success' });
  });

  it.each([
    ['writer_not_running', 'The app that writes this source was not running, so nothing new could have arrived'],
    ['fda_denied', 'Full Disk Access is denied, so the source could not be read'],
    ['future_reason', 'Not measured: future reason'],
    [undefined, 'The source was not measured']
  ])('explains unmeasured reason %j', (reason, want) =>
    expect(unmeasuredReasonLabel(run('completed', 0, { outcome: 'unmeasured', reason }))).toBe(want));

  it('explains nothing for a measured run', () => {
    expect(unmeasuredReasonLabel(run('completed', 0, { outcome: 'completed' }))).toBeUndefined();
    expect(unmeasuredReasonLabel(run('failed', 0, { outcome: 'failed', reason: 'x' }))).toBeUndefined();
    expect(unmeasuredReasonLabel(null)).toBeUndefined();
  });
});
