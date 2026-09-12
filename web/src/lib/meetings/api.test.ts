import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import { createMeetingsAPI, MeetingsAPIError } from './api';

describe('MeetingsAPI', () => {
  it('uses the production transport and generated request shapes for every meeting operation', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/context')) {
        return Response.json({
          schema_version: 1,
          format: 'json',
          content: '{"title":"Café ☕"}',
          content_bytes: 21,
          truncated: false,
          omitted_message_ids: [],
        });
      }
      if (path.endsWith('/actions')) {
        return Response.json({
          schema_version: 1,
          archive_uid: 'archive-test',
          rows: [],
          total_count: 0,
          coverage: {
            meeting_count: 1,
            available: 1,
            partial: 0,
            unsupported: 0,
            unavailable: 0,
          },
          scope: { kind: 'direct' },
        });
      }
      return Response.json({
        schema_version: 1,
        archive_uid: 'archive-test',
        totals: {
          meeting_count: 1,
          known_duration_count: 0,
          unknown_duration_count: 1,
          total_known_seconds: 0,
          average_known_seconds: null,
        },
        first_meeting_at: null,
        last_meeting_at: null,
        undated_count: 1,
        duration_by_basis: [],
        months: [],
        scope: { kind: 'direct' },
      });
    });
    const api = createMeetingsAPI(createAPIClient(fetchFn));
    const controller = new AbortController();

    const context = await api.context(
      { message_ids: [42], format: 'json', include_transcript: false },
      controller.signal,
    );
    const actions = await api.actions({ scope: { message_ids: [42] }, limit: 200 }, controller.signal);
    const metrics = await api.metrics({ scope: { message_ids: [42] } }, controller.signal);

    expect(context.content).toBe('{"title":"Café ☕"}');
    expect(actions.coverage.available).toBe(1);
    expect(metrics.totals.average_known_seconds).toBeNull();
    expect(requests.map((request) => new URL(request.url).pathname)).toEqual([
      '/api/v1/meetings/context',
      '/api/v1/meetings/actions',
      '/api/v1/meetings/metrics',
    ]);
    await expect(Promise.all(requests.map((request) => request.clone().json()))).resolves.toEqual([
      { message_ids: [42], format: 'json', include_transcript: false },
      { scope: { message_ids: [42] }, limit: 200 },
      { scope: { message_ids: [42] } },
    ]);
    controller.abort();
    expect(requests.every((request) => request.signal.aborted)).toBe(true);
  });

  it('retains HTTP status, server error code, and message for caller state decisions', async () => {
    const api = createMeetingsAPI(
      createAPIClient(async () =>
        Response.json(
          {
            error: 'meeting_scope_changed',
            message: 'The selected meeting population changed',
          },
          { status: 409 },
        ),
      ),
    );

    const failure = await api.actions({ scope: { message_ids: [42] } }).catch((cause: unknown) => cause);

    expect(failure).toBeInstanceOf(MeetingsAPIError);
    expect(failure).toMatchObject({
      status: 409,
      code: 'meeting_scope_changed',
      message: 'The selected meeting population changed',
    });
  });
});
