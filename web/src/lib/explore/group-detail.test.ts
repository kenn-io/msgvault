import { describe, expect, it, vi } from 'vitest';

import type { ExploreAPI } from './api';
import { findGroupDetail } from './group-detail';
import type {
  ExploreCacheUnavailable,
  ExploreGroupLoadResult,
  ExploreGroupRow,
  ExplorePredicate
} from './models';

function groupRow(key: string, overrides: Partial<ExploreGroupRow> = {}): ExploreGroupRow {
  return {
    key,
    label: `Group ${key}`,
    count: 1,
    estimated_bytes: 10,
    latest_at: '2026-07-18T12:00:00Z',
    ...overrides
  };
}

function ready(rows: ExploreGroupRow[], nextCursor?: string): ExploreGroupLoadResult {
  return {
    status: 'ready',
    result: {
      rows,
      totalCount: rows.length,
      cacheRevision: 'cache-1',
      searchProvenance: {},
      ...(nextCursor === undefined ? {} : { nextCursor })
    }
  };
}

/**
 * Mocks the daemon's keyed-lookup contract: group_key returns the exact match
 * (any rank), while unkeyed requests page the ranked listing. A paging client
 * walking this listing would never reach a group ranked beyond its page cap.
 */
function keyedListing(ranked: ExploreGroupRow[]): ExploreAPI['groups'] {
  return vi.fn<ExploreAPI['groups']>(async (predicate) => {
    if (predicate.group_key) {
      return ready(ranked.filter((row) => row.key === predicate.group_key));
    }
    const offset = predicate.cursor ? Number(predicate.cursor) : 0;
    const page = ranked.slice(offset, offset + (predicate.limit ?? 100));
    const next = offset + page.length;
    return ready(page, next < ranked.length ? String(next) : undefined);
  });
}

const predicate: ExplorePredicate = {
  filters: [{ dimension: 'participant', values: ['2'] }],
  limit: 500
};

describe('findGroupDetail', () => {
  it('resolves the requested key with one keyed request even when a co-participant ranks first', async () => {
    const groups = keyedListing([
      groupRow('1', { label: 'Alice', count: 8 }),
      groupRow('2', { label: 'Bob', count: 5 })
    ]);

    const lookup = await findGroupDetail({ groups }, predicate, 'participant', '2');

    expect(lookup).toEqual({ status: 'found', row: groupRow('2', { label: 'Bob', count: 5 }) });
    expect(groups).toHaveBeenCalledTimes(1);
    expect(vi.mocked(groups).mock.calls[0]![0]).toMatchObject({
      filters: [{ dimension: 'participant', values: ['2'] }],
      group_key: '2',
      limit: 1
    });
    expect(vi.mocked(groups).mock.calls[0]![1]).toBe('participant');
  });

  it('resolves a key ranked beyond any bounded page walk', async () => {
    const ranked = Array.from({ length: 600 }, (_, index) => groupRow(`other-${index}`, { count: 601 - index }));
    ranked.push(groupRow('2', { label: 'Bob', count: 1 }));
    const groups = keyedListing(ranked);

    const lookup = await findGroupDetail({ groups }, predicate, 'participant', '2');

    expect(lookup).toEqual({ status: 'found', row: groupRow('2', { label: 'Bob', count: 1 }) });
    expect(groups).toHaveBeenCalledTimes(1);
  });

  it('reports authoritative missing when the key matches no group', async () => {
    const groups = keyedListing([groupRow('1', { label: 'Alice' })]);

    const lookup = await findGroupDetail({ groups }, predicate, 'participant', '2');

    expect(lookup).toEqual({ status: 'missing' });
    expect(groups).toHaveBeenCalledTimes(1);
  });

  it('reports missing instead of adopting a wrong row from a daemon ignoring group_key', async () => {
    const groups = vi.fn<ExploreAPI['groups']>(async () => ready([groupRow('1', { label: 'Alice', count: 8 })]));

    const lookup = await findGroupDetail({ groups }, predicate, 'participant', '2');

    expect(lookup).toEqual({ status: 'missing' });
  });

  it('surfaces cache-unavailable verbatim', async () => {
    const unavailable: ExploreCacheUnavailable = {
      error: 'cache_unavailable',
      message: 'The analytical cache is absent.',
      readiness: 'absent',
      recovery_action: 'msgvault build-cache'
    };
    const groups = vi.fn<ExploreAPI['groups']>(async () => ({ status: 'unavailable', unavailable }));

    const lookup = await findGroupDetail({ groups }, predicate, 'participant', '2');

    expect(lookup).toEqual({ status: 'unavailable', unavailable });
  });

  it('forwards the abort signal to the keyed request', async () => {
    const controller = new AbortController();
    const groups = keyedListing([groupRow('2')]);

    await findGroupDetail({ groups }, predicate, 'participant', '2', controller.signal);

    expect(groups).toHaveBeenCalledTimes(1);
    expect(vi.mocked(groups).mock.calls[0]![2]).toBe(controller.signal);
  });
});
