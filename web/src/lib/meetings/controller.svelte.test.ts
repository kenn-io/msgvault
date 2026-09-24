import { describe, expect, it, vi } from 'vitest';
import type { ActionsPage, Metrics } from '../api/generated/models';
import { MeetingsAPIError, type MeetingsAPI } from './api';
import { MeetingsController, type MeetingPanelScope } from './controller.svelte';
import { meetingAction, meetingActions, meetingMetrics } from './fixtures.test-support';

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
function api(): MeetingsAPI {
  return { context: vi.fn(), actions: vi.fn(async () => meetingActions()), metrics: vi.fn(async () => meetingMetrics()) };
}
const person: MeetingPanelScope = { kind: 'direct', scope: { person_id: 7 } };
const explore: MeetingPanelScope = { kind: 'explore', explore: { predicate: { query: 'budget', search_mode: 'hybrid', filters: [
  { dimension: 'domain', values: ['first.example'] }, { dimension: 'domain', values: ['second.example'] },
  { dimension: 'participant', values: ['3'] }, { dimension: 'participant', values: ['7'] }
] }, cache_revision: 'cache-1', search_provenance: { lexical_index_revision: 'lex-1', vector_generation: 2 }, candidate_snapshot_id: 'candidate-1' } };

describe('MeetingsController', () => {
  it('loads metrics and actions under the exact generated scope and defaults to all source statuses', async () => {
    const transport = api();
    const controller = new MeetingsController(transport);
    await controller.setScope(explore);
    expect(transport.metrics).toHaveBeenCalledWith({ explore: explore.explore }, expect.any(AbortSignal));
    expect(transport.actions).toHaveBeenCalledWith({ explore: explore.explore, limit: 50 }, expect.any(AbortSignal));
    expect(controller.metrics?.totals.total_known_seconds).toBe(6000);
    expect(controller.actions?.coverage).toEqual(meetingActions().coverage);
  });

  it('clears previous values and cursor, aborts both requests, and ignores ignored-abort fulfillment', async () => {
    const transport = api();
    const oldMetrics = deferred<Metrics>();
    const oldActions = deferred<ActionsPage>();
    vi.mocked(transport.metrics).mockReturnValueOnce(oldMetrics.promise);
    vi.mocked(transport.actions).mockReturnValueOnce(oldActions.promise);
    const controller = new MeetingsController(transport);
    const first = controller.setScope(person, 'revision-1');
    const oldSignal = vi.mocked(transport.metrics).mock.calls[0]![1]!;
    await controller.setScope({ kind: 'direct', scope: { person_id: 8 } });
    expect(oldSignal.aborted).toBe(true);
    oldMetrics.resolve(meetingMetrics({ totals: { ...meetingMetrics().totals, meeting_count: 99 } }));
    oldActions.resolve(meetingActions({ total_count: 99, next_cursor: 'stale' }));
    await first;
    expect(controller.metrics?.totals.meeting_count).toBe(4);
    expect(controller.actions?.total_count).toBe(0);
    expect(controller.actions?.next_cursor).toBeUndefined();
  });

  it('restarts page one for same-person identity rebind or merge without substituting participant IDs', async () => {
    const transport = api();
    vi.mocked(transport.actions).mockResolvedValue(meetingActions({ next_cursor: 'next-page' }));
    const controller = new MeetingsController(transport);
    await controller.setScope(person, 'revision-1:3,7');
    await controller.loadMore();
    expect(vi.mocked(transport.actions).mock.calls[1]![0].cursor).toBe('next-page');
    const pending = deferred<ActionsPage>();
    vi.mocked(transport.actions).mockReturnValueOnce(pending.promise);
    const reload = controller.setScope(person, 'revision-2:7,9');
    expect(controller.actions).toBeUndefined();
    expect(vi.mocked(transport.actions).mock.lastCall![0]).toEqual({ scope: { person_id: 7 }, limit: 50 });
    pending.resolve(meetingActions());
    await reload;
    expect(transport.metrics).toHaveBeenCalledTimes(2);
  });

  it('normalizes object order and set-valued lists but fingerprints every search authority field', async () => {
    const transport = api(); const controller = new MeetingsController(transport);
    await controller.setScope({ kind: 'direct', scope: { domains: ['Example.test', 'other.test'], source_ids: [2, 1] } });
    await controller.setScope({ kind: 'direct', scope: { source_ids: [1, 2], domains: ['other.test', 'example.test'] } });
    expect(transport.metrics).toHaveBeenCalledTimes(1);
    await controller.setScope(explore);
    await controller.setScope({ kind: 'explore', explore: { ...explore.explore, candidate_snapshot_id: 'candidate-2' } });
    expect(transport.metrics).toHaveBeenCalledTimes(3);
  });

  it('clears pagination when source status or exact assignee email changes', async () => {
    const transport = api(); const controller = new MeetingsController(transport);
    vi.mocked(transport.actions).mockResolvedValue(meetingActions({ next_cursor: 'next' }));
    await controller.setScope(person);
    await controller.loadMore();
    await controller.setFilters({ status: 'pending', assigneeEmail: ' Person@Example.test ' });
    expect(vi.mocked(transport.actions).mock.lastCall![0]).toEqual({ scope: { person_id: 7 }, limit: 50, status: 'pending', assignee_email: 'person@example.test' });
  });

  it('reconciles repeated stable action keys with newer evidence while retaining distinct actions', async () => {
    const first = meetingAction();
    const retained = meetingAction({ meeting: { ...first.meeting, message_id: 44 } });
    const updated = meetingAction({ meeting: { ...first.meeting, title: 'Updated archived title' },
      action: { ...first.action, title: 'Review prepared', status: 'completed' } });
    const anotherAction = meetingAction({ action: { ...first.action, ordinal: 1, locator: 'action:1' } });
    const anotherMeeting = meetingAction({ meeting: { ...first.meeting, message_id: 43 } });
    const next = meetingActions({ rows: [updated, anotherAction, anotherMeeting], total_count: 4,
      coverage: { meeting_count: 3, available: 3, partial: 0, unsupported: 0, unavailable: 0 }, next_cursor: 'third-page' });
    const transport = api();
    vi.mocked(transport.actions).mockResolvedValueOnce(meetingActions({ rows: [first, retained], next_cursor: 'second-page' })).mockResolvedValueOnce(next);
    const controller = new MeetingsController(transport);
    await controller.setScope(person);
    await controller.loadMore();
    expect(controller.actions).toEqual({ ...next, rows: [updated, retained, anotherAction, anotherMeeting] });
  });

  it.each([
    [409, 'meeting_scope_changed', 'reload', /Reload/],
    [409, 'cache_revision_mismatch', 'reload', /Reload/],
    [400, 'meeting_scope_too_large', 'narrow', /narrower filters/i],
    [400, 'candidate_pool_saturated', 'narrow', /narrower filters/i],
    [404, 'not_found', 'unavailable', /daemon/i]
  ])('surfaces %s %s once with actionable recovery', async (status, code, recovery, copy) => {
    const transport = api(); const controller = new MeetingsController(transport);
    vi.mocked(transport.metrics).mockRejectedValue(new MeetingsAPIError(status, { error: code, message: 'Server detail' }));
    await controller.setScope(explore);
    expect(controller.metricsError?.recovery).toBe(recovery);
    expect(controller.metricsError?.message).toMatch(copy);
    expect(transport.metrics).toHaveBeenCalledTimes(1);
  });
});
