import { describe, expect, it } from 'vitest';
import type { CardDAVRunResponse, CardDAVStatusResponse } from '../api/generated/models';
import { contactFailure, contactRunState } from './contacts';

const status: CardDAVStatusResponse = {
  configured: true, available: true, credential_configured: true, enabled: true,
  scheduled: true, schedule: '0 * * * *', scheduler_running: false,
};
const run: CardDAVRunResponse = {
  id: 1, state: 'succeeded', started_at: '2026-07-19T10:00:00Z', finished_at: '2026-07-19T10:00:05Z',
  books: 1, created: 4, updated: 0, removed: 0, trigger: 'scheduled', full: false, account_id: 1,
};
const queued_at = '2026-07-20T10:00:00Z';

describe('contacts status', () => {
  it.each<[Partial<CardDAVStatusResponse>, string | undefined]>([
    [{ queued_at }, 'queued'],
    [{ queued_at, latest: run }, 'queued'],
    [{ queued_at, latest: { ...run, state: 'cancelled' } }, 'queued'],
    [{ scheduler_running: true }, 'running'],
    [{ queued_at, scheduler_running: true, latest: run }, 'running'],
    [{ active: { ...run, state: 'running' }, queued_at }, 'running'],
    [{}, undefined],
    [{ latest: run }, 'succeeded'],
  ])('derives the current run state for %j', (extra, want) => {
    expect(contactRunState({ ...status, ...extra })).toBe(want);
  });

  it.each(['failed', 'partial'] as const)('retains %s diagnostics during a scheduler handoff', state => {
    const latest = { ...run, state, error_message: 'Address book unavailable.\nUpstream details' };
    const queued = { ...status, queued_at, latest };
    expect(contactRunState(queued)).toBe(state);
    expect(contactFailure({ connection: 'default', orphaned: false, status: queued })).toBe('Address book unavailable.');

    const running = { ...queued, scheduler_running: true };
    expect(contactRunState(running)).toBe('running');
    expect(contactFailure({ connection: 'default', orphaned: false, status: running })).toBe('Address book unavailable.');
  });
});
