import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import type { DomainSummary, ExplorePredicate, PersonSummary } from '../explore/models';
import { RelationshipsController } from './controller.svelte';
import type { RelationshipRow, RelationshipTimelineRow } from './models';

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
    file_count: 0,
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

function timelineRow(key: string): RelationshipTimelineRow {
  return {
    key,
    kind: 'email',
    occurred_at: when,
    preview: 'preview',
    source_id: 1,
    title: key,
    has_attachments: false,
    message_count: 1
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

  it('searches via /api/v1/people/search once the query is non-empty, finding any person ranked or not', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (pathOf(request) === '/api/v1/people/search') {
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
      if (path === '/api/v1/people/search') {
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
      if (path === '/api/v1/people/search') {
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
      if (path === '/api/v1/people/search') {
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

    expect(controller.degraded).toBe('cache_unavailable');
    expect(controller.listError).toBeNull();
    expect(controller.listLoading).toBe(false);
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
      if (pathOf(request) !== '/api/v1/people/search') throw new Error(`unexpected path ${pathOf(request)}`);
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
      if (path === '/api/v1/people/search') {
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
      if (path === '/api/v1/people/search') {
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
  it('drops a carried text query from every surface: ranked list, searches, and both timelines', async () => {
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
      if (path === '/api/v1/people/search' || path === '/api/v1/domains/search') {
        return Response.json({ rows: [], total_count: 0, cache_revision: 'cache-rel', search_provenance: {} });
      }
      if (path === '/api/v1/people/12' && request.method === 'GET') return Response.json(person(12));
      if (path === '/api/v1/people/12/summary') {
        return Response.json({ summary: person(12), cache_revision: 'cache-rel', search_provenance: {} });
      }
      if (path === '/api/v1/relationships/12/timeline') {
        return Response.json({ canonical_id: 12, identity_revision: 1, rows: [], total_count: 0, cache_revision: 'cache-rel' });
      }
      if (path === '/api/v1/domains/example.com' && request.method === 'GET') {
        return Response.json(domainSummary('example.com'));
      }
      if (path === '/api/v1/domains/example.com/summary') {
        return Response.json({ summary: domainSummary('example.com'), cache_revision: 'cache-rel', search_provenance: {} });
      }
      if (path === '/api/v1/domains/example.com/timeline') {
        return Response.json({ rows: [], total_count: 0, cache_revision: 'cache-rel', search_provenance: {} });
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
      '/api/v1/domains/example.com/timeline',
      '/api/v1/domains/search',
      '/api/v1/people/12/summary',
      '/api/v1/people/search',
      '/api/v1/relationships',
      '/api/v1/relationships/12/timeline'
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
  it("opens a cluster target's person header and cluster timeline, storing canonical_id + identity_revision", async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/people/12' && request.method === 'GET') return Response.json(person(12));
      if (path === '/api/v1/people/12/summary') {
        return Response.json({ summary: person(12), cache_revision: 'cache-rel', search_provenance: {} });
      }
      if (path === '/api/v1/relationships/12/timeline') {
        return Response.json({
          canonical_id: 12, identity_revision: 3, rows: [timelineRow('t1')], total_count: 1, cache_revision: 'cache-rel'
        });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'America/New_York');

    await controller.openTarget('cluster:12', {
      filters: [{ dimension: 'source', values: ['1'] }], presentation: 'table'
    });

    expect(controller.detail).toEqual(person(12));
    expect(controller.timelineRows).toEqual([timelineRow('t1')]);
    expect(controller.canonicalID).toBe(12);
    expect(controller.identityRevision).toBe(3);
    expect(controller.timelineLoading).toBe(false);
    const timelineRequest = requests.find((request) => pathOf(request) === '/api/v1/relationships/12/timeline');
    await expect(timelineRequest!.clone().json()).resolves.toMatchObject({
      timezone: 'America/New_York', filters: [{ dimension: 'source', values: ['1'] }], limit: 200
    });
  });

  it('opens a domain target through the existing domain detail + domain timeline endpoints, not a cluster timeline', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/domains/example.com' && request.method === 'GET') return Response.json(domainSummary('example.com'));
      if (path === '/api/v1/domains/example.com/timeline') {
        return Response.json({ rows: [timelineRow('t1')], total_count: 1, cache_revision: 'cache-rel', search_provenance: {} });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.openTarget('domain:example.com', { filters: [], presentation: 'table' });

    expect(controller.detail).toEqual(domainSummary('example.com'));
    expect(controller.timelineRows).toEqual([timelineRow('t1')]);
    expect(controller.canonicalID).toBeNull();
    expect(controller.identityRevision).toBeNull();
    expect(requests.some((request) => pathOf(request).includes('/relationships/'))).toBe(false);
  });

  it('dispatches on the target prefix, not the facet, when the two disagree', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/domains/example.com' && request.method === 'GET') return Response.json(domainSummary('example.com'));
      if (path === '/api/v1/domains/example.com/timeline') {
        return Response.json({ rows: [], total_count: 0, cache_revision: 'cache-rel', search_provenance: {} });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    controller.facet = 'people';

    await controller.openTarget('domain:example.com', { filters: [], presentation: 'table' });

    expect(controller.detail).toEqual(domainSummary('example.com'));
  });

  it('discards a stale openTarget response that resolves after a newer openTarget call', async () => {
    let resolveFirstPerson: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/1' && request.method === 'GET') {
        return new Promise<Response>((resolve) => { resolveFirstPerson = resolve; });
      }
      if (path === '/api/v1/relationships/1/timeline') {
        return Response.json({ canonical_id: 1, identity_revision: 1, rows: [], total_count: 0, cache_revision: 'cache-rel' });
      }
      if (path === '/api/v1/people/2' && request.method === 'GET') return Response.json(person(2));
      if (path === '/api/v1/relationships/2/timeline') {
        return Response.json({
          canonical_id: 2, identity_revision: 5, rows: [timelineRow('t2')], total_count: 1, cache_revision: 'cache-rel'
        });
      }
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
    expect(controller.timelineRows).toEqual([timelineRow('t2')]);
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
        { participant_id: id, type: 'email', value: `p${id}@example.com`, is_primary: true, provenance: 'message_headers' }
      ],
      cluster: { canonical_id: id, member_ids: [id, id + 100], edges: [{ participant_a: id, participant_b: id + 100 }] }
    };
  }

  function filteredPersonSummary(id: number): PersonSummary {
    return { ...person(id), activity_count: 7, file_count: 2, first_at: '2026-01-05T00:00:00Z', identifiers: null };
  }

  it('shows the contextual person summary metrics, keeping cluster metadata from the unfiltered GET', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/people/12' && request.method === 'GET') return Response.json(clusterPerson(12));
      if (path === '/api/v1/people/12/summary') {
        return Response.json({ summary: filteredPersonSummary(12), cache_revision: 'cache-rel', search_provenance: {} });
      }
      if (path === '/api/v1/relationships/12/timeline') {
        return Response.json({ canonical_id: 12, identity_revision: 1, rows: [], total_count: 0, cache_revision: 'cache-rel' });
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
    expect(controller.timelineError).toBeNull();
    const summaryRequest = requests.find((request) => pathOf(request) === '/api/v1/people/12/summary');
    await expect(summaryRequest!.clone().json()).resolves.toEqual({ filters: [sourceFilter], presentation: 'table' });
  });

  it('shows the contextual domain summary metrics with the active filters applied', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/domains/example.com' && request.method === 'GET') {
        return Response.json({ ...domainSummary('example.com'), activity_count: 900, file_count: 40, person_count: 60 });
      }
      if (path === '/api/v1/domains/example.com/summary') {
        return Response.json({
          summary: { ...domainSummary('example.com'), activity_count: 5, file_count: 1, person_count: 2 },
          cache_revision: 'cache-rel',
          search_provenance: {}
        });
      }
      if (path === '/api/v1/domains/example.com/timeline') {
        return Response.json({ rows: [], total_count: 0, cache_revision: 'cache-rel', search_provenance: {} });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');

    await controller.openTarget('domain:example.com', filtered);

    const detail = controller.detail as DomainSummary;
    expect(detail.activity_count).toBe(5);
    expect(detail.file_count).toBe(1);
    expect(detail.person_count).toBe(2);
    const summaryRequest = requests.find((request) => pathOf(request) === '/api/v1/domains/example.com/summary');
    await expect(summaryRequest!.clone().json()).resolves.toEqual({ filters: [sourceFilter], presentation: 'table' });
  });

  it('skips the summary endpoints entirely when no filters are active', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/people/12' && request.method === 'GET') return Response.json(clusterPerson(12));
      if (path === '/api/v1/relationships/12/timeline') {
        return Response.json({ canonical_id: 12, identity_revision: 1, rows: [], total_count: 0, cache_revision: 'cache-rel' });
      }
      if (path === '/api/v1/domains/example.com' && request.method === 'GET') {
        return Response.json(domainSummary('example.com'));
      }
      if (path === '/api/v1/domains/example.com/timeline') {
        return Response.json({ rows: [], total_count: 0, cache_revision: 'cache-rel', search_provenance: {} });
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
      if (path === '/api/v1/people/12' && request.method === 'GET') return Response.json(clusterPerson(12));
      if (path === '/api/v1/people/12/summary') {
        return Response.json({ error: 'internal_error', message: 'summary boom' }, { status: 500 });
      }
      if (path === '/api/v1/relationships/12/timeline') {
        return Response.json({ canonical_id: 12, identity_revision: 1, rows: [], total_count: 0, cache_revision: 'cache-rel' });
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
      if (path === '/api/v1/people/1' && request.method === 'GET') return Response.json(clusterPerson(1));
      if (path === '/api/v1/people/1/summary') {
        return new Promise<Response>((resolve) => { resolveStaleSummary = resolve; });
      }
      if (path === '/api/v1/relationships/1/timeline') {
        return Response.json({ canonical_id: 1, identity_revision: 1, rows: [], total_count: 0, cache_revision: 'cache-rel' });
      }
      if (path === '/api/v1/people/2' && request.method === 'GET') return Response.json(clusterPerson(2));
      if (path === '/api/v1/people/2/summary') {
        return Response.json({ summary: filteredPersonSummary(2), cache_revision: 'cache-rel', search_provenance: {} });
      }
      if (path === '/api/v1/relationships/2/timeline') {
        return Response.json({ canonical_id: 2, identity_revision: 1, rows: [], total_count: 0, cache_revision: 'cache-rel' });
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

describe('RelationshipsController.loadMoreTimeline', () => {
  it('sends the stored cursor and restarts from page 1 exactly once on a 409 cursor_invalidated', async () => {
    let timelineCalls = 0;
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/people/1' && request.method === 'GET') return Response.json(person(1));
      if (path === '/api/v1/relationships/1/timeline') {
        timelineCalls += 1;
        const body = (await request.clone().json()) as { cursor?: string };
        if (body.cursor === undefined && timelineCalls === 1) {
          return Response.json({
            canonical_id: 1, identity_revision: 1, rows: [timelineRow('t1')], total_count: 3,
            cache_revision: 'cache-rel', next_cursor: 'page-2'
          });
        }
        if (body.cursor === 'page-2') {
          return Response.json({ error: 'cursor_invalidated', message: 'The timeline context changed; restart pagination' }, { status: 409 });
        }
        if (body.cursor === undefined && timelineCalls === 3) {
          return Response.json({
            canonical_id: 1, identity_revision: 2, rows: [timelineRow('t1-restart')], total_count: 1, cache_revision: 'cache-rel-2'
          });
        }
        throw new Error(`unexpected timeline call ${timelineCalls} body ${JSON.stringify(body)}`);
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    expect(controller.timelineCursor).toBe('page-2');

    await controller.loadMoreTimeline();

    expect(timelineCalls).toBe(3);
    expect(controller.timelineRows).toEqual([timelineRow('t1-restart')]);
    expect(controller.timelineCursor).toBeNull();
    expect(controller.timelineLoadingMore).toBe(false);
    expect(controller.identityRevision).toBe(2);
    expect(controller.timelineRestartNotice).toBe('Timeline restarted: the archive changed.');
  });

  it('clears a stale restart notice when a fresh openTarget navigates elsewhere', async () => {
    let timelineCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/1' && request.method === 'GET') return Response.json(person(1));
      if (path === '/api/v1/people/2' && request.method === 'GET') return Response.json(person(2));
      if (path === '/api/v1/relationships/1/timeline') {
        timelineCalls += 1;
        const body = (await request.clone().json()) as { cursor?: string };
        if (body.cursor === undefined && timelineCalls === 1) {
          return Response.json({
            canonical_id: 1, identity_revision: 1, rows: [timelineRow('t1')], total_count: 2,
            cache_revision: 'cache-rel', next_cursor: 'page-2'
          });
        }
        if (body.cursor === 'page-2') {
          return Response.json({ error: 'cursor_invalidated', message: 'restart pagination' }, { status: 409 });
        }
        return Response.json({
          canonical_id: 1, identity_revision: 2, rows: [timelineRow('t1-restart')], total_count: 1, cache_revision: 'cache-rel-2'
        });
      }
      if (path === '/api/v1/relationships/2/timeline') {
        return Response.json({ canonical_id: 2, identity_revision: 1, rows: [], total_count: 0, cache_revision: 'cache-rel' });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    await controller.loadMoreTimeline();
    expect(controller.timelineRestartNotice).toBe('Timeline restarted: the archive changed.');

    await controller.openTarget('cluster:2', { filters: [], presentation: 'table' });
    expect(controller.timelineRestartNotice).toBeNull();
  });

  it('resets timelineLoadingMore on every early return, so a failed page never wedges pagination', async () => {
    let failPage = true;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/1' && request.method === 'GET') return Response.json(person(1));
      if (path === '/api/v1/relationships/1/timeline') {
        const body = (await request.clone().json()) as { cursor?: string };
        if (body.cursor === undefined) {
          return Response.json({
            canonical_id: 1, identity_revision: 1, rows: [timelineRow('t1')], total_count: 2,
            cache_revision: 'cache-rel', next_cursor: 'page-2'
          });
        }
        if (failPage) return Response.json({ error: 'internal_error', message: 'boom' }, { status: 500 });
        return Response.json({
          canonical_id: 1, identity_revision: 1, rows: [timelineRow('t2')], total_count: 2, cache_revision: 'cache-rel'
        });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    expect(controller.timelineLoadingMore).toBe(false);

    // A second concurrent call while a page is in flight must return immediately without
    // ever setting the flag itself (the guard at the top of loadMoreTimeline).
    const first = controller.loadMoreTimeline();
    const second = controller.loadMoreTimeline();
    await Promise.all([first, second]);

    expect(controller.timelineLoadingMore).toBe(false);
    expect(controller.timelineError).toBe('boom');
    // A transient 500 keeps the cursor so a retry can re-attempt the page.
    expect(controller.timelineCursor).toBe('page-2');

    failPage = false;
    await controller.loadMoreTimeline();

    expect(controller.timelineLoadingMore).toBe(false);
    expect(controller.timelineError).toBeNull();
    expect(controller.timelineRows).toEqual([timelineRow('t1'), timelineRow('t2')]);
    expect(controller.timelineCursor).toBeNull();
  });

  it('keeps the timeline cursor when a page fetch throws (network failure), so a retry re-attempts it', async () => {
    let failPage = true;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/1' && request.method === 'GET') return Response.json(person(1));
      if (path === '/api/v1/relationships/1/timeline') {
        const body = (await request.clone().json()) as { cursor?: string };
        if (body.cursor === undefined) {
          return Response.json({
            canonical_id: 1, identity_revision: 1, rows: [timelineRow('t1')], total_count: 2,
            cache_revision: 'cache-rel', next_cursor: 'page-2'
          });
        }
        if (failPage) throw new TypeError('network down');
        return Response.json({
          canonical_id: 1, identity_revision: 1, rows: [timelineRow('t2')], total_count: 2, cache_revision: 'cache-rel'
        });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.openTarget('cluster:1', { filters: [], presentation: 'table' });

    await controller.loadMoreTimeline();

    expect(controller.timelineError).toBe('network down');
    expect(controller.timelineCursor).toBe('page-2');
    expect(controller.timelineLoadingMore).toBe(false);

    failPage = false;
    await controller.loadMoreTimeline();

    expect(controller.timelineError).toBeNull();
    expect(controller.timelineRows).toEqual([timelineRow('t1'), timelineRow('t2')]);
    expect(controller.timelineCursor).toBeNull();
  });

  it('restarts a domain timeline from page 1 exactly once on a 409 archive_revision_changed', async () => {
    let timelineCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/domains/example.com' && request.method === 'GET') {
        return Response.json(domainSummary('example.com'));
      }
      if (path === '/api/v1/domains/example.com/timeline') {
        timelineCalls += 1;
        const body = (await request.clone().json()) as { cursor?: string };
        if (body.cursor === undefined && timelineCalls === 1) {
          return Response.json({
            rows: [timelineRow('t1')], total_count: 3, cache_revision: 'cache-rel',
            search_provenance: {}, next_cursor: 'page-2'
          });
        }
        if (body.cursor === 'page-2') {
          return Response.json(
            { error: 'archive_revision_changed', message: 'The committed analytical cache changed; restart pagination' },
            { status: 409 }
          );
        }
        if (body.cursor === undefined && timelineCalls === 3) {
          return Response.json({
            rows: [timelineRow('t1-restart')], total_count: 1, cache_revision: 'cache-rel-2', search_provenance: {}
          });
        }
        throw new Error(`unexpected timeline call ${timelineCalls} body ${JSON.stringify(body)}`);
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.openTarget('domain:example.com', { filters: [], presentation: 'table' });
    expect(controller.timelineCursor).toBe('page-2');

    await controller.loadMoreTimeline();

    expect(timelineCalls).toBe(3);
    expect(controller.timelineRows).toEqual([timelineRow('t1-restart')]);
    expect(controller.timelineCursor).toBeNull();
    expect(controller.timelineLoadingMore).toBe(false);
    expect(controller.timelineError).toBeNull();
    expect(controller.timelineRestartNotice).toBe('Timeline restarted: the archive changed.');
  });

  it('keeps a domain timeline cursor through a transient failure, so a retry appends the same page', async () => {
    let failPage = true;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/domains/example.com' && request.method === 'GET') {
        return Response.json(domainSummary('example.com'));
      }
      if (path === '/api/v1/domains/example.com/timeline') {
        const body = (await request.clone().json()) as { cursor?: string };
        if (body.cursor === undefined) {
          return Response.json({
            rows: [timelineRow('t1')], total_count: 2, cache_revision: 'cache-rel',
            search_provenance: {}, next_cursor: 'page-2'
          });
        }
        if (failPage) return Response.json({ error: 'internal_error', message: 'boom' }, { status: 500 });
        return Response.json({
          rows: [timelineRow('t2')], total_count: 2, cache_revision: 'cache-rel', search_provenance: {}
        });
      }
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.openTarget('domain:example.com', { filters: [], presentation: 'table' });

    await controller.loadMoreTimeline();

    expect(controller.timelineError).toBe('boom');
    expect(controller.timelineCursor).toBe('page-2');

    failPage = false;
    await controller.loadMoreTimeline();

    expect(controller.timelineError).toBeNull();
    expect(controller.timelineRows).toEqual([timelineRow('t1'), timelineRow('t2')]);
    expect(controller.timelineCursor).toBeNull();
  });
});

describe('RelationshipsController.linkParticipants / unlinkParticipants', () => {
  it('maps 200 to ok with cacheState, and re-opens the target once identityRevision actually changed', async () => {
    let personCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/1' && request.method === 'GET') {
        personCalls += 1;
        return Response.json(person(1));
      }
      if (path === '/api/v1/relationships/1/timeline') {
        return Response.json({
          canonical_id: 1, identity_revision: personCalls, rows: [], total_count: 0, cache_revision: 'cache-rel'
        });
      }
      if (path === '/api/v1/identity/links') return Response.json({ identity_revision: 2, cache_state: 'ready' });
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    expect(controller.identityRevision).toBe(1);

    const outcome = await controller.linkParticipants(1, 2);

    expect(outcome).toEqual({ ok: true, identityRevision: 2, cacheState: 'ready' });
    expect(personCalls).toBe(2);
    expect(controller.identityRevision).toBe(2);
  });

  it('does not re-open the target when identityRevision is unchanged', async () => {
    let personCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/1' && request.method === 'GET') {
        personCalls += 1;
        return Response.json(person(1));
      }
      if (path === '/api/v1/relationships/1/timeline') {
        return Response.json({ canonical_id: 1, identity_revision: 1, rows: [], total_count: 0, cache_revision: 'cache-rel' });
      }
      if (path === '/api/v1/identity/unlinks') return Response.json({ identity_revision: 1, cache_state: 'stale' });
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    expect(personCalls).toBe(1);

    const outcome = await controller.unlinkParticipants(1, 3);

    expect(outcome).toEqual({ ok: true, identityRevision: 1, cacheState: 'stale' });
    expect(personCalls).toBe(1);
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
      if (path === '/api/v1/people/1' && request.method === 'GET') {
        personCalls += 1;
        return Response.json(person(1));
      }
      if (path === '/api/v1/relationships/1/timeline') {
        return Response.json({
          canonical_id: 1, identity_revision: personCalls, rows: [], total_count: 0, cache_revision: 'cache-rel'
        });
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

  it('refreshes detail unconditionally when identityRevision was null after a failed timeline load', async () => {
    let personCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/1' && request.method === 'GET') {
        personCalls += 1;
        return Response.json(person(1));
      }
      if (path === '/api/v1/relationships/1/timeline') {
        return personCalls === 1
          ? Response.json({ error: 'internal_error', message: 'boom' }, { status: 500 })
          : Response.json({
              canonical_id: 1, identity_revision: 3, rows: [], total_count: 0, cache_revision: 'cache-rel'
            });
      }
      if (path === '/api/v1/identity/links') return Response.json({ identity_revision: 3, cache_state: 'ready' });
      throw new Error(`unexpected path ${path}`);
    });
    const controller = new RelationshipsController(createAPIClient(fetchFn), () => 'UTC');
    await controller.openTarget('cluster:1', { filters: [], presentation: 'table' });
    expect(controller.identityRevision).toBeNull();
    expect(personCalls).toBe(1);

    const outcome = await controller.linkParticipants(1, 2);

    expect(outcome).toEqual({ ok: true, identityRevision: 3, cacheState: 'ready' });
    expect(personCalls).toBe(2);
    expect(controller.identityRevision).toBe(3);
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
  it('resets target/detail/timeline state and discards a late in-flight detail response', async () => {
    let resolveTimeline: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/1' && request.method === 'GET') return Response.json(person(1));
      if (path === '/api/v1/relationships/1/timeline') {
        return new Promise<Response>((resolve) => { resolveTimeline = resolve; });
      }
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

    resolveTimeline?.(Response.json({
      canonical_id: 1, identity_revision: 1, rows: [timelineRow('t1')], total_count: 1, cache_revision: 'cache-rel'
    }));
    await openPromise;

    expect(controller.target).toBeNull();
    expect(controller.detail).toBeNull();
    expect(controller.timelineRows).toEqual([]);
  });
});

describe('RelationshipsController abort/destroy', () => {
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
