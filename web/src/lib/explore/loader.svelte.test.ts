import { flushSync } from 'svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import { ExploreLoader, type ExploreLoaderCallbacks } from './loader.svelte';
import { LOAD_THROUGH_END_MAX_PAGES } from './paging';
import { ExploreState } from './state.svelte';

function exploreResponse(overrides: Record<string, unknown> = {}) {
  return {
    rows: [],
    total_count: 0,
    cache_revision: 'cache-1',
    search_provenance: {},
    ...overrides
  };
}

function entry(index: number) {
  return {
    key: `message:${index}`,
    kind: 'message',
    message_type: 'email',
    conversation_type: 'email',
    title: `Synthetic subject ${index}`,
    preview: `Synthetic excerpt ${index}`,
    occurred_at: '2026-07-18T12:00:00Z',
    source_id: 1,
    source_identifier: 'archive@example.com',
    source_type: 'synthetic',
    participant_labels: ['Example Person'],
    participant_ids: [1],
    attachment_count: 0,
    attachment_size: 0,
    has_attachments: false,
    deleted_from_source: false,
    message_count: 1,
    match: {}
  };
}

function callbacks(overrides: Partial<ExploreLoaderCallbacks> = {}): ExploreLoaderCallbacks {
  return {
    isEnabled: () => true,
    onPredicateChange: () => undefined,
    onPagingNotice: () => undefined,
    onRestorationFocus: () => undefined,
    ...overrides
  };
}

/** Constructs an ExploreLoader inside a standalone effect root (the loader
 * relies on `$effect` internally, which requires an active reactive root
 * outside of a component). Returns the loader plus a teardown that disposes
 * both the root and the loader. */
function setup(
  fetchFn: typeof fetch,
  state: ExploreState,
  callbackOverrides: Partial<ExploreLoaderCallbacks> = {}
): { loader: ExploreLoader; cleanup: () => void } {
  const client = createAPIClient(fetchFn);
  let loader!: ExploreLoader;
  const disposeRoot = $effect.root(() => {
    loader = new ExploreLoader(client, state, callbacks(callbackOverrides));
  });
  flushSync();
  return {
    loader,
    cleanup: () => {
      loader.destroy();
      disposeRoot();
    }
  };
}

describe('ExploreLoader', () => {
  it('issues exactly one fetch for one filter commit', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let exploreCount = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path !== '/api/v1/explore') return Response.json(exploreResponse());
      exploreCount += 1;
      return Response.json(exploreResponse({ rows: [entry(1)] }));
    });
    const state = new ExploreState(window);
    const { loader, cleanup } = setup(fetchFn, state);
    await vi.waitFor(() => expect(loader.rows).toHaveLength(1));
    expect(exploreCount).toBe(1);

    state.commitNavigation({ filters: [{ dimension: 'source', values: ['1'] }] });
    flushSync();
    await vi.waitFor(() => expect(exploreCount).toBe(2));

    cleanup();
    state.destroy();
  });

  it('does not refetch when an equal-content filters array replaces the current one by reference', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let exploreCount = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path !== '/api/v1/explore') return Response.json(exploreResponse());
      exploreCount += 1;
      return Response.json(exploreResponse({ rows: [entry(1)] }));
    });
    const state = new ExploreState(window);
    const { cleanup } = setup(fetchFn, state);
    await vi.waitFor(() => expect(exploreCount).toBe(1));

    state.replaceTransient({ activeRow: 'message:1' });
    flushSync();
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(exploreCount).toBe(1);

    cleanup();
    state.destroy();
  });

  it('caps loadThroughEnd at LOAD_THROUGH_END_MAX_PAGES and reports a paging notice', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let explorePostCount = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path !== '/api/v1/explore') return Response.json(exploreResponse());
      explorePostCount += 1;
      const page = explorePostCount;
      return Response.json(exploreResponse({
        rows: [entry(page)], total_count: 10_000, next_cursor: `cursor-${page}`
      }));
    });
    const state = new ExploreState(window);
    const notices: (string | null)[] = [];
    const { loader, cleanup } = setup(fetchFn, state, { onPagingNotice: (message) => notices.push(message) });
    await vi.waitFor(() => expect(loader.rows).toHaveLength(1));

    await loader.loadThroughEnd();

    expect(explorePostCount).toBe(1 + LOAD_THROUGH_END_MAX_PAGES);
    expect(notices).toEqual([expect.stringContaining('press End again to continue')]);

    cleanup();
    state.destroy();
  });

  it('clears the paging notice once a later loadThroughEnd reaches the true end', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let explorePostCount = 0;
    const totalPages = 1 + LOAD_THROUGH_END_MAX_PAGES + 2;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path !== '/api/v1/explore') return Response.json(exploreResponse());
      explorePostCount += 1;
      const page = explorePostCount;
      return Response.json(exploreResponse({
        rows: [entry(page)], total_count: 10_000,
        ...(page < totalPages ? { next_cursor: `cursor-${page}` } : {})
      }));
    });
    const state = new ExploreState(window);
    const notices: (string | null)[] = [];
    const { loader, cleanup } = setup(fetchFn, state, { onPagingNotice: (message) => notices.push(message) });
    await vi.waitFor(() => expect(loader.rows).toHaveLength(1));

    await loader.loadThroughEnd();
    expect(notices.at(-1)).toMatch(/press End again to continue/);

    await loader.loadThroughEnd();
    expect(explorePostCount).toBe(totalPages);
    expect(notices.at(-1)).toBeNull();

    cleanup();
    state.destroy();
  });

  it('walks cursor pages during restoration until the durable selected row becomes visible', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let explorePostCount = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path !== '/api/v1/explore') return Response.json(exploreResponse());
      explorePostCount += 1;
      const page = explorePostCount;
      return Response.json(exploreResponse({
        rows: [entry(page)], total_count: 10_000,
        ...(page < 4 ? { next_cursor: `cursor-${page}` } : {})
      }));
    });
    const state = new ExploreState(window);
    state.replaceTransient({ selectedRow: 'message:3' });
    const restorationFocusCalls: number[] = [];
    const { loader, cleanup } = setup(fetchFn, state, {
      onRestorationFocus: () => restorationFocusCalls.push(1)
    });

    await vi.waitFor(() => expect(loader.rows.some((row) => row.key === 'message:3')).toBe(true));
    expect(explorePostCount).toBe(3);
    await vi.waitFor(() => expect(state.peekRestorationEpoch()).toBeUndefined());
    expect(restorationFocusCalls).toEqual([]);

    cleanup();
    state.destroy();
  });

  it('keeps loaded rows and retries the same cursor after a transient load-more failure', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const cursorsSeen: (string | undefined)[] = [];
    let cursorCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input as Request;
      if (new URL(request.url).pathname !== '/api/v1/explore') return Response.json(exploreResponse());
      const body = await request.clone().json() as { cursor?: string };
      if (!body.cursor) {
        return Response.json(exploreResponse({ rows: [entry(1)], total_count: 2, next_cursor: 'cursor-1' }));
      }
      cursorsSeen.push(body.cursor);
      cursorCalls += 1;
      if (cursorCalls === 1) return Response.json({ message: 'Synthetic page failure' }, { status: 500 });
      return Response.json(exploreResponse({ rows: [entry(2)], total_count: 2 }));
    });
    const state = new ExploreState(window);
    const { loader, cleanup } = setup(fetchFn, state);
    await vi.waitFor(() => expect(loader.rows).toHaveLength(1));

    const failed = await loader.loadMore();
    expect(failed.status).toBe('failed');
    expect(loader.rows).toHaveLength(1);
    expect(loader.pageError).toBe('Synthetic page failure');
    expect(loader.error).toBe('');
    expect(loader.unavailable).toBeUndefined();
    expect(loader.nextCursor).toBe('cursor-1');

    const retried = await loader.loadMore();
    expect(retried.status).toBe('exhausted');
    expect(cursorsSeen).toEqual(['cursor-1', 'cursor-1']);
    expect(loader.rows.map((row) => row.key)).toEqual(['message:1', 'message:2']);
    expect(loader.pageError).toBe('');

    cleanup();
    state.destroy();
  });

  it('does not flip the global unavailable state when a cursor page returns 503', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input as Request;
      if (new URL(request.url).pathname !== '/api/v1/explore') return Response.json(exploreResponse());
      const body = await request.clone().json() as { cursor?: string };
      if (!body.cursor) {
        return Response.json(exploreResponse({ rows: [entry(1)], total_count: 2, next_cursor: 'cursor-1' }));
      }
      return Response.json({
        error: 'cache_unavailable',
        message: 'The analytical cache is rebuilding.',
        readiness: 'building',
        recovery_action: 'msgvault build-cache'
      }, { status: 503 });
    });
    const state = new ExploreState(window);
    const { loader, cleanup } = setup(fetchFn, state);
    await vi.waitFor(() => expect(loader.rows).toHaveLength(1));

    const outcome = await loader.loadMore();

    expect(outcome.status).toBe('failed');
    expect(loader.rows).toHaveLength(1);
    expect(loader.unavailable).toBeUndefined();
    expect(loader.error).toBe('');
    expect(loader.pageError).toBe('The analytical cache is rebuilding.');
    expect(loader.nextCursor).toBe('cursor-1');

    cleanup();
    state.destroy();
  });

  it('keeps loaded groups and retries the same grouped cursor after a failure', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let cursorCalls = 0;
    const group = (index: number) => ({
      key: String(index), label: `Group ${index}`, count: index,
      estimated_bytes: 10, latest_at: '2026-07-18T12:00:00Z'
    });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input as Request;
      if (new URL(request.url).pathname !== '/api/v1/explore/groups') return Response.json(exploreResponse());
      const body = await request.clone().json() as { cursor?: string };
      if (!body.cursor) {
        return Response.json(exploreResponse({ rows: [group(1)], total_count: 2, next_cursor: 'group-cursor-1' }));
      }
      cursorCalls += 1;
      if (cursorCalls === 1) return Response.json({ message: 'Synthetic group page failure' }, { status: 500 });
      return Response.json(exploreResponse({ rows: [group(2)], total_count: 2 }));
    });
    const state = new ExploreState(window);
    state.commitGrouping('source');
    const { loader, cleanup } = setup(fetchFn, state);
    await vi.waitFor(() => expect(loader.groupRows).toHaveLength(1));

    const failed = await loader.loadMore();
    expect(failed.status).toBe('failed');
    expect(loader.groupRows).toHaveLength(1);
    expect(loader.pageError).toBe('Synthetic group page failure');
    expect(loader.error).toBe('');

    const retried = await loader.loadMore();
    expect(retried.status).toBe('exhausted');
    expect(loader.groupRows.map((row) => row.key)).toEqual(['1', '2']);
    expect(loader.pageError).toBe('');

    cleanup();
    state.destroy();
  });

  it('keeps a terminal authority-change failure inline while dropping the cursor', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input as Request;
      if (new URL(request.url).pathname !== '/api/v1/explore') return Response.json(exploreResponse());
      const body = await request.clone().json() as { cursor?: string };
      if (!body.cursor) {
        return Response.json(exploreResponse({ rows: [entry(1)], total_count: 2, next_cursor: 'cursor-1' }));
      }
      return Response.json(exploreResponse({
        rows: [entry(2)], total_count: 2, cache_revision: 'cache-2'
      }));
    });
    const state = new ExploreState(window);
    const { loader, cleanup } = setup(fetchFn, state);
    await vi.waitFor(() => expect(loader.rows).toHaveLength(1));

    const outcome = await loader.loadMore();

    expect(outcome.status).toBe('failed');
    expect(loader.rows).toHaveLength(1);
    expect(loader.pageError).toContain('Reload this view.');
    expect(loader.error).toBe('');
    expect(loader.nextCursor).toBeUndefined();

    cleanup();
    state.destroy();
  });

  it('leaves initial-load failures on the global error state', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input as Request;
      if (new URL(request.url).pathname !== '/api/v1/explore') return Response.json(exploreResponse());
      return Response.json({ message: 'Synthetic initial failure' }, { status: 500 });
    });
    const state = new ExploreState(window);
    const { loader, cleanup } = setup(fetchFn, state);

    await vi.waitFor(() => expect(loader.error).toBe('Synthetic initial failure'));
    expect(loader.rows).toHaveLength(0);
    expect(loader.pageError).toBe('');

    cleanup();
    state.destroy();
  });

  it('ignores a superseded cursor-page failure', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let rejectCursorPage: ((cause: Error) => void) | undefined;
    let exploreCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input as Request;
      if (new URL(request.url).pathname !== '/api/v1/explore') return Response.json(exploreResponse());
      const body = await request.clone().json() as { cursor?: string };
      exploreCalls += 1;
      if (body.cursor) {
        return new Promise<Response>((_, reject) => {
          rejectCursorPage = reject;
        });
      }
      return Response.json(exploreResponse({
        rows: [entry(exploreCalls)], total_count: 2,
        ...(exploreCalls === 1 ? { next_cursor: 'cursor-1' } : {})
      }));
    });
    const state = new ExploreState(window);
    const { loader, cleanup } = setup(fetchFn, state);
    await vi.waitFor(() => expect(loader.rows).toHaveLength(1));

    const pending = loader.loadMore();
    await vi.waitFor(() => expect(rejectCursorPage).toBeDefined());
    state.commitNavigation({ filters: [{ dimension: 'source', values: ['1'] }] });
    flushSync();
    rejectCursorPage!(new Error('Synthetic superseded failure'));

    const outcome = await pending;
    expect(outcome.status).toBe('stale');
    expect(loader.pageError).toBe('');
    expect(loader.error).toBe('');

    cleanup();
    state.destroy();
  });

  it('clears stale rows and result the moment a fresh predicate load starts', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let resolveSecond: ((response: Response) => void) | undefined;
    let exploreCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path !== '/api/v1/explore') return Response.json(exploreResponse());
      exploreCalls += 1;
      if (exploreCalls === 1) {
        return Response.json(exploreResponse({ rows: [entry(1)], total_count: 1 }));
      }
      return new Promise<Response>((resolve) => {
        resolveSecond = resolve;
      });
    });
    const state = new ExploreState(window);
    const { loader, cleanup } = setup(fetchFn, state);
    await vi.waitFor(() => expect(loader.rows).toHaveLength(1));
    expect(loader.result).toBeDefined();

    state.commitNavigation({ filters: [{ dimension: 'source', values: ['1'] }] });
    flushSync();

    expect(loader.rows).toHaveLength(0);
    expect(loader.result).toBeUndefined();
    expect(loader.loading).toBe(true);

    await vi.waitFor(() => expect(resolveSecond).toBeDefined());
    resolveSecond!(Response.json(exploreResponse({ rows: [entry(2)], total_count: 1 })));
    await vi.waitFor(() => expect(loader.rows.map((row) => row.key)).toEqual(['message:2']));
    expect(loader.result?.totalCount).toBe(1);
    expect(loader.loading).toBe(false);

    cleanup();
    state.destroy();
  });

  it('ignores a superseded fresh-load response that resolves after a newer predicate load', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let resolveFirst: ((response: Response) => void) | undefined;
    let exploreCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path !== '/api/v1/explore') return Response.json(exploreResponse());
      exploreCalls += 1;
      if (exploreCalls === 1) {
        return new Promise<Response>((resolve) => {
          resolveFirst = resolve;
        });
      }
      return Response.json(exploreResponse({ rows: [entry(2)], total_count: 1 }));
    });
    const state = new ExploreState(window);
    const { loader, cleanup } = setup(fetchFn, state);
    await vi.waitFor(() => expect(resolveFirst).toBeDefined());

    state.commitNavigation({ filters: [{ dimension: 'source', values: ['1'] }] });
    flushSync();
    await vi.waitFor(() => expect(loader.rows.map((row) => row.key)).toEqual(['message:2']));

    resolveFirst!(Response.json(exploreResponse({ rows: [entry(1)], total_count: 1 })));
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(loader.rows.map((row) => row.key)).toEqual(['message:2']);
    expect(loader.error).toBe('');

    cleanup();
    state.destroy();
  });

  it('exhausts finite pages once when a distinct selected row is missing', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let explorePostCount = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path !== '/api/v1/explore') return Response.json(exploreResponse());
      explorePostCount += 1;
      const page = explorePostCount;
      return Response.json(exploreResponse({
        rows: [entry(page)], total_count: 10_000,
        ...(page < 3 ? { next_cursor: `cursor-${page}` } : {})
      }));
    });
    const state = new ExploreState(window);
    state.replaceTransient({ selectedRow: 'message:missing' });
    const { loader, cleanup } = setup(fetchFn, state);

    await vi.waitFor(() => expect(state.peekRestorationEpoch()).toBeUndefined());
    expect(explorePostCount).toBe(3);
    expect(loader.nextCursor).toBeUndefined();

    cleanup();
    state.destroy();
  });
});
