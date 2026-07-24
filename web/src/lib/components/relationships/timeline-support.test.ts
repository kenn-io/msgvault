import { describe, expect, it } from 'vitest';

import type { RelationshipTimelineRow } from '../../relationships/models';
import {
  localDayBoundsUTC,
  monthGroupKey,
  monthGroupLabel,
  timelineRowTitle,
  timelineRowToSelection
} from './timeline-support';

function emailRow(overrides: Partial<RelationshipTimelineRow> = {}): RelationshipTimelineRow {
  return {
    key: 'message:1',
    kind: 'email',
    occurred_at: '2026-07-18T12:00:00Z',
    preview: 'Preview text',
    source_id: 4,
    title: 'Subject line',
    has_attachments: false,
    message_count: 1,
    anchor_message_id: 91,
    conversation_id: 71,
    ...overrides
  };
}

function burstRow(overrides: Partial<RelationshipTimelineRow> = {}): RelationshipTimelineRow {
  return {
    key: 'burst:2:71:2026-07-18',
    kind: 'chat_burst',
    occurred_at: '2026-07-18T20:00:00Z',
    first_at: '2026-07-18T08:00:00Z',
    preview: 'Latest chat snippet',
    source_id: 2,
    title: 'Team Chat',
    has_attachments: true,
    message_count: 14,
    anchor_message_id: 500,
    conversation_id: 71,
    ...overrides
  };
}

describe('timelineRowTitle', () => {
  it('renders single-message rows with their own title', () => {
    expect(timelineRowTitle(emailRow())).toBe('Subject line');
  });

  it('renders chat bursts as "N messages in <title>"', () => {
    expect(timelineRowTitle(burstRow())).toBe('14 messages in Team Chat');
  });

  it('falls back to a generic conversation label when a burst has no title', () => {
    expect(timelineRowTitle(burstRow({ title: '' }))).toBe('14 messages in Conversation');
  });

  it('falls back to "(untitled)" for an untitled single-message row', () => {
    expect(timelineRowTitle(emailRow({ title: '' }))).toBe('(untitled)');
  });
});

describe('timelineRowToSelection', () => {
  it('adapts a single-message row into an entry selection', () => {
    const selection = timelineRowToSelection(emailRow());
    expect(selection.kind).toBe('entry');
    if (selection.kind !== 'entry') throw new Error('expected entry selection');
    expect(selection.row).toMatchObject({
      key: 'message:1',
      title: 'Subject line',
      preview: 'Preview text',
      message_type: 'email',
      conversation_type: 'email',
      conversation_id: 71,
      anchor_message_id: 91,
      source_id: 4,
      source_identifier: '4',
      attachment_count: 0,
      has_attachments: false
    });
  });

  it('adapts a chat burst into an entry selection carrying the burst title and a chat conversation type', () => {
    const selection = timelineRowToSelection(burstRow());
    if (selection.kind !== 'entry') throw new Error('expected entry selection');
    expect(selection.row.title).toBe('14 messages in Team Chat');
    expect(selection.row.conversation_type).toBe('chat');
    expect(selection.row.message_type).toBe('chat_burst');
    expect(selection.row.attachment_count).toBe(1);
    expect(selection.row.anchor_message_id).toBe(500);
    expect(selection.row.conversation_id).toBe(71);
  });
});

describe('localDayBoundsUTC', () => {
  it('returns the local midnight-to-midnight UTC bounds containing the instant', () => {
    const bounds = localDayBoundsUTC('2026-07-18T08:00:00Z');
    const start = new Date(bounds.start);
    const end = new Date(bounds.end);
    expect(start.getHours()).toBe(0);
    expect(end.getTime() - start.getTime()).toBe(24 * 60 * 60 * 1000);
    expect(new Date('2026-07-18T08:00:00Z').getTime()).toBeGreaterThanOrEqual(start.getTime());
    expect(new Date('2026-07-18T08:00:00Z').getTime()).toBeLessThan(end.getTime());
  });
});

describe('monthGroupKey / monthGroupLabel', () => {
  // Noon-UTC, mid-month instants stay within the same calendar day/month for
  // any real-world timezone offset (-12h..+14h), so these assertions hold
  // regardless of the test runner's local TZ.
  it('buckets same-month instants under one key and formats a readable label', () => {
    expect(monthGroupKey('2026-07-10T12:00:00Z')).toBe(monthGroupKey('2026-07-20T12:00:00Z'));
    expect(monthGroupLabel('2026-07-18T12:00:00Z')).toContain('2026');
  });

  it('distinguishes adjacent months', () => {
    expect(monthGroupKey('2026-07-15T12:00:00Z')).not.toBe(monthGroupKey('2026-08-15T12:00:00Z'));
  });
});
