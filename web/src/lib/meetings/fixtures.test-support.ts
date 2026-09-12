import type { ActionRow, ActionsPage, Metrics } from '../api/generated/models';

export function meetingAction(overrides: Partial<ActionRow> = {}): ActionRow {
  return {
    meeting: { message_id: 42, conversation_id: 8, source_id: 3, source_type: 'zoom', source_identifier: 'example-zoom',
      source_message_id: 'source-42', title: 'Archived review', occurred_at: '2026-01-01T00:00:00Z', archive_path: '/api/v1/messages/42' },
    action: { ordinal: 0, locator: 'action:0', origin: 'source', title: 'Prepare review', status: 'pending' },
    ...overrides
  };
}

export function meetingMetrics(overrides: Partial<Metrics> = {}): Metrics {
  return {
    schema_version: 1, archive_uid: 'archive-test', scope: { kind: 'direct' },
    totals: { meeting_count: 4, known_duration_count: 3, unknown_duration_count: 1,
      total_known_seconds: 6000, average_known_seconds: 2000 },
    first_meeting_at: '2026-01-02T10:00:00Z', last_meeting_at: '2026-02-03T10:00:00Z', undated_count: 0,
    duration_by_basis: [
      { basis: 'provider', count: 1, total_seconds: 1800 },
      { basis: 'scheduled', count: 1, total_seconds: 3600 },
      { basis: 'transcript_span', count: 1, total_seconds: 600 }
    ],
    months: [
      { month: '2026-01', totals: { meeting_count: 2, known_duration_count: 2, unknown_duration_count: 0, total_known_seconds: 5400, average_known_seconds: 2700 } },
      { month: '2026-02', totals: { meeting_count: 2, known_duration_count: 1, unknown_duration_count: 1, total_known_seconds: 600, average_known_seconds: 600 } }
    ],
    ...overrides
  };
}

export function meetingActions(overrides: Partial<ActionsPage> = {}): ActionsPage {
  return { schema_version: 1, archive_uid: 'archive-test', scope: { kind: 'direct' }, rows: [], total_count: 0,
    coverage: { meeting_count: 4, available: 1, partial: 1, unsupported: 1, unavailable: 1 }, ...overrides };
}

/** Meeting endpoint defaults for tests exercising adjacent workspace behavior. */
export function meetingFixtureResponse(path: string): Response | undefined {
  if (path === '/api/v1/meetings/metrics') return Response.json(meetingMetrics());
  if (path === '/api/v1/meetings/actions') return Response.json(meetingActions());
  return undefined;
}
