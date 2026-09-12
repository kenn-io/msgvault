import { describe, expect, it, vi } from 'vitest';
import { meetingMetrics } from './fixtures.test-support';
import { createAPIClient } from '../api/client';
import type { MessageDetail } from '../api/generated/models';
import { ArchiveMeetingNavigation, archiveMeetingSelection, parseArchiveMeetingSelection } from './archive-navigation.svelte';

const detail: MessageDetail = { id: 42, conversation_id: 8, message_type: 'meeting_transcript', subject: 'Archived review', sent_at: '2026-01-01T00:00:00Z', from: 'Example', to: [], body: 'Archived transcript', labels: [], attachments: [], has_attachments: false, size_bytes: 10, snippet: 'Archived' };

function eligibility(count = 1): Response {
  return Response.json(meetingMetrics({ totals: { meeting_count: count, known_duration_count: 0, unknown_duration_count: count, total_known_seconds: 0, average_known_seconds: null } }));
}

describe('archive meeting navigation', () => {
  it('parses only positive safe archive IDs without impersonating an Explore row', () => {
    expect(archiveMeetingSelection(42)).toBe('archive-meeting:42');
    expect(parseArchiveMeetingSelection('archive-meeting:42')).toBe(42);
    for (const value of [null, 'message:42', 'archive-meeting:0', 'archive-meeting:-1', 'archive-meeting:2.5', 'archive-meeting:9007199254740992']) expect(parseArchiveMeetingSelection(value)).toBeUndefined();
  });
  it('opens only the exact authoritative archive detail and validates conversation and type', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => new URL((input as Request).url).pathname.endsWith('/meetings/metrics') ? eligibility() : Response.json(detail));
    const navigation = new ArchiveMeetingNavigation(createAPIClient(fetchFn));
    expect(await navigation.load(42, 8)).toEqual(detail);
    const request = fetchFn.mock.calls[0]![0] as Request;
    expect(new URL(request.url).pathname).toBe('/api/v1/messages/42');
    expect(request.method).toBe('GET');
    expect(await navigation.load(42, 9)).toBeUndefined();
    expect(navigation.error).toMatch(/conversation/);
    fetchFn.mockResolvedValue(Response.json({ ...detail, message_type: 'email' }));
    expect(await navigation.load(42)).toBeUndefined();
    expect(navigation.error).toMatch(/meeting transcript/);
  });
  it('retains source-deleted archive detail while excluding a locally hidden meeting', async () => {
    const archived = { ...detail, deleted_at: '2026-02-01T00:00:00Z' };
    let count = 1;
    const fetchFn = vi.fn<typeof fetch>(async (input) => new URL((input as Request).url).pathname.endsWith('/meetings/metrics') ? eligibility(count) : Response.json(archived));
    const navigation = new ArchiveMeetingNavigation(createAPIClient(fetchFn));
    expect(await navigation.load(42, 8)).toEqual(archived);
    const request = fetchFn.mock.calls.map(([input]) => input as Request).find((request) => request.url.endsWith('/meetings/metrics'))!;
    expect(request).toBeDefined();
    expect(await request.clone().json()).toEqual({ scope: { message_ids: [42] } });
    count = 0;
    expect(await navigation.load(42)).toBeUndefined();
    expect(navigation.detail).toBeUndefined();
    expect(navigation.error).toBe('This archived meeting is no longer available.');
  });
  it('reports eligibility request failures without presenting them as an unavailable meeting', async () => {
    const client = createAPIClient(async (input) => new URL((input as Request).url).pathname.endsWith('/meetings/metrics')
      ? Response.json({ error: 'internal_error', message: 'Meeting eligibility could not be checked.' }, { status: 500 }) : Response.json(detail));
    const navigation = new ArchiveMeetingNavigation(client);
    expect(await navigation.load(42)).toBeUndefined();
    expect(navigation.error).toBe('Meeting eligibility could not be checked.');
  });
  it('aborts and discards stale eligibility results when the next meeting owns navigation', async () => {
    let resolveFirst!: (response: Response) => void;
    let firstEligibility!: Request;
    const client = createAPIClient(async (input) => {
      const request = input as Request;
      const path = new URL(request.url).pathname;
      if (path.endsWith('/meetings/metrics')) {
        const body = await request.clone().json();
        if (body.scope.message_ids[0] === 42) {
          firstEligibility = request;
          return new Promise<Response>((resolve) => { resolveFirst = resolve; });
        }
        return eligibility();
      }
      return Response.json({ ...detail, id: path.endsWith('/43') ? 43 : 42 });
    });
    const navigation = new ArchiveMeetingNavigation(client);
    const first = navigation.load(42);
    await vi.waitFor(() => expect(resolveFirst).toBeDefined());
    expect(await navigation.load(43)).toEqual({ ...detail, id: 43 });
    expect(firstEligibility.signal.aborted).toBe(true);
    resolveFirst(eligibility(0));
    expect(await first).toBeUndefined();
    expect(navigation.detail?.id).toBe(43);
    expect(navigation.error).toBe('');
    expect(navigation.loading).toBe(false);
  });
  it('discards stale and missing getMessage responses without leaving a previous meeting visible', async () => {
    let resolveFirst!: (response: Response) => void;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL((input as Request).url).pathname;
      if (path.endsWith('/meetings/metrics')) return eligibility();
      if (path.endsWith('/messages/42')) return new Promise<Response>((resolve) => { resolveFirst = resolve; });
      if (path.endsWith('/messages/43')) return Response.json({ ...detail, id: 43 });
      return Response.json({ error: 'not_found', message: 'Archived meeting missing.' }, { status: 404 });
    });
    const navigation = new ArchiveMeetingNavigation(createAPIClient(fetchFn));
    const first = navigation.load(42);
    const firstRequest = fetchFn.mock.calls[0]![0] as Request;
    await navigation.load(43);
    expect(firstRequest.signal.aborted).toBe(true);
    resolveFirst(Response.json(detail));
    expect(await first).toBeUndefined();
    expect(navigation.detail?.id).toBe(43);
    await navigation.load(44);
    expect(navigation.detail).toBeUndefined();
    expect(navigation.error).toBe('Archived meeting missing.');
  });
});
