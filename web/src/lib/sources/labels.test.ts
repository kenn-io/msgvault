import { describe, expect, it } from 'vitest';
import { sourceFailure, sourceMailbox, sourceRunState, sourceTypeLabel, syncUnavailableLabel } from './labels';

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

  it.each([
    [{ active_sync: null, latest_sync: null, scheduler_queued: true }, 'queued'],
    [{ active_sync: null, latest_sync: run('cancelled'), scheduler_queued: true }, 'queued'],
    [{ active_sync: null, latest_sync: run('failed'), scheduler_queued: true }, 'failed'],
    [{ active_sync: null, latest_sync: run('completed'), scheduler_queued: true }, 'queued'],
    [{ active_sync: null, latest_sync: run('completed'), scheduler_queued: true, scheduler_started_at: '2026-07-19T10:00:00Z' }, 'running'],
    [{ active_sync: null, latest_sync: run('running'), scheduler_queued: true }, 'running'],
    [{ active_sync: null, latest_sync: run('completed', 2), scheduler_queued: true }, 'partial'],
    [{ active_sync: null, latest_sync: run('cancelled') }, 'cancelled'],
    [{ active_sync: null, latest_sync: run('completed', 2) }, 'partial'],
    [{ active_sync: null, latest_sync: run('paused') }, 'paused'],
    [{ active_sync: run('running'), latest_sync: run('completed') }, 'running'],
    [{ active_sync: null, latest_sync: run('completed') }, 'succeeded'],
    [{ active_sync: null, latest_sync: run('failed') }, 'failed'],
    [{ active_sync: null, latest_sync: null }, undefined],
  ])('maps source run state %#', (source, want) => expect(sourceRunState(source)).toBe(want));

  it.each([
    ['imaps://work%40example.com@imap.example.com', 'work@example.com'],
    ['imaps://home%40example.com@imap.example.com', 'home@example.com']
  ])('identifies mailbox %s', (identifier, want) => expect(sourceMailbox(identifier)).toBe(want));
});

it('prefers the run error over the scheduler error', () => {
  expect(sourceFailure({ latest_sync: { ...run('paused'), error_message: 'Rate limit reached' }, scheduler_last_error: 'Scheduler stopped' })).toBe('Rate limit reached');
});
