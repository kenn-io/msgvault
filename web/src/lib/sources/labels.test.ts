import { describe, expect, it } from 'vitest';
import { sourceFailure, sourceRunState, sourceTypeLabel, syncStatusChip, syncUnavailableLabel } from './labels';

const run = (status: string, errors = 0) => ({
  id: 1, source_id: 1, started_at: '2026-07-19T10:00:00Z', completed_at: null, status,
  messages_processed: 1, messages_added: 1, messages_updated: 0, errors_count: errors, error_message: null
});

describe('source labels', () => {
  it.each([
    ['gmail', 'Gmail'], ['imap', 'IMAP'], ['msmail', 'Microsoft mail'], ['teams', 'Teams'],
    ['discord', 'Discord'], ['meeting_import', 'Meeting import'], ['synctech_sms', 'SMS backup'],
    ['imazing_csv', 'iMazing CSV'], ['circleback', 'Circleback'], ['gcal', 'Google Calendar'],
    ['plaud', 'Plaud'], ['muesli', 'Muesli'], ['granola', 'Granola'], ['notion_meetings', 'Notion meetings'], ['twilio', 'Twilio'], ['twenty', 'Twenty'],
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

  it('maps run states for rows and status tones for the job indicator', () => {
    expect(sourceRunState({ scheduler_queued: true })).toBe('queued');
    expect(sourceRunState({ latest_sync: run('cancelled') })).toBe('cancelled');
    expect(sourceRunState({ latest_sync: run('completed', 2) })).toBe('partial');
    expect(sourceRunState({ latest_sync: run('paused') })).toBe('paused');
    expect(sourceRunState({ active_sync: run('running'), latest_sync: run('completed') })).toBe('running');
    expect(sourceRunState({ latest_sync: run('completed') })).toBe('succeeded');
    expect(sourceRunState({ latest_sync: run('failed') })).toBe('failed');
    expect(sourceRunState({ latest_sync: null })).toBeUndefined();
    expect(syncStatusChip({ latest_sync: run('failed') }).tone).toBe('danger');
    expect(syncStatusChip({ latest_sync: run('completed') }).tone).toBe('success');
  });
});

it('preserves paused run errors before scheduler errors', () => {
  expect(sourceFailure({ latest_sync: { ...run('paused'), error_message: 'Rate limit reached' }, identifier: 'archive@example.com', source_type: 'omi', scheduler_last_error: 'Scheduler stopped' })).toEqual({ reason: 'Rate limit reached' });
});
