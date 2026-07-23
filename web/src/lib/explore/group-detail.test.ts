import { describe, expect, it, vi } from 'vitest';

import type { ExploreAPI } from './api';
import {
  GROUP_DETAIL_MAX_PAGES,
  GROUP_DETAIL_PAGE_SIZE,
  findGroupDetail
} from './group-detail';
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

function readyPage(rows: ExploreGroupRow[], nextCursor?: string): ExploreGroupLoadResult {
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

const predicate: ExplorePredicate = {
  filters: [{ dimension: 'participant', values: ['2'] }],
  limit: 500
};

describe('findGroupDetail', () => {
  it('resolves the requested key even when a co-participant ranks first', async () => {
    const groups = vi.fn<ExploreAPI['groups']>(async () => readyPage([
      groupRow('1', { label: 'Alice', count: 8 }),
      groupRow('2', { label: 'Bob', count: 5 })
    ]));

    const lookup = await findGroupDetail({ groups }, predicate, 'participant', '2');

    expect(lookup).toEqual({ status: 'found', row: groupRow('2', { label: 'Bob', count: 5 }) });
    expect(groups).toHaveBeenCalledTimes(1);
    expect(groups.mock.calls[0]![0]).toMatchObject({
      filters: [{ dimension: 'participant', values: ['2'] }],
      limit: GROUP_DETAIL_PAGE_SIZE
    });
    expect(groups.mock.calls[0]![1]).toBe('participant');
  });

  it('follows the cursor to a later page containing the requested key', async () => {
    const filler = Array.from({ length: GROUP_DETAIL_PAGE_SIZE }, (_, index) => groupRow(`other-${index}`));
    const groups = vi.fn<ExploreAPI['groups']>(async (page) => page.cursor === 'page-2'
      ? readyPage([groupRow('2', { label: 'Bob', count: 5 })])
      : readyPage(filler, 'page-2'));

    const lookup = await findGroupDetail({ groups }, predicate, 'participant', '2');

    expect(lookup).toEqual({ status: 'found', row: groupRow('2', { label: 'Bob', count: 5 }) });
    expect(groups).toHaveBeenCalledTimes(2);
    expect(groups.mock.calls[0]![0].cursor).toBeUndefined();
    expect(groups.mock.calls[1]![0]).toMatchObject({ cursor: 'page-2', limit: GROUP_DETAIL_PAGE_SIZE });
  });

  it('reports missing when the listing ends without the requested key', async () => {
    const groups = vi.fn<ExploreAPI['groups']>(async () => readyPage([groupRow('1', { label: 'Alice' })]));

    const lookup = await findGroupDetail({ groups }, predicate, 'participant', '2');

    expect(lookup).toEqual({ status: 'missing' });
    expect(groups).toHaveBeenCalledTimes(1);
  });

  it('degrades to missing at the bounded page cap instead of walking forever', async () => {
    let page = 0;
    const groups = vi.fn<ExploreAPI['groups']>(async () => {
      page += 1;
      return readyPage([groupRow(`other-${page}`)], `page-${page + 1}`);
    });

    const lookup = await findGroupDetail({ groups }, predicate, 'participant', '2');

    expect(lookup).toEqual({ status: 'missing' });
    expect(groups).toHaveBeenCalledTimes(GROUP_DETAIL_MAX_PAGES);
  });

  it('surfaces cache-unavailable from any page verbatim', async () => {
    const unavailable: ExploreCacheUnavailable = {
      error: 'cache_unavailable',
      message: 'The analytical cache is absent.',
      readiness: 'absent',
      recovery_action: 'msgvault build-cache'
    };
    const groups = vi.fn<ExploreAPI['groups']>(async (page) => page.cursor
      ? { status: 'unavailable', unavailable }
      : readyPage(
          Array.from({ length: GROUP_DETAIL_PAGE_SIZE }, (_, index) => groupRow(`other-${index}`)),
          'page-2'
        ));

    const lookup = await findGroupDetail({ groups }, predicate, 'participant', '2');

    expect(lookup).toEqual({ status: 'unavailable', unavailable });
  });

  it('forwards the abort signal to every page request', async () => {
    const controller = new AbortController();
    const groups = vi.fn<ExploreAPI['groups']>(async (page) => page.cursor === 'page-2'
      ? readyPage([groupRow('2')])
      : readyPage(
          Array.from({ length: GROUP_DETAIL_PAGE_SIZE }, (_, index) => groupRow(`other-${index}`)),
          'page-2'
        ));

    await findGroupDetail({ groups }, predicate, 'participant', '2', controller.signal);

    expect(groups).toHaveBeenCalledTimes(2);
    expect(groups.mock.calls[0]![2]).toBe(controller.signal);
    expect(groups.mock.calls[1]![2]).toBe(controller.signal);
  });
});
