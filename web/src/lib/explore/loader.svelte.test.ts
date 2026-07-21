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
