import type { TimelineEntry, TimelinePerson, TimelineResult } from '../api/generated/models';

export const when = '2026-10-22T10:00:00Z';
export const person: TimelinePerson = {
  name: 'Alex Example', person_id: 7, participant_id: 17, tier: 1, tier_source: 'person',
};
const text = (text: string) => ({ text, truncated: false });
const base = (kind: string, ref: string): TimelineEntry => ({
  key: `${kind}:${ref}`, kind, ref, anchor_ref: ref, time: when, open: [],
});
export const entries: TimelineEntry[] = [
  { ...base('meeting', 'evt:1@20261022T100000Z'), meeting: {
    untrusted_text: text('Planning meeting'), all_day: false, attended: true,
    action_item_count: 1, attendee_count: 1, people_count: 1, attendees: [person], people: [person],
    linked_transcript_ids: [11],
  }, open: [{ tool: 'get_calendar_event', args: { ref: 'evt:1@20261022T100000Z' } },
    { tool: 'get_meeting_context', args: { message_ids: [11] } }] },
  { ...base('transcript', 'msg:12'), meeting: {
    untrusted_text: text('Transcript summary'), all_day: false, attended: true,
    action_item_count: 0, attendee_count: 0, people_count: 1, people: [person],
  }, open: [{ tool: 'get_meeting_context', args: { message_ids: [12] } }] },
  { ...base('burst', 'conv:3'), key: 'burst:1:3:2026-10-22', anchor_ref: 'burst:1:3:2026-10-22', burst: {
    untrusted_text: text('A conversation preview'), channel: 'email', first_at: when, last_at: when,
    inbound: 2, outbound: 1, people: [person], people_count: 1, latest_message_ids: [13], copies: { '13': 2 },
  }, open: [{ tool: 'list_messages', args: { conversation_id: 3 } }, { tool: 'get_message', args: { id: 13 } }] },
  { ...base('message', 'msg:14'), message: {
    untrusted_text: text('<img src=x onerror=alert(1)>'), direction: 'inbound', people: [person], people_count: 1,
  }, open: [{ tool: 'get_message', args: { id: 14 } }] },
  { ...base('call', 'msg:15'), call: { outcome: 'answered', duration_seconds: 120, people: [person], people_count: 1 },
    open: [{ tool: 'get_message', args: { id: 15 } }] },
  { ...base('loop', 'msg:16'), loop: { kind: 'reply_owed', age: '2d', person },
    open: [{ tool: 'get_message', args: { id: 16 } }] },
  { ...base('note', 'note:17'), anchor_ref: '2026-10-22', note: {
    untrusted_text: text('Authored daily note'), people: [person], people_count: 1,
  } },
];

export function golden(): TimelineResult {
  return structuredClone({
    mode: 'day', timezone: 'UTC', timezone_is_default: false, now: when,
    window: { from: '2026-10-21', to: '2026-10-22' },
    freshness: { as_of: when, stale_threshold: 86400, warnings: [], sources: [], projections: [] },
    omitted: { budget: {}, noise: 0, cold: 0 },
    open_loops: { items: [entries[5]!], truncated: false, coverage_incomplete: false, skipped_unparseable: 0 },
    upcoming: [entries[0]!],
    days: [{ date: '2026-10-22', meetings: [entries[1]!], bursts: [entries[2]!], calls: [entries[4]!], notes: [entries[6]!] }],
  });
}
