import type { ReadingPaneSelection } from '../reader/ReadingPane.svelte';
import type { RelationshipTimelineRow } from '../../relationships/models';

/**
 * Pure helpers shared by RelationshipTimeline and RelationshipsWorkspace:
 * turning a TimelineRow into display text, a reading-pane entry selection,
 * and (for chat bursts) the UTC bounds of the burst's local calendar day.
 * Kept dependency-free so they're testable without mounting Svelte.
 */

/** "N messages in <title>" for chat_burst rows; the row's own title otherwise. */
export function timelineRowTitle(row: RelationshipTimelineRow): string {
  if (row.kind !== 'chat_burst') return row.title || '(untitled)';
  return `${row.message_count.toLocaleString()} messages in ${row.title || 'Conversation'}`;
}

/**
 * Adapts a TimelineRow into the EntryRow shape the reading pane's
 * `{kind: 'entry'}` selection expects. TimelineRow is a leaner shape (no message_type,
 * source_type/identifier, attachment counts, participants) than the
 * Everything-table EntryRow it's borrowing the reading pane from — fields
 * absent from the timeline response fall back to values derived from what
 * IS present (source_id as a string identifier, has_attachments as a 0/1
 * attachment count) rather than being fabricated.
 */
export function timelineRowToSelection(row: RelationshipTimelineRow): ReadingPaneSelection {
  const isBurst = row.kind === 'chat_burst';
  return {
    kind: 'entry',
    row: {
      key: row.key,
      kind: row.kind,
      title: timelineRowTitle(row),
      preview: row.preview,
      occurred_at: row.occurred_at,
      message_type: row.kind,
      source_type: '',
      source_identifier: String(row.source_id),
      source_id: row.source_id,
      conversation_type: isBurst ? 'chat' : row.kind,
      conversation_id: row.conversation_id,
      anchor_message_id: row.anchor_message_id,
      message_count: row.message_count,
      attachment_count: row.has_attachments ? 1 : 0,
      attachment_size: 0,
      has_attachments: row.has_attachments,
      deleted_from_source: false,
      match: {}
    }
  };
}

/**
 * UTC [start, end) bounds of the local calendar day containing `instant`,
 * per the host's local timezone (the same zone RelationshipsController
 * sends as `timezone()` — the day boundary the backend already bucketed the
 * burst on).
 */
export function localDayBoundsUTC(instant: string): { start: string; end: string } {
  const date = new Date(instant);
  const startOfDay = new Date(date.getFullYear(), date.getMonth(), date.getDate());
  const endOfDay = new Date(startOfDay);
  endOfDay.setDate(endOfDay.getDate() + 1);
  return { start: startOfDay.toISOString(), end: endOfDay.toISOString() };
}

/** Sortable "YYYY-MM" bucket key for `instant`, in the local timezone. */
export function monthGroupKey(instant: string): string {
  const date = new Date(instant);
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}`;
}

/** Human month header label for `instant`, in the local timezone. */
export function monthGroupLabel(instant: string): string {
  return new Intl.DateTimeFormat(undefined, { month: 'long', year: 'numeric' }).format(new Date(instant));
}
