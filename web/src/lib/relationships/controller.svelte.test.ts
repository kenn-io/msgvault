import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import type { DomainSummary, ExplorePredicate, PersonSummary } from '../explore/models';
import { RelationshipsController } from './controller.svelte';
import type { RelationshipRow } from './models';

const when = '2026-07-19T10:00:00Z';

function relationshipRow(id: number, label: string): RelationshipRow {
  return {
    canonical_id: id,
    display_label: label,
    last_at: when,
    member_ids: [id],
    score: 1,
    signals: {
      last_interaction_at: when,
      meeting_count: 0,
      meetings_together: 0,
      modalities: 1,
      received_from_them: 1,
      sent_count: 1,
      sent_to_them: 1
    }
  };
}

function person(id: number): PersonSummary {
  return {
    id,
    display_label: `Person ${id}`,
    partial_label: false,
    identifiers: [],
    activity_count: 1,
    meeting_count: 0,
    file_count: 0,
    current_relationship_temperature: 0,
    peak_relationship_temperature: 0,
    peak_relationship_year: 0,
    source_counts: [],
    first_at: when,
    last_at: when,
    cache_revision: 'cache-rel'
  };
}

function domainSummary(domain: string): DomainSummary {
  return {
    domain,
    activity_count: 3,
    file_count: 1,
    person_count: 2,
    first_at: when,
    last_at: when,
    source_counts: [],
    cache_revision: 'cache-rel'
  };
}

function pathOf(request: Request): string {
  return new URL(request.url, document.baseURI).pathname;
}

describe('RelationshipsController.loadList', () => {
  it('ranks via /api/v1/relationships when the people facet has an empty query, preserving server order', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (pathOf(request) === '/api/v1/relationships') {
        return Response.json({
          rows: [relationshipRow(2, 'Bob'), relationshipRow(1, 'Alice')],
          total_count: 2,
          cache_revision: 'cache-rel',
          identity_revision: 1
        });
      }
      throw new Error(`unexpected path ${pathOf(request)}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    controller.showAll = true;

    await controller.loadList({ filters: [{ dimension: 'source', values: ['1'] }], presentation: 'table' });

    expect(controller.listRows).toEqual([relationshipRow(2, 'Bob'), relationshipRow(1, 'Alice')]);
    expect(controller.listLoading).toBe(false);
    expect(controller.listError).toBeNull();
    await expect(requests[0]!.clone().json()).resolves.toEqual({
      show_all: true,
      limit: 200,
      filters: [{ dimension: 'source', values: ['1'] }]
    });
  });

  it('searches via /api/v1/participants/search once the query is non-empty, finding any person ranked or not', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (pathOf(request) === '/api/v1/participants/search') {
        return Response.json({ rows: [person(11)], total_count: 1, cache_revision: 'cache-rel', search_provenance: {} });
      }
      throw new Error(`unexpected path ${pathOf(request)}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    controller.query = 'ali';

    await controller.loadList({ filters: [], presentation: 'table' });

    expect(controller.listRows).toEqual([person(11)]);
    await expect(requests[0]!.clone().json()).resolves.toMatchObject({ identity_query: 'ali' });
  });

  it('always searches domains via /api/v1/domains/search, even with an empty query', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (pathOf(request) === '/api/v1/domains/search') {
        return Response.json({
          rows: [domainSummary('example.com')], total_count: 1, cache_revision: 'cache-rel', search_provenance: {}
        });
      }
      throw new Error(`unexpected path ${pathOf(request)}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    controller.facet = 'domains';

    await controller.loadList({ filters: [], presentation: 'table' });

    expect(controller.listRows).toEqual([domainSummary('example.com')]);
    expect(requests).toHaveLength(1);
    await expect(requests[0]!.clone().json()).resolves.toMatchObject({ identity_query: '' });
  });

  it('clears previous-context rows when a fresh-context load fails, so none stay selectable', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/relationships') {
        return Response.json({
          rows: [relationshipRow(1, 'Alice')], total_count: 1, cache_revision: 'cache-rel', identity_revision: 1
        });
      }
      if (path === '/api/v1/participants/search') {
        return Response.json({ error: 'internal_error', message: 'search boom' }, { status: 500 });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.loadList({ filters: [], presentation: 'table' });
    expect(controller.listRows).toEqual([relationshipRow(1, 'Alice')]);

    controller.query = 'ali';
    await controller.loadList({ filters: [], presentation: 'table' });

    expect(controller.listRows).toEqual([]);
    expect(controller.listError).toBe('search boom');
    expect(controller.listLoading).toBe(false);
  });

  it('clears previous-context rows when a fresh-context fetch throws (network failure)', async () => {
    let fail = false;
    const fetchFn = vi.fn<typeof fetch>(async () => {
      if (fail) throw new TypeError('network down');
      return Response.json({
        rows: [relationshipRow(1, 'Alice')], total_count: 1, cache_revision: 'cache-rel', identity_revision: 1
      });
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.loadList({ filters: [], presentation: 'table' });
    expect(controller.listRows).toEqual([relationshipRow(1, 'Alice')]);

    fail = true;
    await controller.loadList({ filters: [{ dimension: 'source', values: ['2'] }], presentation: 'table' });

    expect(controller.listRows).toEqual([]);
    expect(controller.listError).toBe('network down');
    expect(controller.listLoading).toBe(false);
  });

  it('clears previous-context rows while a fresh-context load is in flight, so none stay clickable', async () => {
    let resolveSearch: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/relationships') {
        return Response.json({
          rows: [relationshipRow(1, 'Alice')], total_count: 1, cache_revision: 'cache-rel', identity_revision: 1
        });
      }
      if (path === '/api/v1/participants/search') {
        return new Promise<Response>((resolve) => { resolveSearch = resolve; });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.loadList({ filters: [], presentation: 'table' });
    expect(controller.listRows).toEqual([relationshipRow(1, 'Alice')]);

    controller.query = 'ali';
    const pending = controller.loadList({ filters: [], presentation: 'table' });
    await vi.waitFor(() => expect(resolveSearch).toBeDefined());

    // The hanging search must not leave the old ranked rows selectable under
    // the new query: the list shows its loading state instead.
    expect(controller.listRows).toEqual([]);
    expect(controller.listLoading).toBe(true);

    resolveSearch?.(Response.json({
      rows: [person(11)], total_count: 1, cache_revision: 'cache-rel', search_provenance: {}
    }));
    await pending;

    expect(controller.listRows).toEqual([person(11)]);
    expect(controller.listLoading).toBe(false);
  });

  it('keeps the visible rows through a same-context reload (no flash-clear)', async () => {
    let calls = 0;
    let resolveReload: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (pathOf(request) !== '/api/v1/relationships') throw new Error(`unexpected path ${pathOf(request)}`);
      calls += 1;
      if (calls === 1) {
        return Response.json({
          rows: [relationshipRow(1, 'Alice')], total_count: 1, cache_revision: 'cache-rel', identity_revision: 1
        });
      }
      return new Promise<Response>((resolve) => { resolveReload = resolve; });
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    const predicate: ExplorePredicate = { filters: [], presentation: 'table' };
    await controller.loadList(predicate);
    expect(controller.listRows).toEqual([relationshipRow(1, 'Alice')]);

    // The same context reloading (as after a link/unlink) keeps its rows
    // visible until the refreshed page lands.
    const pending = controller.loadList(predicate);
    await vi.waitFor(() => expect(resolveReload).toBeDefined());
    expect(controller.listRows).toEqual([relationshipRow(1, 'Alice')]);
    expect(controller.listLoading).toBe(true);

    resolveReload?.(Response.json({
      rows: [relationshipRow(2, 'Bob')], total_count: 1, cache_revision: 'cache-rel', identity_revision: 2
    }));
    await pending;

    expect(controller.listRows).toEqual([relationshipRow(2, 'Bob')]);
  });

  it('ignores a stale failure from a superseded load, keeping the newer rows', async () => {
    let rejectStale: ((cause: Error) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/relationships') {
        return new Promise<Response>((_resolve, reject) => { rejectStale = reject; });
      }
      if (path === '/api/v1/participants/search') {
        return Response.json({ rows: [person(11)], total_count: 1, cache_revision: 'cache-rel', search_provenance: {} });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    const stale = controller.loadList({ filters: [], presentation: 'table' });
    await Promise.resolve();

    controller.query = 'ali';
    await controller.loadList({ filters: [], presentation: 'table' });
    expect(controller.listRows).toEqual([person(11)]);

    rejectStale?.(new Error('stale boom'));
    await stale;

    expect(controller.listRows).toEqual([person(11)]);
    expect(controller.listError).toBeNull();
  });

  it('names cache unavailability as a degraded state instead of throwing', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      error: 'analytical_cache_unavailable', message: 'The committed analytical cache is unavailable',
      readiness: 'stale_schema', recovery_action: 'Rebuild the analytical cache'
    }, { status: 503 }));
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await expect(controller.loadList({ filters: [], presentation: 'table' })).resolves.toBeUndefined();

    expect(controller.degraded?.readiness).toBe('stale_schema');
    expect(controller.listError).toBeNull();
    expect(controller.listLoading).toBe(false);
  });

  it('retries relationship ranking while the analytical cache is building', async () => {
    vi.useFakeTimers();
    let calls = 0;
    const fetchFn = vi.fn<typeof fetch>(async () => {
      calls += 1;
      if (calls === 1) return Response.json({
        error: 'analytical_cache_unavailable', message: 'The analytical cache is being prepared',
        readiness: 'building', recovery_action: ''
      }, { status: 503 });
      return Response.json({
        rows: [relationshipRow(11, 'Recovered Person')], total_count: 1,
        cache_revision: 'cache-rel', identity_revision: 1
      });
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.loadList({ filters: [], presentation: 'table' });
    expect(controller.degraded?.readiness).toBe('building');

    await vi.advanceTimersByTimeAsync(1_000);
    expect(controller.listRows).toEqual([relationshipRow(11, 'Recovered Person')]);
    expect(controller.degraded).toBeNull();
    expect(calls).toBe(2);

    controller.destroy();
    vi.useRealTimers();
  });

  it('cancels relationship cache-readiness polling when destroyed', async () => {
    vi.useFakeTimers();
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      error: 'analytical_cache_unavailable', message: 'The analytical cache is being prepared',
      readiness: 'building', recovery_action: ''
    }, { status: 503 }));
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.loadList({ filters: [], presentation: 'table' });
    controller.destroy();
    await vi.advanceTimersByTimeAsync(1_000);

    expect(fetchFn).toHaveBeenCalledOnce();
    vi.useRealTimers();
  });
});

describe('RelationshipsController.loadMoreList', () => {
  it('keeps next_cursor/total_count from the first ranked page, appends deduped rows, and stops at the end', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (pathOf(request) !== '/api/v1/relationships') throw new Error(`unexpected path ${pathOf(request)}`);
      const body = (await request.clone().json()) as { cursor?: string };
      if (body.cursor === undefined) {
        return Response.json({
          rows: [relationshipRow(1, 'Alice'), relationshipRow(2, 'Bob')], total_count: 3,
          cache_revision: 'cache-rel', identity_revision: 1, next_cursor: 'page-2'
        });
      }
      if (body.cursor === 'page-2') {
        return Response.json({
          rows: [relationshipRow(2, 'Bob'), relationshipRow(3, 'Cara')], total_count: 3,
          cache_revision: 'cache-rel', identity_revision: 1
        });
      }
      throw new Error(`unexpected cursor ${body.cursor}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.loadList({ filters: [], presentation: 'table' });

    expect(controller.listCursor).toBe('page-2');
    expect(controller.listTotalCount).toBe(3);

    await controller.loadMoreList();

    expect(controller.listRows).toEqual([
      relationshipRow(1, 'Alice'), relationshipRow(2, 'Bob'), relationshipRow(3, 'Cara')
    ]);
    expect(controller.listCursor).toBeNull();
    expect(controller.listLoadingMore).toBe(false);
    await expect(requests[1]!.clone().json()).resolves.toMatchObject({ cursor: 'page-2' });

    // No next_cursor on the last page: further calls are guarded no-ops.
    await controller.loadMoreList();
    expect(requests).toHaveLength(2);
  });

  it('keeps the loaded rows visible while a next page is in flight (pagination never clears)', async () => {
    let resolvePage: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (pathOf(request) !== '/api/v1/relationships') throw new Error(`unexpected path ${pathOf(request)}`);
      const body = (await request.clone().json()) as { cursor?: string };
      if (body.cursor === undefined) {
        return Response.json({
          rows: [relationshipRow(1, 'Alice')], total_count: 2,
          cache_revision: 'cache-rel', identity_revision: 1, next_cursor: 'page-2'
        });
      }
      return new Promise<Response>((resolve) => { resolvePage = resolve; });
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.loadList({ filters: [], presentation: 'table' });

    const pending = controller.loadMoreList();
    await vi.waitFor(() => expect(resolvePage).toBeDefined());

    expect(controller.listRows).toEqual([relationshipRow(1, 'Alice')]);
    expect(controller.listLoadingMore).toBe(true);

    resolvePage?.(Response.json({
      rows: [relationshipRow(2, 'Bob')], total_count: 2, cache_revision: 'cache-rel', identity_revision: 1
    }));
    await pending;

    expect(controller.listRows).toEqual([relationshipRow(1, 'Alice'), relationshipRow(2, 'Bob')]);
  });

  it('pages a people search with the stored cursor in the request body, deduping by id', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (pathOf(request) !== '/api/v1/participants/search') throw new Error(`unexpected path ${pathOf(request)}`);
      const body = (await request.clone().json()) as { cursor?: string };
      if (body.cursor === undefined) {
        return Response.json({
          rows: [person(11)], total_count: 2, cache_revision: 'cache-rel',
          search_provenance: {}, next_cursor: 'page-2'
        });
      }
      return Response.json({
        rows: [person(11), person(12)], total_count: 2, cache_revision: 'cache-rel', search_provenance: {}
      });
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    controller.query = 'ali';
    await controller.loadList({ filters: [], presentation: 'table' });
    expect(controller.listCursor).toBe('page-2');
    expect(controller.listTotalCount).toBe(2);

    await controller.loadMoreList();

    expect(controller.listRows).toEqual([person(11), person(12)]);
    expect(controller.listCursor).toBeNull();
    await expect(requests[1]!.clone().json()).resolves.toMatchObject({ identity_query: 'ali', cursor: 'page-2' });
  });

  it('drops the cursor and total when the context changes, replacing rows rather than appending', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/relationships') {
        return Response.json({
          rows: [relationshipRow(1, 'Alice')], total_count: 500,
          cache_revision: 'cache-rel', identity_revision: 1, next_cursor: 'ranked-page-2'
        });
      }
      if (path === '/api/v1/participants/search') {
        return Response.json({ rows: [person(11)], total_count: 1, cache_revision: 'cache-rel', search_provenance: {} });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.loadList({ filters: [], presentation: 'table' });
    expect(controller.listCursor).toBe('ranked-page-2');

    controller.query = 'ali';
    await controller.loadList({ filters: [], presentation: 'table' });

    expect(controller.listRows).toEqual([person(11)]);
    expect(controller.listCursor).toBeNull();
    expect(controller.listTotalCount).toBe(1);

    // The old ranked cursor is gone with its context: no stray page fetch.
    await controller.loadMoreList();
    expect(fetchFn).toHaveBeenCalledTimes(2);
  });

  it('ignores a late page response that resolves after a newer context load', async () => {
    let resolveStalePage: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/relationships') {
        const body = (await request.clone().json()) as { cursor?: string };
        if (body.cursor === undefined) {
          return Response.json({
            rows: [relationshipRow(1, 'Alice')], total_count: 2,
            cache_revision: 'cache-rel', identity_revision: 1, next_cursor: 'page-2'
          });
        }
        return new Promise<Response>((resolve) => { resolveStalePage = resolve; });
      }
      if (path === '/api/v1/participants/search') {
        return Response.json({ rows: [person(11)], total_count: 1, cache_revision: 'cache-rel', search_provenance: {} });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.loadList({ filters: [], presentation: 'table' });
    const stalePage = controller.loadMoreList();
    await Promise.resolve();

    controller.query = 'ali';
    await controller.loadList({ filters: [], presentation: 'table' });
    expect(controller.listLoadingMore).toBe(false);

    resolveStalePage?.(Response.json({
      rows: [relationshipRow(2, 'Bob')], total_count: 2,
      cache_revision: 'cache-rel', identity_revision: 1, next_cursor: 'page-3'
    }));
    await stalePage;

    expect(controller.listRows).toEqual([person(11)]);
    expect(controller.listCursor).toBeNull();
    expect(controller.listLoadingMore).toBe(false);
  });

  it('stops paging with a named error when the server repeats a cursor without progress', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (pathOf(request) !== '/api/v1/relationships') throw new Error(`unexpected path ${pathOf(request)}`);
      return Response.json({
        rows: [relationshipRow(1, 'Alice')], total_count: 2,
        cache_revision: 'cache-rel', identity_revision: 1, next_cursor: 'page-2'
      });
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.loadList({ filters: [], presentation: 'table' });
    await controller.loadMoreList();
    expect(controller.listCursor).toBe('page-2');

    await controller.loadMoreList();

    expect(controller.listError).toBe('Pagination stopped because the server repeated a cursor without progress.');
    expect(controller.listCursor).toBeNull();
    expect(fetchFn).toHaveBeenCalledTimes(2);
  });

  it('keeps loaded rows and the cursor when a later page fails transiently, so a retry appends the same page', async () => {
    let failPage = true;
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (pathOf(request) !== '/api/v1/relationships') throw new Error(`unexpected path ${pathOf(request)}`);
      const body = (await request.clone().json()) as { cursor?: string };
      if (body.cursor === undefined) {
        return Response.json({
          rows: [relationshipRow(1, 'Alice')], total_count: 2,
          cache_revision: 'cache-rel', identity_revision: 1, next_cursor: 'page-2'
        });
      }
      if (failPage) return Response.json({ error: 'internal_error', message: 'boom' }, { status: 500 });
      return Response.json({
        rows: [relationshipRow(2, 'Bob')], total_count: 2, cache_revision: 'cache-rel', identity_revision: 1
      });
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.loadList({ filters: [], presentation: 'table' });

    await controller.loadMoreList();

    expect(controller.listRows).toEqual([relationshipRow(1, 'Alice')]);
    expect(controller.listError).toBe('boom');
    expect(controller.listCursor).toBe('page-2');
    expect(controller.listLoadingMore).toBe(false);

    failPage = false;
    await controller.loadMoreList();

    expect(controller.listRows).toEqual([relationshipRow(1, 'Alice'), relationshipRow(2, 'Bob')]);
    expect(controller.listError).toBeNull();
    expect(controller.listCursor).toBeNull();
    await expect(requests[2]!.clone().json()).resolves.toMatchObject({ cursor: 'page-2' });
  });

  it('keeps the cursor when a page fetch throws (network failure), so a retry re-attempts the same page', async () => {
    let failPage = true;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (pathOf(request) !== '/api/v1/relationships') throw new Error(`unexpected path ${pathOf(request)}`);
      const body = (await request.clone().json()) as { cursor?: string };
      if (body.cursor === undefined) {
        return Response.json({
          rows: [relationshipRow(1, 'Alice')], total_count: 2,
          cache_revision: 'cache-rel', identity_revision: 1, next_cursor: 'page-2'
        });
      }
      if (failPage) throw new TypeError('network down');
      return Response.json({
        rows: [relationshipRow(2, 'Bob')], total_count: 2, cache_revision: 'cache-rel', identity_revision: 1
      });
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.loadList({ filters: [], presentation: 'table' });

    await controller.loadMoreList();

    expect(controller.listRows).toEqual([relationshipRow(1, 'Alice')]);
    expect(controller.listError).toBe('network down');
    expect(controller.listCursor).toBe('page-2');
    expect(controller.listLoadingMore).toBe(false);

    failPage = false;
    await controller.loadMoreList();

    expect(controller.listRows).toEqual([relationshipRow(1, 'Alice'), relationshipRow(2, 'Bob')]);
    expect(controller.listError).toBeNull();
    expect(controller.listCursor).toBeNull();
  });

  it('discards the cursor when a page fails with a terminal status (409 revision change)', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (pathOf(request) !== '/api/v1/relationships') throw new Error(`unexpected path ${pathOf(request)}`);
      const body = (await request.clone().json()) as { cursor?: string };
      if (body.cursor === undefined) {
        return Response.json({
          rows: [relationshipRow(1, 'Alice')], total_count: 2,
          cache_revision: 'cache-rel', identity_revision: 1, next_cursor: 'page-2'
        });
      }
      return Response.json(
        { error: 'archive_revision_changed', message: 'The committed analytical cache changed; restart pagination' },
        { status: 409 }
      );
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.loadList({ filters: [], presentation: 'table' });

    await controller.loadMoreList();

    expect(controller.listRows).toEqual([relationshipRow(1, 'Alice')]);
    expect(controller.listError).toBe('The committed analytical cache changed; restart pagination');
    expect(controller.listCursor).toBeNull();

    // With the cursor discarded, further calls are guarded no-ops.
    await controller.loadMoreList();
    expect(fetchFn).toHaveBeenCalledTimes(2);
  });
});

describe('RelationshipsController text-query consistency', () => {
  // The relationships ranking and cluster-timeline endpoints accept no text
  // query, so the hub applies a carried workspace query to NO surface: a
  // predicate that still carries one (e.g. a stale deep link) must be
  // stripped uniformly rather than half-applied to some endpoints.
  it('drops a carried text query from the ranked list, searches, and summaries', async () => {
    const carried: ExplorePredicate = {
      query: 'quarterly plan',
      search_mode: 'full_text',
      filters: [{ dimension: 'source', values: ['1'] }],
      presentation: 'table'
    };
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/relationships') {
        return Response.json({ rows: [], total_count: 0, cache_revision: 'cache-rel', identity_revision: 1 });
      }
      if (path === '/api/v1/participants/search' || path === '/api/v1/domains/search') {
        return Response.json({ rows: [], total_count: 0, cache_revision: 'cache-rel', search_provenance: {} });
      }
      if (path === '/api/v1/participants/12' && request.method === 'GET') return Response.json(person(12));
      if (path === '/api/v1/participants/12/summary') {
        return Response.json({ summary: person(12), cache_revision: 'cache-rel', search_provenance: {} });
      }
      if (path === '/api/v1/domains/example.com' && request.method === 'GET') {
        return Response.json(domainSummary('example.com'));
      }
      if (path === '/api/v1/domains/example.com/summary') {
        return Response.json({ summary: domainSummary('example.com'), cache_revision: 'cache-rel', search_provenance: {} });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.loadList(carried);
    controller.query = 'ali';
    await controller.loadList(carried);
    controller.query = '';
    controller.facet = 'domains';
    await controller.loadList(carried);
    await controller.openTarget('cluster:12', carried);
    await controller.openTarget('domain:example.com', carried);

    const posts = requests.filter((request) => request.method === 'POST');
    expect(posts.map(pathOf).sort()).toEqual([
      '/api/v1/domains/example.com/summary',
      '/api/v1/domains/search',
      '/api/v1/participants/12/summary',
      '/api/v1/participants/search',
      '/api/v1/relationships',
    '/api/v1/relationships/12/calendar'
    ]);
    for (const post of posts) {
      const body = (await post.clone().json()) as Record<string, unknown>;
      expect(body, pathOf(post)).not.toHaveProperty('query');
      expect(body, pathOf(post)).not.toHaveProperty('search_mode');
      expect(body.predicate ?? {}, pathOf(post)).not.toHaveProperty('query');
      expect(body.predicate ?? {}, pathOf(post)).not.toHaveProperty('search_mode');
    }
    // Every surface still receives the carried filters, so the shared
    // workspace context (minus the unsupported text query) stays applied.
    const relationshipsBody = (await posts
      .find((post) => pathOf(post) === '/api/v1/relationships')!
      .clone().json()) as Record<string, unknown>;
    expect(relationshipsBody.filters).toEqual([{ dimension: 'source', values: ['1'] }]);
  });
});

describe('RelationshipsController.openTarget', () => {
  function calendar(id: number, year: number, cacheRevision = 'cache-rel', identityRevision = 3) {
    return {
      participant_id: id, canonical_id: id, year, timezone: 'UTC',
      days: [{ date: `${year}-01-01`, sent: 1, received: 0, email: 1, chat: 0, meetings: 0, total: 1, modality_mask: 1, level: 'FIRST_QUARTILE' }],
      current: { temperature: 62, rank: 4, population: 20, raw_score: 3, signals: { sent_signal: 1, received_volume: 0, meeting_signal: 0, modalities: 1 } },
      annual: [], peak_temperature: 87, peak_year: 2018, scoring_timezone: 'UTC',
      score_version: 1, effective_date: `${year}-08-22`, cache_revision: cacheRevision,
      identity_revision: identityRevision
    };
  }

  it('loads, caches, and revisits exact person/year/timezone calendar revisions', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/participants/12') return Response.json({ ...person(12), first_at: '2018-01-02T00:00:00Z' });
      if (path === '/api/v1/relationships/12/calendar') {
        const body = await request.clone().json() as { year: number };
        return Response.json(calendar(12, body.year));
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.openTarget('cluster:12', { filters: [], presentation: 'table' });
    await vi.waitFor(() => expect(controller.relationshipCalendar?.year).toBe(2026));
    expect(controller.relationshipCalendar?.current.temperature).toBe(62);
    expect(controller.relationshipCalendarFirstYear).toBe(2018);

    await controller.loadRelationshipYear(2025);
    expect(controller.relationshipCalendar?.year).toBe(2025);
    await controller.loadRelationshipYear(2026);
    expect(controller.relationshipCalendar?.year).toBe(2026);
    await controller.openTarget('cluster:12', { filters: [], presentation: 'table' });
    await vi.waitFor(() => expect(controller.relationshipCalendar?.year).toBe(2026));
    const calendarRequests = requests.filter((request) => pathOf(request).endsWith('/calendar'));
    expect(calendarRequests).toHaveLength(3);
    await expect(calendarRequests[0]!.clone().json()).resolves.toEqual({ year: 2026, timezone: 'UTC' });
  });

  it.each(['once', 'every call'] as const)('reopens the full target at most once when the calendar drifts %s while preserving the selected year', async (drift) => {
    let detailCalls = 0;
    let calendarCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/participants/12') {
        detailCalls += 1;
        return Response.json({
          ...person(12),
          activity_count: detailCalls,
          first_at: '2018-01-02T00:00:00Z',
          cache_revision: `cache-rel-${detailCalls}`
        });
      }
      if (path === '/api/v1/relationships/12/calendar') {
        calendarCalls += 1;
        const body = await request.clone().json() as { year: number };
        const revision = drift === 'every call' ? calendarCalls : calendarCalls === 1 ? 1 : 2;
        return Response.json(calendar(12, body.year, `cache-rel-${revision}`, revision));
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.openTarget('cluster:12', { filters: [], presentation: 'table' });
    await vi.waitFor(() => expect(controller.relationshipCalendar?.year).toBe(2026));

    await controller.loadRelationshipYear(2025);
    if (drift === 'every call') {
      await vi.waitFor(() => expect(controller.relationshipCalendarError).toBe('Relationship activity changed again. Retry when the archive settles.'));
    } else {
      await vi.waitFor(() => expect(controller.relationshipCalendar?.year).toBe(2025));
      expect(controller.relationshipCalendar?.cache_revision).toBe('cache-rel-2');
      expect(controller.relationshipCalendarError).toBeNull();
    }

    expect(detailCalls).toBe(2);
    expect(calendarCalls).toBe(3);
    expect(controller.detail).toMatchObject({ activity_count: 2, cache_revision: 'cache-rel-2' });
    controller.destroy();
  });

  it('clamps a preserved calendar year when revision drift moves first activity later', async () => {
    const currentYear = new Date().getUTCFullYear();
    let detailCalls = 0;
    let calendarCalls = 0;
    const requestedYears: number[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/participants/12') {
        detailCalls += 1;
        return Response.json({
          ...person(12),
          first_at: detailCalls === 1 ? '2018-01-02T00:00:00Z' : `${currentYear}-01-02T00:00:00Z`,
          cache_revision: `cache-rel-${detailCalls}`
        });
      }
      if (path === '/api/v1/relationships/12/calendar') {
        calendarCalls += 1;
        const body = await request.clone().json() as { year: number };
        requestedYears.push(body.year);
        const revision = calendarCalls === 1 ? 1 : 2;
        return Response.json(calendar(12, body.year, `cache-rel-${revision}`, revision));
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.openTarget('cluster:12', { filters: [], presentation: 'table' });
    await vi.waitFor(() => expect(controller.relationshipCalendar?.year).toBe(currentYear));
    await controller.loadRelationshipYear(currentYear - 1);
    await vi.waitFor(() => expect(controller.relationshipCalendar?.cache_revision).toBe('cache-rel-2'));

    expect(requestedYears).toEqual([currentYear, currentYear - 1, currentYear]);
    expect(controller.relationshipCalendarFirstYear).toBe(currentYear);
    expect(controller.relationshipCalendarYear).toBe(currentYear);
    expect(controller.relationshipCalendar?.year).toBe(currentYear);
  });

  it('clamps future-only contacts to the current supported calendar year', async () => {
    const currentYear = new Date().getUTCFullYear();
    const requestedYears: number[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/participants/12') {
        return Response.json({ ...person(12), first_at: '2099-01-02T00:00:00Z' });
      }
      if (path === '/api/v1/relationships/12/calendar') {
        const body = await request.clone().json() as { year: number };
        requestedYears.push(body.year);
        return Response.json(calendar(12, body.year));
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.openTarget('cluster:12', { filters: [], presentation: 'table' });
    await vi.waitFor(() => expect(controller.relationshipCalendar?.year).toBe(currentYear));

    expect(requestedYears).toEqual([currentYear]);
    expect(controller.relationshipCalendarFirstYear).toBe(currentYear);
    expect(controller.relationshipCalendarYear).toBe(currentYear);
  });

  it('rejects a stale calendar completion after switching to a domain target', async () => {
    let resolveCalendar: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/participants/12') return Response.json(person(12));
      if (path === '/api/v1/relationships/12/calendar') {
        return new Promise<Response>((resolve) => { resolveCalendar = resolve; });
      }
      if (path === '/api/v1/domains/example.com') return Response.json(domainSummary('example.com'));
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.openTarget('cluster:12', { filters: [], presentation: 'table' });
    await vi.waitFor(() => expect(resolveCalendar).toBeTypeOf('function'));
    await controller.openTarget('domain:example.com', { filters: [], presentation: 'table' });
    resolveCalendar?.(Response.json(calendar(12, 2026)));
    await Promise.resolve();

    expect(controller.relationshipCalendar).toBeNull();
    expect(controller.relationshipCalendarLoading).toBe(false);
  });
  it('recovers a deep-linked cluster detail after cache initialization finishes', async () => {
    vi.useFakeTimers();
    const calls = new Map<string, number>();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      const count = (calls.get(path) ?? 0) + 1;
      calls.set(path, count);
      if (count === 1) return Response.json({
        error: 'analytical_cache_unavailable', message: 'The analytical cache is being prepared',
        readiness: 'building', recovery_action: ''
      }, { status: 503 });
      if (path === '/api/v1/participants/12') return Response.json(person(12));
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.openTarget('cluster:12', { filters: [], presentation: 'table' });
    expect(controller.detail).toBeNull();

    await vi.advanceTimersByTimeAsync(1_000);
    await vi.waitFor(() => expect(controller.detail).toEqual(person(12)));
    expect(calls.get('/api/v1/participants/12')).toBe(2);

    controller.destroy();
    vi.useRealTimers();
  });

  it("opens a cluster target's person header and stores its canonical ID", async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/participants/12' && request.method === 'GET') return Response.json(person(12));
      if (path === '/api/v1/participants/12/summary') {
        return Response.json({ summary: person(12), cache_revision: 'cache-rel', search_provenance: {} });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'America/New_York');

    await controller.openTarget('cluster:12', {
      filters: [{ dimension: 'source', values: ['1'] }], presentation: 'table'
    });

    expect(controller.detail).toEqual(person(12));
    expect(controller.canonicalID).toBe(12);
    expect(controller.timelineLoading).toBe(false);
  });

  it('opens a domain target through the domain detail endpoint', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/domains/example.com' && request.method === 'GET') return Response.json(domainSummary('example.com'));
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.openTarget('domain:example.com', { filters: [], presentation: 'table' });

    expect(controller.detail).toEqual(domainSummary('example.com'));
    expect(controller.canonicalID).toBeNull();
    expect(controller.identityRevision).toBeNull();
    expect(requests.some((request) => pathOf(request).includes('/relationships/'))).toBe(false);
  });

  it('discards a stale openTarget response that resolves after a newer openTarget call', async () => {
    let resolveFirstPerson: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/participants/1' && request.method === 'GET') {
        return new Promise<Response>((resolve) => { resolveFirstPerson = resolve; });
      }
      if (path === '/api/v1/participants/2' && request.method === 'GET') return Response.json(person(2));
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    const firstOpen = controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    await controller.openTarget('cluster:2', { filters: [], presentation: 'table' });

    expect(controller.detail).toEqual(person(2));
    expect(controller.canonicalID).toBe(2);

    resolveFirstPerson?.(Response.json(person(1)));
    await firstOpen;

    expect(controller.detail).toEqual(person(2));
    expect(controller.canonicalID).toBe(2);
  });
});

describe('RelationshipsController filtered header metrics', () => {
  const sourceFilter = { dimension: 'source' as const, values: ['1'] };
  const filtered: ExplorePredicate = { filters: [sourceFilter], presentation: 'table' };

  function clusterPerson(id: number): PersonSummary {
    return {
      ...person(id),
      activity_count: 500,
      file_count: 90,
      first_at: '2010-01-01T00:00:00Z',
      identifiers: [
        { participant_id: id, type: 'beeper', value: 'beeper:8:whatsapp:9:@user:x.y', is_primary: true, provenance: 'participant_identifiers',
          service_slug: 'whatsapp', service_label: 'WhatsApp', scope_kind: 'account', scope_value: 'local-whatsapp_ba_example',
          participant_display_name: 'Alias Example' }
      ],
      cluster: { canonical_id: id, member_ids: [id, id + 100], edges: [{ participant_a: id, participant_b: id + 100 }] }
    };
  }

  function filteredPersonSummary(id: number): PersonSummary {
    return { ...person(id), activity_count: 7, file_count: 2, first_at: '2026-01-05T00:00:00Z', identifiers: [
      { participant_id: id, type: 'beeper', value: 'beeper:8:whatsapp:9:@user:x.y', is_primary: true, provenance: 'participant_identifiers' }
    ] };
  }

  it('shows the contextual person summary metrics, keeping cluster metadata from the unfiltered GET', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/participants/12' && request.method === 'GET') return Response.json(clusterPerson(12));
      if (path === '/api/v1/participants/12/summary') {
        return Response.json({ summary: filteredPersonSummary(12), cache_revision: 'cache-rel', search_provenance: {} });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.openTarget('cluster:12', filtered);

    const detail = controller.detail as PersonSummary;
    expect(detail.activity_count).toBe(7);
    expect(detail.file_count).toBe(2);
    expect(detail.first_at).toBe('2026-01-05T00:00:00Z');
    expect(detail.cluster).toEqual(clusterPerson(12).cluster);
    expect(detail.identifiers).toEqual(clusterPerson(12).identifiers);
    expect(controller.relationshipCalendarFirstYear).toBe(2010);
    expect(controller.timelineError).toBeNull();
    const summaryRequest = requests.find((request) => pathOf(request) === '/api/v1/participants/12/summary');
    await expect(summaryRequest!.clone().json()).resolves.toEqual({ filters: [sourceFilter], presentation: 'table' });
  });

  it('clears contextual person metrics without losing cluster metadata when the summary is missing', async () => {
    const base = { ...clusterPerson(12), source_counts: [{ source_type: 'gmail', count: 500 }], meeting_count: 8, current_relationship_temperature: 50, peak_relationship_temperature: 80, peak_relationship_year: 2025 };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/participants/12') return Response.json(base);
      if (path === '/api/v1/participants/12/summary') return Response.json({ error: 'Person not found' }, { status: 404 });
      if (path === '/api/v1/relationships/12/calendar') return Response.json({
        participant_id: 12, canonical_id: 12, year: 2026, timezone: 'UTC', days: [], annual: [],
        current: { temperature: 50, rank: 1, population: 1, raw_score: 1, signals: { sent_signal: 1, received_volume: 0, meeting_signal: 0, modalities: 1 } },
        peak_temperature: 80, peak_year: 2025, scoring_timezone: 'UTC', score_version: 1, effective_date: '2026-07-19', cache_revision: 'cache-rel', identity_revision: 1
      });
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.openTarget('cluster:12', filtered);

    const detail = controller.detail as PersonSummary;
    expect.soft(detail.source_counts).toEqual([]);
    expect.soft(detail.activity_count).toBe(0);
    expect.soft(detail.file_count).toBe(0);
    expect.soft(detail.meeting_count).toBe(0);
    expect.soft(detail.current_relationship_temperature).toBe(0);
    expect.soft(detail.peak_relationship_temperature).toBe(0);
    expect.soft(detail.peak_relationship_year).toBe(0);
    expect(detail.cluster).toEqual(base.cluster);
    expect(detail.identifiers).toEqual(base.identifiers);
    expect.soft(controller.timelineError).toBeNull();
  });

  it.each([
    {
      name: 'matching summary', status: 200,
      response: { summary: { ...domainSummary('example.com'), activity_count: 5, file_count: 1, person_count: 2 }, cache_revision: 'cache-rel', search_provenance: {} },
      expected: { detail: { domain: 'example.com', activity_count: 5, file_count: 1, person_count: 2, first_at: when, last_at: when } }
    },
    {
      name: 'empty summary', status: 404, response: { error: 'Domain not found' },
      expected: { detail: { domain: 'example.com', activity_count: 0, file_count: 0, person_count: 0, source_counts: [] }, timelineError: null }
    }
  ])('shows contextual domain metrics with active filters: $name', async ({ status, response, expected }) => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/domains/example.com' && request.method === 'GET') {
        return Response.json({ ...domainSummary('example.com'), activity_count: 900, file_count: 40, person_count: 60, source_counts: [{ source_type: 'gmail', count: 900 }] });
      }
      if (path === '/api/v1/domains/example.com/summary') return Response.json(response, { status });
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.openTarget('domain:example.com', filtered);

    expect(controller).toMatchObject(expected);
    const summaryRequest = requests.find((request) => pathOf(request) === '/api/v1/domains/example.com/summary');
    await expect(summaryRequest!.clone().json()).resolves.toEqual({ filters: [sourceFilter], presentation: 'table' });
  });

  it('skips the summary endpoints entirely when no filters are active', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/participants/12' && request.method === 'GET') return Response.json(clusterPerson(12));
      if (path === '/api/v1/domains/example.com' && request.method === 'GET') {
        return Response.json(domainSummary('example.com'));
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.openTarget('cluster:12', { filters: [], presentation: 'table' });
    expect(controller.detail).toEqual(clusterPerson(12));

    await controller.openTarget('domain:example.com', { filters: [], presentation: 'table' });
    expect(controller.detail).toEqual(domainSummary('example.com'));

    expect(requests.some((request) => pathOf(request).endsWith('/summary'))).toBe(false);
  });

  it('keeps the unfiltered GET header and surfaces an error when the summary request fails', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/participants/12' && request.method === 'GET') return Response.json(clusterPerson(12));
      if (path === '/api/v1/participants/12/summary') {
        return Response.json({ error: 'internal_error', message: 'summary boom' }, { status: 500 });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.openTarget('cluster:12', filtered);

    expect(controller.detail).toEqual(clusterPerson(12));
    expect(controller.timelineError).toBe('summary boom');
  });

  it('discards a stale summary that resolves after a newer openTarget call', async () => {
    let resolveStaleSummary: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/participants/1' && request.method === 'GET') return Response.json(clusterPerson(1));
      if (path === '/api/v1/participants/1/summary') {
        return new Promise<Response>((resolve) => { resolveStaleSummary = resolve; });
      }
      if (path === '/api/v1/participants/2' && request.method === 'GET') return Response.json(clusterPerson(2));
      if (path === '/api/v1/participants/2/summary') {
        return Response.json({ summary: filteredPersonSummary(2), cache_revision: 'cache-rel', search_provenance: {} });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    const firstOpen = controller.openTarget('cluster:1', filtered);
    await controller.openTarget('cluster:2', filtered);
    expect((controller.detail as PersonSummary).id).toBe(2);
    expect((controller.detail as PersonSummary).activity_count).toBe(7);

    resolveStaleSummary?.(Response.json({
      summary: { ...filteredPersonSummary(1), activity_count: 999 }, cache_revision: 'cache-rel', search_provenance: {}
    }));
    await firstOpen;

    expect((controller.detail as PersonSummary).id).toBe(2);
    expect((controller.detail as PersonSummary).activity_count).toBe(7);
  });
});

describe('RelationshipsController.linkParticipants / unlinkParticipants', () => {
  it('maps 200 to ok with cacheState and reopens the target', async () => {
    let personCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/participants/1' && request.method === 'GET') {
        personCalls += 1;
        return Response.json(person(1));
      }
      if (path === '/api/v1/identity/links') return Response.json({ identity_revision: 2, cache_state: 'ready' });
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.openTarget('cluster:1', { filters: [], presentation: 'table' });

    const outcome = await controller.linkParticipants(1, 2);

    expect(outcome).toEqual({ ok: true, identityRevision: 2, cacheState: 'ready' });
    expect(personCalls).toBe(2);
  });

  it('also reloads the ranked list after a successful link, using the last-loaded predicate', async () => {
    let personCalls = 0;
    let relationshipsCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/relationships') {
        relationshipsCalls += 1;
        return Response.json({
          rows: [relationshipRow(1, 'Alice')], total_count: 1, cache_revision: 'cache-rel', identity_revision: 1
        });
      }
      if (path === '/api/v1/participants/1' && request.method === 'GET') {
        personCalls += 1;
        return Response.json(person(1));
      }
      if (path === '/api/v1/identity/links') return Response.json({ identity_revision: 2, cache_state: 'ready' });
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.loadList({ filters: [], presentation: 'table' });
    await controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    expect(relationshipsCalls).toBe(1);

    const outcome = await controller.linkParticipants(1, 2);

    expect(outcome).toEqual({ ok: true, identityRevision: 2, cacheState: 'ready' });
    expect(relationshipsCalls).toBe(2);
  });

  it('does not reload the list when none was ever loaded (no stray fetch)', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ identity_revision: 1, cache_state: 'ready' }));
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    const outcome = await controller.linkParticipants(1, 2);

    expect(outcome).toEqual({ ok: true, identityRevision: 1, cacheState: 'ready' });
    expect(fetchFn).toHaveBeenCalledTimes(1);
  });

  it('maps a 409 already_linked body to code already_linked', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(
      { error: 'already_linked', message: 'these participants are already connected through other links' },
      { status: 409 }
    ));
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    const outcome = await controller.linkParticipants(1, 2);

    expect(outcome).toEqual({
      ok: false, code: 'already_linked', message: 'these participants are already connected through other links'
    });
  });

  it('returns a strictly validated merge-required outcome without replaying the link', async () => {
    const conflict = {
      error: 'person_merge_required', message: 'Choose a survivor', profiles: [
        { etag: '"person-7-r4"', person: { id: 7, revision: 4 } },
        { etag: '"person-9-r2"', person: { id: 9, revision: 2 } }
      ]
    };
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(conflict, { status: 409 }));
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await expect(controller.linkParticipants(1, 2)).resolves.toEqual({
      ok: false, code: 'merge_required', message: 'Choose a survivor', conflict
    });
    expect(fetchFn).toHaveBeenCalledOnce();

    const malformed = vi.fn<typeof fetch>(async () => Response.json({
      ...conflict, profiles: [conflict.profiles[0], conflict.profiles[0]]
    }, { status: 409 }));
    const malformedController = new RelationshipsController(createAPIClient(malformed), () => 'UTC');
    await expect(malformedController.linkParticipants(1, 2)).resolves.toEqual({
      ok: false, code: 'error', message: 'Choose a survivor'
    });
  });

  it('reconciles only the captured target and list context without replaying identity linking', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/relationships') return Response.json({ rows: [], total_count: 0 });
      if (path === '/api/v1/participants/1') return Response.json(person(1));
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    const predicate = { filters: [], presentation: 'table' as const };
    await controller.loadList(predicate);
    await controller.openTarget('cluster:1', predicate);
    const context = controller.personMergeContextSnapshot();
    requests.length = 0;

    await controller.reconcilePersonMerge(context);

    expect(requests.some((request) => pathOf(request) === '/api/v1/identity/links')).toBe(false);
    expect(requests.filter((request) => pathOf(request) === '/api/v1/participants/1')).toHaveLength(1);
    expect(requests.filter((request) => pathOf(request) === '/api/v1/relationships')).toHaveLength(1);

    const staleContext = controller.personMergeContextSnapshot();
    controller.target = 'cluster:2';
    controller.query = 'different list';
    requests.length = 0;
    await controller.reconcilePersonMerge(staleContext);
    expect(requests).toHaveLength(0);
  });

  it('maps a 400 body to code invalid', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(
      { error: 'invalid_participant_id', message: 'participant_a and participant_b must be distinct positive participant IDs' },
      { status: 400 }
    ));
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    const outcome = await controller.unlinkParticipants(-1, 2);

    expect(outcome).toEqual({
      ok: false, code: 'invalid', message: 'participant_a and participant_b must be distinct positive participant IDs'
    });
  });

  it('maps any other failure (500, network error) to code error', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(
      { error: 'internal_error', message: 'failed to update participant links' }, { status: 500 }
    ));
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    const outcome = await controller.linkParticipants(1, 2);

    expect(outcome).toEqual({ ok: false, code: 'error', message: 'failed to update participant links' });
  });
});

describe('RelationshipsController.clearTarget', () => {
  it('resets the target and detail and discards a late detail response', async () => {
    let resolveDetail: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/participants/1' && request.method === 'GET') return new Promise<Response>(resolve => { resolveDetail = resolve; });
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    const openPromise = controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    await Promise.resolve();

    controller.clearTarget();

    expect(controller.target).toBeNull();
    expect(controller.detail).toBeNull();
    expect(controller.canonicalID).toBeNull();
    expect(controller.identityRevision).toBeNull();

    resolveDetail?.(Response.json(person(1)));
    await openPromise;

    expect(controller.target).toBeNull();
    expect(controller.detail).toBeNull();
  });
});

describe('RelationshipsController abort/destroy', () => {
  it('cancels a pending deep-link cache retry when destroyed', async () => {
    vi.useFakeTimers();
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      error: 'analytical_cache_unavailable', message: 'The analytical cache is being prepared',
      readiness: 'building', recovery_action: ''
    }, { status: 503 }));
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.openTarget('cluster:12', { filters: [], presentation: 'table' });
    expect(fetchFn).toHaveBeenCalledTimes(1);
    controller.destroy();
    await vi.advanceTimersByTimeAsync(1_000);

    expect(fetchFn).toHaveBeenCalledTimes(1);
    vi.useRealTimers();
  });

  it('destroy() aborts the in-flight list load and detail load AbortControllers', async () => {
    const signals: AbortSignal[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      signals.push(request.signal);
      return new Promise<Response>(() => {
        // Never resolves; destroy() must abort it rather than leave it hanging forever.
      });
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    void controller.loadList({ filters: [], presentation: 'table' });
    void controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    await Promise.resolve();
    await Promise.resolve();

    expect(signals.length).toBeGreaterThanOrEqual(2);
    expect(signals.every((signal) => !signal.aborted)).toBe(true);

    controller.destroy();

    expect(signals.every((signal) => signal.aborted)).toBe(true);
  });

  it('aborts the previous list load when a new one starts', async () => {
    const signals: AbortSignal[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      signals.push(request.signal);
      if (signals.length === 1) return new Promise<Response>(() => {});
      return Response.json({ rows: [], total_count: 0, cache_revision: 'cache-rel', identity_revision: 1 });
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    void controller.loadList({ filters: [], presentation: 'table' });
    await Promise.resolve();
    await controller.loadList({ filters: [], presentation: 'table' });

    expect(signals[0]!.aborted).toBe(true);
    expect(signals[1]!.aborted).toBe(false);
  });
});

describe('RelationshipsController draft-only targets', () => {
  it.each([
    { target: 'cluster:12', detailPath: '/api/v1/participants/12', filtered: false },
    { target: 'domain:example.com', detailPath: '/api/v1/domains/example.com', filtered: false },
    { target: 'cluster:12', detailPath: '/api/v1/participants/12', filtered: true },
    { target: 'domain:example.com', detailPath: '/api/v1/domains/example.com', filtered: true }
  ])('opens $target without a detail error or calendar, filtered=$filtered', async ({ target, detailPath, filtered }) => {
    const draftPerson = { ...person(12), activity_count: 0 };
    delete draftPerson.first_at;
    delete draftPerson.last_at;
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === detailPath) return target.startsWith('cluster:')
        ? Response.json(draftPerson)
        : Response.json({ error: 'No contact activity' }, { status: 404 });
      if (path.endsWith('/calendar')) return Response.json({ error: 'participant_not_found' }, { status: 404 });
      if (path === `${detailPath}/summary`) return Response.json({ error: 'No matching activity' }, { status: 404 });
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.openTarget(target, { filters: filtered ? [{ dimension: 'source', values: ['1'] }] : [], presentation: 'table' });

    expect.soft(controller.timelineError).toBeNull();
    expect.soft(controller.detail?.activity_count ?? null).toBe(target.startsWith('cluster:') ? 0 : null);
    expect.soft(controller.detailMissing).toBe(target.startsWith('domain:'));
    expect(requests.some((request) => pathOf(request).endsWith('/calendar'))).toBe(false);
    if (target.startsWith('cluster:')) {
      expect(controller.relationshipCalendar).toBeNull();
      expect(controller.relationshipCalendarError).toBeNull();
      expect(controller.relationshipCalendarFirstYear).toBeNull();
    }
  });

  it.each([
    { filtered: false, timelineError: null },
    { filtered: true, timelineError: 'Summary unavailable' },
  ])('marks a missing person as not found without loading a calendar, filtered=$filtered', async ({ filtered, timelineError }) => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (pathOf(request) === '/api/v1/participants/12') return Response.json({ error: 'participant_not_found', message: 'Participant cluster not found' }, { status: 404 });
      if (pathOf(request) === '/api/v1/relationships/12/timeline') return Response.json({ canonical_id: 12, identity_revision: 1, rows: [], total_count: 0, cache_revision: 'cache-rel' });
      if (pathOf(request) === '/api/v1/participants/12/summary') return Response.json({ error: 'unavailable', message: 'Summary unavailable' }, { status: 503 });
      return Response.json({ error: 'participant_not_found' }, { status: 404 });
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.openTarget('cluster:12', { filters: filtered ? [{ dimension: 'source', values: ['1'] }] : [], presentation: 'table' });

    expect(controller.detail).toBeNull();
    expect(controller.detailMissing).toBe(true);
    expect(controller.timelineError).toBe(timelineError);
    expect(requests.some((request) => pathOf(request).endsWith('/calendar'))).toBe(false);
  });
});

it('keeps the chosen People order on later pages', async () => {
  const bodies: Array<Record<string, unknown>> = [];
  const controller = new RelationshipsController(createAPIClient(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    expect(new URL(request.url).pathname).toBe('/api/v1/participants/search');
    bodies.push(await request.json());
    return Response.json({ rows: [person(bodies.length)], total_count: 2, cache_revision: 'cache-rel',
      ...(bodies.length === 1 ? { next_cursor: 'next' } : {}) });
  }), () => 'UTC');
  controller.sort = { field: 'display_label', direction: 'asc' };
  await controller.loadList({ filters: [], presentation: 'table' });
  controller.sort = { field: 'latest_at', direction: 'desc' };
  await controller.loadMoreList();
  expect(bodies.map((body) => body.sort)).toEqual([{ field: 'display_label', direction: 'asc' }, { field: 'display_label', direction: 'asc' }]);
  expect(controller.listRows.map((row) => 'id' in row ? row.id : null)).toEqual([1, 2]);
  controller.destroy();
});

it('captures cadence including zero for paging and rejects an old merge context', async () => {
  const bodies: Record<string, unknown>[] = [];
  const controller = new RelationshipsController(createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    bodies.push(await request.json());
    return Response.json({ rows: [relationshipRow(bodies.length, 'Synthetic')], next_cursor: bodies.length === 1 ? 'next' : undefined });
  }), () => 'UTC');
  try {
    controller.cadence = 'due'; controller.dueWithinDays = 0;
    await controller.loadList({});
    const context = controller.personMergeContextSnapshot();
    controller.cadence = 'overdue'; controller.dueWithinDays = 7;
    await controller.loadMoreList();
    expect(bodies[0]).toMatchObject({ cadence: 'due', due_within_days: 0 });
    expect(bodies[1]).toMatchObject({ cadence: 'due', due_within_days: 0, cursor: 'next' });
    await controller.reconcilePersonMerge(context);
    expect(bodies).toHaveLength(2);
    await controller.loadList({});
    expect(bodies[2]).toMatchObject({ cadence: 'overdue' });
    expect(bodies[2]).not.toHaveProperty('due_within_days');
  } finally { controller.destroy(); }
});

it('suspends pending list work and prevents merge and link refresh from reviving it', async () => {
  let resolve!: (response: Response) => void;
  const paths: string[] = [];
  const controller = new RelationshipsController(createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    paths.push(new URL(request.url).pathname);
    return paths.length === 1 ? new Promise<Response>(done => resolve = done) : Response.json({ identity_revision: 2, cache_state: 'ready' });
  }), () => 'UTC');
  try {
    const pending = controller.loadList({});
    const context = controller.personMergeContextSnapshot();
    controller.suspendList();
    resolve(Response.json({ rows: [relationshipRow(1, 'Hidden')], next_cursor: 'old' }));
    await pending;
    await controller.loadMoreList();
    await controller.reconcilePersonMerge(context);
    await controller.linkParticipants(1, 2);
    expect(paths).toHaveLength(2);
    expect(controller.listRows).toEqual([]);
    expect(controller.listCursor).toBeNull();
    expect(controller.listLoading).toBe(false);
  } finally { controller.destroy(); }
});

it('cancels a building-cache retry when the list is suspended', async () => {
  vi.useFakeTimers();
  const fetch = vi.fn(async () => Response.json({ error: 'analytical_cache_unavailable', readiness: 'building', message: 'Building', recovery_action: '' }, { status: 503 }));
  const controller = new RelationshipsController(createAPIClient(fetch), () => 'UTC');
  try {
    await controller.loadList({});
    controller.suspendList();
    await vi.advanceTimersByTimeAsync(2000);
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(controller.degraded).toBeNull();
  } finally { controller.destroy(); vi.useRealTimers(); }
});

it('clears rows on a cadence change and ignores a late old response', async () => {
  let resolve!: (response: Response) => void;
  let count = 0;
  const controller = new RelationshipsController(createAPIClient(async () => ++count === 2
    ? new Promise<Response>(done => resolve = done)
    : Response.json({ rows: [relationshipRow(count, 'Synthetic')], next_cursor: 'next' })), () => 'UTC');
  try {
    await controller.loadList({});
    controller.cadence = 'due'; controller.dueWithinDays = 7;
    const pending = controller.loadList({});
    expect(controller.listRows).toEqual([]);
    expect(controller.listCursor).toBeNull();
    controller.cadence = 'no_list';
    await controller.loadList({});
    resolve(Response.json({ rows: [relationshipRow(2, 'Old due row')] }));
    await pending;
    expect(controller.listRows.map(row => 'canonical_id' in row ? row.canonical_id : null)).toEqual([3]);
  } finally { controller.destroy(); }
});
