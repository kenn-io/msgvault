import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { appShortcuts } from '@kenn-io/kit-ui';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { LOAD_THROUGH_END_MAX_PAGES } from '../../explore/paging';
import { ExploreState, parseExploreURLState } from '../../explore/state.svelte';
import AppShell from './AppShell.svelte';

function exploreResponse(overrides: Record<string, unknown> = {}) {
  return {
    rows: [],
    total_count: 0,
    cache_revision: 'cache-1',
    search_provenance: {},
    ...overrides
  };
}

describe('EverythingWorkspace', () => {
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

  it('keeps requested Semantic mode selected while showing incomplete coverage and a search error', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/coverage')) return Response.json({
        status: 'incomplete', eligible_count: 2, embedded_count: 1, percentage: 50,
        vector_generation: 7, cache_revision: 'cache-1', actions: []
      });
      return Response.json({ error: 'vector_initializing', message: 'Vector search is still building.' }, { status: 503 });
    });
    const state = new ExploreState(window);
    state.replaceSearchDraft('alpha', 'semantic');
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect(await screen.findByText(/Semantic index: 50% of 2 items/)).toBeDefined();
    expect((await screen.findByRole('alert')).textContent).toContain('Vector search is still building.');
    expect(screen.getByRole('radio', { name: 'Semantic' }).getAttribute('aria-checked')).toBe('true');
    expect(state.current.searchMode).toBe('semantic');
    rendered.unmount();
    state.destroy();
  });


  it('polls initializing semantic coverage until it reaches a terminal state', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let coverageCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path.endsWith('/coverage')) {
        coverageCalls += 1;
        return Response.json({
          status: coverageCalls === 1 ? 'initializing' : 'ready', eligible_count: 2,
          embedded_count: coverageCalls === 1 ? 0 : 2, percentage: coverageCalls === 1 ? 0 : 100,
          cache_revision: 'cache-1', actions: []
        });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.replaceSearchDraft('', 'semantic');
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      await screen.findByText('Semantic index is initializing.');
      await vi.advanceTimersByTimeAsync(2_000);
      expect(await screen.findByText('Semantic index: 100% of 2 items.')).toBeDefined();
      expect(coverageCalls).toBeGreaterThanOrEqual(2);
    } finally {
      rendered.unmount();
      state.destroy();
      vi.useRealTimers();
    }
  });


  it('backs off exponentially while semantic coverage stays initializing', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let coverageCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path.endsWith('/coverage')) {
        coverageCalls += 1;
        return Response.json({
          status: 'initializing', eligible_count: 2, embedded_count: 0, percentage: 0,
          cache_revision: 'cache-1', actions: []
        });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.replaceSearchDraft('', 'semantic');
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      await screen.findByText('Semantic index is initializing.');
      expect(coverageCalls).toBe(1);

      await vi.advanceTimersByTimeAsync(1_000);
      expect(coverageCalls).toBe(2);

      await vi.advanceTimersByTimeAsync(2_000);
      expect(coverageCalls).toBe(3);

      await vi.advanceTimersByTimeAsync(4_000);
      expect(coverageCalls).toBe(4);
    } finally {
      rendered.unmount();
      state.destroy();
      vi.useRealTimers();
    }
  });


  it('resets the poll backoff after a coverage error so a later retry starts at 1s again', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let coverageCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path.endsWith('/coverage')) {
        coverageCalls += 1;
        // The first two calls back off normally, then fail once, then the
        // manual retry (call 4) and its follow-up poll (call 5) resume
        // initializing.
        if (coverageCalls === 3) throw new Error('Coverage endpoint unavailable.');
        return Response.json({
          status: 'initializing', eligible_count: 2, embedded_count: 0, percentage: 0,
          cache_revision: 'cache-1', actions: []
        });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.replaceSearchDraft('', 'semantic');
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      await screen.findByText('Semantic index is initializing.');
      expect(coverageCalls).toBe(1);

      await vi.advanceTimersByTimeAsync(1_000);
      expect(coverageCalls).toBe(2);

      await vi.advanceTimersByTimeAsync(2_000);
      expect(await screen.findByText('Coverage endpoint unavailable.')).toBeDefined();
      expect(coverageCalls).toBe(3);

      await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
      await waitFor(() => expect(coverageCalls).toBe(4));
      await screen.findByText('Semantic index is initializing.');

      // If the backoff counter had not been reset on the error, this poll
      // would be scheduled several seconds out instead of at 1s.
      await vi.advanceTimersByTimeAsync(1_000);
      expect(coverageCalls).toBe(5);
    } finally {
      rendered.unmount();
      state.destroy();
      vi.useRealTimers();
    }
  });


  it('does not install a coverage poll after an aborted request settles during teardown', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let resolveCoverage!: (response: Response) => void;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path.endsWith('/coverage')) return new Promise<Response>((resolve) => { resolveCoverage = resolve; });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.replaceSearchDraft('', 'semantic');
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    await waitFor(() => expect(resolveCoverage).toBeTypeOf('function'));

    rendered.unmount();
    resolveCoverage(Response.json({
      status: 'initializing', eligible_count: 2, embedded_count: 0,
      percentage: 0, cache_revision: 'cache-1', actions: []
    }));
    await Promise.resolve();
    await Promise.resolve();
    expect(vi.getTimerCount()).toBe(0);
    await vi.advanceTimersByTimeAsync(2_000);

    expect(fetchFn.mock.calls.filter(([input]) => new URL(input instanceof Request ? input.url : String(input)).pathname.endsWith('/coverage'))).toHaveLength(1);
    state.destroy();
    vi.useRealTimers();
  });


  it('runs a full rebuild for stale coverage and refreshes the named status after completion', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const requests: Request[] = [];
    let coverageCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/coverage')) {
        coverageCalls += 1;
        return Response.json({
          status: coverageCalls === 1 ? 'stale' : 'ready',
          eligible_count: 2, embedded_count: 2, percentage: 100,
          vector_generation: coverageCalls === 1 ? 7 : 8,
          cache_revision: 'cache-1', actions: coverageCalls === 1 ? ['build_index'] : []
        });
      }
      if (path.endsWith('/cli/run')) {
        return new Response(`${JSON.stringify({ type: 'complete' })}\n`, {
          headers: { 'Content-Type': 'application/x-ndjson' }
        });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.replaceSearchDraft('', 'semantic');
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    await screen.findByText('Semantic index is stale.');
    await fireEvent.click(screen.getByRole('button', { name: 'Build index' }));
    expect(requests.some((request) => new URL(request.url).pathname.endsWith('/cli/run'))).toBe(false);
    await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(requests.some((request) => new URL(request.url).pathname.endsWith('/cli/run'))).toBe(false);

    await fireEvent.click(screen.getByRole('button', { name: 'Build index' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm full rebuild' }));
    await screen.findByText('Semantic index: 100% of 2 items.');

    expect(coverageCalls).toBe(2);
    const cliRequest = requests.find((request) => new URL(request.url).pathname.endsWith('/cli/run'));
    await expect(cliRequest?.clone().json()).resolves.toEqual({
      args: ['embeddings', 'build', '--full-rebuild', '--yes']
    });
    expect(screen.getByRole('radio', { name: 'Semantic' }).getAttribute('aria-checked')).toBe('true');
    rendered.unmount();
    state.destroy();
  });


  it('surfaces a streamed build failure without switching the requested mode', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/coverage')) return Response.json({
        status: 'stale', eligible_count: 2, embedded_count: 1, percentage: 50,
        vector_generation: 7, cache_revision: 'cache-1', actions: ['build_index']
      });
      if (path.endsWith('/cli/run')) return new Response([
        JSON.stringify({ type: 'stdout', data: 'starting\n' }),
        JSON.stringify({ type: 'error', error: 'embedding endpoint failed' })
      ].join('\n') + '\n', { headers: { 'Content-Type': 'application/x-ndjson' } });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.replaceSearchDraft('', 'hybrid');
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    await screen.findByText('Semantic index is stale.');
    await fireEvent.click(screen.getByRole('button', { name: 'Build index' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm full rebuild' }));

    expect(await screen.findByText('embedding endpoint failed')).toBeDefined();
    expect(screen.getByRole('radio', { name: 'Hybrid' }).getAttribute('aria-checked')).toBe('true');
    expect(state.current.searchMode).toBe('hybrid');
    rendered.unmount();
    state.destroy();
  });


  it('uses selection preflight as the sole authority for shell actions', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const preflightRequests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname.endsWith('/explore/preflight')) {
        preflightRequests.push(request);
        return Response.json({
          count: 1, estimated_bytes: 10, cache_revision: 'cache-1', search_provenance: {},
          unavailable_actions: [
            { action: 'export', reason: 'selection_contains_items_without_exportable_files' },
            { action: 'open_in_source', reason: 'trusted_source_link_unavailable' }
          ],
          action_targets: [],
          operation_token: 'operation-1', expires_at: '2026-07-19T10:05:00Z'
        });
      }
      return Response.json(exploreResponse({ rows: [entry(1)], total_count: 1 }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Everything results' });
    await screen.findByText('Synthetic subject 1');
    grid.focus();
    await fireEvent.keyDown(grid, { key: ' ' });

    expect(await screen.findByText('Export: selection_contains_items_without_exportable_files')).toBeDefined();
    expect(screen.getByText('Open in source: trusted_source_link_unavailable')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Export selection' })).toBeNull();
    const body = await preflightRequests[0]!.clone().json();
    expect(body.selection).toMatchObject({
      mode: 'explicit', row_keys: ['message:1'], cache_revision: 'cache-1'
    });

    rendered.unmount();
    state.destroy();
  });


  it('downloads the exact server-authorized raw message export target', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const requests: Request[] = [];
    const createObjectURL = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:raw-message');
    const revokeObjectURL = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    const anchorClick = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/explore/preflight')) return Response.json({
        count: 1, estimated_bytes: 10, cache_revision: 'cache-1', search_provenance: {},
        unavailable_actions: [{ action: 'open_in_source', reason: 'trusted_source_link_unavailable' }],
        action_targets: [{ action: 'export', message_id: 1, filename: 'message-1.eml' }],
        operation_token: 'operation-1', expires_at: '2026-07-19T10:05:00Z'
      });
      if (path.endsWith('/cli/message/raw')) {
        return new Response('raw mime', { headers: { 'Content-Type': 'message/rfc822' } });
      }
      return Response.json(exploreResponse({ rows: [entry(1)], total_count: 1 }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Everything results' });
    await screen.findByText('Synthetic subject 1');
    grid.focus();
    await fireEvent.keyDown(grid, { key: ' ' });

    await fireEvent.click(await screen.findByRole('button', { name: 'Export selection' }));

    await waitFor(() => expect(requests.some((request) => {
      const url = new URL(request.url);
      return url.pathname.endsWith('/cli/message/raw') && url.searchParams.get('id') === '1';
    })).toBe(true));
    await waitFor(() => expect(createObjectURL).toHaveBeenCalledOnce());
    expect(anchorClick).toHaveBeenCalledOnce();
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:raw-message');
    rendered.unmount();
    state.destroy();
  });


  it('records a transient scroll anchor without refetching the durable predicate', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return Response.json({
        rows: [
          {
            key: 'message:1',
            kind: 'message',
            message_type: 'email',
            conversation_type: 'email',
            title: 'Synthetic subject',
            preview: 'Synthetic excerpt',
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
          }
        ],
        total_count: 1,
        cache_revision: 'cache-1',
        search_provenance: { lexical_index_revision: 'fts-1' }
      });
    });
    const state = new ExploreState(window);
    render(AppShell, { client: createAPIClient(fetchFn), state, enabled: true });
    const grid = await screen.findByRole('grid', { name: 'Everything results' });
    await waitFor(() => expect(requests).toHaveLength(1));
    await screen.findByText('Synthetic subject');

    Object.defineProperty(grid, 'scrollTop', { configurable: true, value: 12, writable: true });
    await fireEvent.scroll(grid);
    await waitFor(() => expect(state.current.scrollAnchor).toEqual({ key: 'message:1', offset: 12 }));

    expect(requests).toHaveLength(1);
    state.destroy();
  });


  it('does not refetch when an equal-content filters array replaces the current one by reference', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    render(AppShell, { client: createAPIClient(fetchFn), state, enabled: true });
    await screen.findByRole('grid', { name: 'Everything results' });
    await waitFor(() => expect(requests).toHaveLength(1));

    state.replaceTransient({ filters: [{ dimension: 'domain', values: ['example.com'] }] });
    await waitFor(() => expect(requests).toHaveLength(2));

    // A fresh array with the same filter content must not change the
    // canonical predicate fingerprint, so no new request should fire.
    state.replaceTransient({ filters: [{ dimension: 'domain', values: ['example.com'] }] });
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(requests).toHaveLength(2);

    // A genuine content change is a different fingerprint and must refetch.
    state.replaceTransient({ filters: [{ dimension: 'domain', values: ['other.example'] }] });
    await waitFor(() => expect(requests).toHaveLength(3));
    state.destroy();
  });


  it('debounces rapid visible conversation churn into one exact-count request', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const countRequests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/match-counts')) {
        countRequests.push(request);
        const body = await request.clone().json();
        return Response.json({
          counts: body.row_keys.map((row_key: string) => ({ row_key, count: 1 })),
          cache_revision: 'cache-1', lexical_index_revision: 'fts-1',
          canonical_query_hash: 'query-1'
        });
      }
      return Response.json(exploreResponse({
        rows: Array.from({ length: 100 }, (_, index) => ({
          ...entry(index + 1), kind: 'conversation', message_type: 'sms'
        })),
        total_count: 100,
        search_provenance: { lexical_index_revision: 'fts-1' }
      }));
    });
    const state = new ExploreState(window);
    state.replaceTransient({ query: 'alpha', searchMode: 'full_text' });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      const grid = await screen.findByRole('grid', { name: 'Everything results' }) as HTMLDivElement;
      await screen.findByText('Synthetic subject 1');
      for (const scrollTop of [36, 72, 108, 144]) {
        Object.defineProperty(grid, 'scrollTop', { configurable: true, value: scrollTop, writable: true });
        await fireEvent.scroll(grid);
      }

      await vi.advanceTimersByTimeAsync(100);
      await waitFor(() => expect(countRequests).toHaveLength(1));
    } finally {
      rendered.unmount();
      state.destroy();
      vi.useRealTimers();
    }
  });


  it('keeps a cached exact lexical match count across a workspace round-trip away from Everything and back', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let matchCountCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/match-counts')) {
        matchCountCalls += 1;
        return Response.json({
          counts: [{ row_key: 'message:1', count: 3 }], cache_revision: 'cache-1',
          lexical_index_revision: 'fts-1', canonical_query_hash: 'query-1'
        });
      }
      return Response.json(exploreResponse({
        rows: [{ ...entry(1), kind: 'conversation', message_type: 'sms' }], total_count: 1,
        search_provenance: { lexical_index_revision: 'fts-1' }
      }));
    });
    const state = new ExploreState(window);
    state.replaceTransient({ query: 'alpha', searchMode: 'full_text' });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      await screen.findByText('Synthetic subject 1');
      await vi.advanceTimersByTimeAsync(100);
      await waitFor(() => expect(matchCountCalls).toBe(1));
      await screen.findByText('3 lexical matches');

      // Leave Everything (destroying EverythingWorkspace) and come back.
      const nav = screen.getByRole('navigation', { name: 'Primary' });
      await fireEvent.click(within(nav).getByRole('button', { name: 'Settings' }));
      expect(screen.queryByRole('main', { name: 'Everything' })).toBeNull();
      await fireEvent.click(within(nav).getByRole('button', { name: 'Everything' }));

      await screen.findByText('Synthetic subject 1');
      await vi.advanceTimersByTimeAsync(100);

      // The cached count must be reused, not refetched, after the round-trip.
      expect(await screen.findByText('3 lexical matches')).toBeDefined();
      expect(matchCountCalls).toBe(1);
    } finally {
      rendered.unmount();
      state.destroy();
      vi.useRealTimers();
    }
  });

  it('refetches a persisted group reading-pane detail after the analytical cache rebuilds', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let cacheRevision = 'cache-1';
    let count = 12;
    let label = 'Group Seven v1';
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/groups')) {
        const row = { key: '7', label, count, estimated_bytes: 42, latest_at: '2026-07-18T12:00:00Z' };
        return Response.json({ rows: [row], total_count: 1, cache_revision: cacheRevision, search_provenance: {} });
      }
      return Response.json(exploreResponse({ cache_revision: cacheRevision }));
    });
    const state = new ExploreState(window);
    state.commitNavigation({ groupingChain: ['source'] });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    await screen.findByText('Group Seven v1');

    await fireEvent.click(screen.getByRole('button', { name: 'Drill into Group Seven v1' }));
    expect(await screen.findByRole('complementary', { name: 'Reading pane: Group Seven v1' })).toBeDefined();
    expect(await screen.findByText(/12 items/)).toBeDefined();

    // Leave Everything via a workspace switch (destroying EverythingWorkspace,
    // but not the session that persists the loaded detail — commitWorkspace
    // resets selectedRow to null, so a normal round-trip through the
    // Everything tab would already wipe the cached detail; use Back instead,
    // which restores the exact prior committed state — including the same
    // selected group — while still tearing down and recreating the
    // component) and simulate an analytical cache rebuild — same key and
    // predicate, but the underlying data changed — while away.
    const nav = screen.getByRole('navigation', { name: 'Primary' });
    await fireEvent.click(within(nav).getByRole('button', { name: 'Settings' }));
    expect(screen.queryByRole('main', { name: 'Everything' })).toBeNull();
    cacheRevision = 'cache-2';
    count = 99;
    label = 'Group Seven v2';
    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));

    // Must reflect the rebuilt data, not the detail persisted from before
    // the rebuild under the same group key and predicate.
    expect(await screen.findByRole('complementary', { name: 'Reading pane: Group Seven v2' })).toBeDefined();
    expect(await screen.findByText(/99 items/)).toBeDefined();
    expect(screen.queryByText(/12 items/)).toBeNull();
    rendered.unmount();
    state.destroy();
  });

  // This coverage previously drove AppShell into the legacy People branch
  // (workspace: 'people') to exercise debouncedSearchPatch's integration
  // mechanics. normalize() now eagerly rewrites 'people' to 'relationships'
  // on every navigation, so that branch is unreachable through ExploreState.
  // The same debounce/flush/cancel mechanics are still live and reachable
  // through the Files workspace's filename-query input, so these three
  // tests exercise them there instead.

  it('aborts an in-flight exact-count request on visible churn and on destroy', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const countSignals: AbortSignal[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname.endsWith('/match-counts')) {
        countSignals.push(request.signal);
        return new Promise<Response>(() => undefined);
      }
      return Response.json(exploreResponse({
        rows: Array.from({ length: 100 }, (_, index) => ({
          ...entry(index + 1), kind: 'conversation', message_type: 'sms'
        })),
        total_count: 100,
        search_provenance: { lexical_index_revision: 'fts-1' }
      }));
    });
    const state = new ExploreState(window);
    state.replaceTransient({ query: 'alpha', searchMode: 'full_text' });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      const grid = await screen.findByRole('grid', { name: 'Everything results' }) as HTMLDivElement;
      await vi.advanceTimersByTimeAsync(60);
      await waitFor(() => expect(countSignals).toHaveLength(1));

      Object.defineProperty(grid, 'scrollTop', { configurable: true, value: 180, writable: true });
      await fireEvent.scroll(grid);
      expect(countSignals[0]!.aborted).toBe(true);
      await vi.advanceTimersByTimeAsync(60);
      await waitFor(() => expect(countSignals).toHaveLength(2));

      rendered.unmount();
      expect(countSignals[1]!.aborted).toBe(true);
    } finally {
      state.destroy();
      vi.useRealTimers();
    }
  });


  it('uses one analytical context for grouped, timeline, and files presentations', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/files/7')) return Response.json({
        id: 7, message_id: 1, conversation_id: 11, filename: 'analysis.pdf',
        mime_type: 'application/pdf', size_bytes: 2048,
        content_state: 'missing_blob', content_available: false
      });
      if (path.endsWith('/explore/files')) return Response.json({
        files: [{
          id: 7, key: 'message:1:file:7', entry_key: 'message:1', message_id: 1, conversation_id: 11,
          occurred_at: '2026-07-18T12:00:00Z', source_id: 1,
          source_identifier: 'archive@example.com', title: 'Synthetic subject 1',
          filename: 'analysis.pdf', mime_type: 'application/pdf', size: 2048
        }],
        total_count: 1, cache_revision: 'cache-1', search_provenance: {}
      });
      return Response.json(exploreResponse({ rows: [entry(1)], total_count: 1 }));
    });
    const state = new ExploreState(window);
    state.commitNavigation({ groupingChain: ['source'] });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    await waitFor(() => expect(requests).toHaveLength(1));
    expect(new URL(requests[0]!.url).pathname).toBe('/api/v1/explore/groups');

    state.commitNavigation({ groupingChain: [], presentation: 'timeline' });
    expect(await screen.findByRole('region', { name: 'Canonical activity timeline' })).toBeDefined();
    expect(await screen.findByText('Synthetic subject 1')).toBeDefined();
    expect(new URL(requests.at(-1)!.url).pathname).toBe('/api/v1/explore');

    state.commitNavigation({ presentation: 'files' });
    expect(await screen.findByRole('grid', { name: 'Files in current context' })).toBeDefined();
    expect(await screen.findByText('analysis.pdf')).toBeDefined();
    const filesRequest = requests.at(-1)!;
    expect(new URL(filesRequest.url).pathname).toBe('/api/v1/explore/files');
    await expect(filesRequest.clone().json()).resolves.toMatchObject({
      predicate: { presentation: 'files' }
    });
    state.replaceTransient({
      activeRow: 'message:1:file:7',
      scrollAnchor: { key: 'message:1:file:7', offset: 4 }
    });
    const filesGrid = screen.getByRole('grid', { name: 'Files in current context' });
    filesGrid.focus();
    await fireEvent.keyDown(filesGrid, { key: 'Enter' });
    await waitFor(() => expect(state.current.selectedRow).toBe('attachment:7'));
    expect(await screen.findByRole('dialog', { name: 'View analysis.pdf' })).toBeDefined();
    expect(state.current.selectedRow).toBe('attachment:7');
    expect(parseExploreURLState(window.location.search).selectedRow).toBe('attachment:7');
    await fireEvent.click(screen.getByRole('button', { name: 'Close file viewer' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'View analysis.pdf' })).toBeNull());
    expect(state.current.selectedRow).toBeNull();
    expect(document.activeElement).toBe(filesGrid);

    await screen.findByText('analysis.pdf');
    const restoredFilesGrid = screen.getByRole('grid', { name: 'Files in current context' });
    await waitFor(() => expect(restoredFilesGrid.getAttribute('aria-activedescendant')).toContain('message-3a-1'));
    restoredFilesGrid.focus();
    await fireEvent.keyDown(restoredFilesGrid, { key: 'Enter' });
    await waitFor(() => expect(state.current.selectedRow).toBe('attachment:7'));
    expect(await screen.findByRole('dialog', { name: 'View analysis.pdf' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Open containing item' }));
    expect(state.current.presentation).toBe('table');
    expect(state.current.selectedRow).toBe('message:1');
    expect(state.current.activeRow).toBeNull();
    expect(state.current.scrollAnchor).toBeNull();
    rendered.unmount();
    state.destroy();
  });


  it('renders dense group rows and drills a supported dimension into entry filters', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (new URL(request.url).pathname.endsWith('/groups')) {
        return Response.json({
          rows: [{ key: '7', label: 'Example source', count: 12, estimated_bytes: 42, latest_at: '2026-07-18T12:00:00Z' }],
          total_count: 1,
          cache_revision: 'cache-1',
          search_provenance: {}
        });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.replaceTransient({ groupingChain: ['source'] });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    await screen.findByText('Example source');
    await fireEvent.click(screen.getByRole('button', { name: 'Drill into Example source' }));
    await waitFor(() => expect(requests).toHaveLength(3));

    const entryRequest = requests.find((request) => new URL(request.url).pathname === '/api/v1/explore');
    const detailRequest = requests.find((request) => {
      const path = new URL(request.url).pathname;
      return path.endsWith('/groups') && request !== requests[0];
    });
    expect(entryRequest).toBeDefined();
    expect(detailRequest).toBeDefined();
    await expect(entryRequest!.clone().json()).resolves.toMatchObject({
      filters: [{ dimension: 'source', values: ['7'] }]
    });
    await expect(detailRequest!.clone().json()).resolves.toMatchObject({
      filters: [{ dimension: 'source', values: ['7'] }],
      grouping: ['source'],
      limit: 1
    });
    expect(state.current.groupingChain).toEqual([]);
    rendered.unmount();
    state.destroy();
  });


  it('aborts superseded and destroyed requests without destroying injected state', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const signals: AbortSignal[] = [];
    const fetchFn = vi.fn<typeof fetch>((input) => {
      const request = input instanceof Request ? input : new Request(input);
      signals.push(request.signal);
      return new Promise<Response>(() => undefined);
    });
    const state = new ExploreState(window);
    const destroy = vi.spyOn(state, 'destroy');
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    await waitFor(() => expect(signals).toHaveLength(1));

    state.commitSearch('next generation', 'full_text');
    await waitFor(() => expect(signals).toHaveLength(2));
    expect(signals[0]!.aborted).toBe(true);

    rendered.unmount();
    expect(signals[1]!.aborted).toBe(true);
    expect(destroy).not.toHaveBeenCalled();
    state.destroy();
  });


  it('invalidates a late-settling noncompliant request before destroy aborts it', async () => {
    const initialState = {
      schemaVersion: 1, workspace: 'everything', query: '', searchMode: 'full_text', filters: [],
      groupingChain: [], presentation: 'table', sort: [{ field: 'occurred_at', direction: 'desc' }],
      columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'], columnWidths: {},
      activeRow: 'message:1200', selectedRow: null, inspectorPinned: true,
      conversationAnchor: null, scrollAnchor: { key: 'message:1190', offset: 5 }
    };
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify(initialState))}`);
    let resolveRequest: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(() => new Promise<Response>((resolve) => { resolveRequest = resolve; }));
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    await waitFor(() => expect(fetchFn).toHaveBeenCalledOnce());

    rendered.unmount();
    resolveRequest?.(Response.json(exploreResponse({
      rows: [entry(1)], total_count: 1200, next_cursor: 'page:500'
    })));
    await new Promise((resolve) => setTimeout(resolve, 20));

    expect(fetchFn).toHaveBeenCalledOnce();
    state.destroy();
  });


  it('loads cursor pages from grid interaction and deduplicates stable server keys', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const body = await request.clone().json();
      return Response.json(
        body.cursor
          ? exploreResponse({ rows: [entry(2), entry(3)], total_count: 3 })
          : exploreResponse({ rows: [entry(1), entry(2)], total_count: 3, next_cursor: 'page-2' })
      );
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Everything results' }) as HTMLDivElement;
    await screen.findByText('Synthetic subject 2');

    Object.defineProperty(grid, 'scrollTop', { configurable: true, value: 36, writable: true });
    await fireEvent.scroll(grid);

    await screen.findByText('Synthetic subject 3');
    expect(requests).toHaveLength(2);
    await expect(requests[1]!.clone().json()).resolves.toMatchObject({ cursor: 'page-2' });
    expect(screen.getAllByText('Synthetic subject 2')).toHaveLength(1);
    rendered.unmount();
    state.destroy();
  });


  it('loads through cursor pages to restore a concrete selected row from a direct URL', async () => {
    const initialState = {
      schemaVersion: 1, workspace: 'everything', query: '', searchMode: 'full_text', filters: [],
      groupingChain: [], presentation: 'table', sort: [{ field: 'occurred_at', direction: 'desc' }],
      columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'], columnWidths: {},
      activeRow: null, selectedRow: 'message:1200', inspectorPinned: true,
      conversationAnchor: null, scrollAnchor: null
    };
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify(initialState))}`);
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const body = await request.clone().json();
      const offset = body.cursor ? Number(String(body.cursor).slice('page:'.length)) : 0;
      return Response.json(exploreResponse({
        rows: Array.from({ length: 500 }, (_, index) => entry(offset + index + 1)),
        total_count: 1500,
        ...(offset + 500 < 1500 ? { next_cursor: `page:${offset + 500}` } : {})
      }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect(await screen.findByRole('complementary', { name: 'Reading pane: Synthetic subject 1200' })).toBeDefined();
    expect(requests).toHaveLength(3);
    expect(state.current.selectedRow).toBe('message:1200');
    rendered.unmount();
    state.destroy();
  });


  it('restores every required durable identity while retaining page-one focus geometry', async () => {
    const initialState = {
      schemaVersion: 1, workspace: 'everything', query: '', searchMode: 'full_text', filters: [],
      groupingChain: [], presentation: 'table', sort: [{ field: 'occurred_at', direction: 'desc' }],
      columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'], columnWidths: {},
      activeRow: 'message:1', selectedRow: 'message:1200', inspectorPinned: true,
      conversationAnchor: null, scrollAnchor: { key: 'message:1', offset: 7 }
    };
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify(initialState))}`);
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const body = await request.clone().json();
      const offset = body.cursor ? Number(String(body.cursor).slice('page:'.length)) : 0;
      return Response.json(exploreResponse({
        rows: Array.from({ length: 500 }, (_, index) => entry(offset + index + 1)),
        total_count: 1500,
        ...(offset + 500 < 1500 ? { next_cursor: `page:${offset + 500}` } : {})
      }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect(await screen.findByRole('complementary', { name: 'Reading pane: Synthetic subject 1200' })).toBeDefined();
    const grid = screen.getByRole('grid', { name: 'Everything results' }) as HTMLDivElement;
    await waitFor(() => expect(grid.getAttribute('aria-activedescendant')).toBe('everything-row-message-3a-1'));
    await waitFor(() => expect(grid.scrollTop).toBe(7));
    expect(requests).toHaveLength(3);
    expect(state.current.activeRow).toBe('message:1');
    expect(state.current.scrollAnchor).toEqual({ key: 'message:1', offset: 7 });
    expect(state.current.selectedRow).toBe('message:1200');
    rendered.unmount();
    state.destroy();
  });


  it('exhausts finite pages once when a distinct selected row is missing', async () => {
    const initialState = {
      schemaVersion: 1, workspace: 'everything', query: '', searchMode: 'full_text', filters: [],
      groupingChain: [], presentation: 'table', sort: [{ field: 'occurred_at', direction: 'desc' }],
      columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'], columnWidths: {},
      activeRow: 'message:1', selectedRow: 'message:missing', inspectorPinned: true,
      conversationAnchor: null, scrollAnchor: { key: 'message:1', offset: 0 }
    };
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify(initialState))}`);
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const body = await request.clone().json();
      return Response.json(body.cursor
        ? exploreResponse({ rows: [entry(2)], total_count: 2 })
        : exploreResponse({ rows: [entry(1)], total_count: 2, next_cursor: 'page:1' }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect((await screen.findByRole('alert')).textContent).toContain('selected entry is no longer available');
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(requests).toHaveLength(2);
    expect(state.current.activeRow).toBe('message:1');
    expect(state.current.selectedRow).toBe('message:missing');
    rendered.unmount();
    state.destroy();
  });


  it('restores a later selected row with earlier focus geometry through Back and Forward', async () => {
    const initialState = {
      schemaVersion: 1, workspace: 'everything', query: '', searchMode: 'full_text', filters: [],
      groupingChain: [], presentation: 'table', sort: [{ field: 'occurred_at', direction: 'desc' }],
      columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'], columnWidths: {},
      activeRow: 'message:1', selectedRow: 'message:1200', inspectorPinned: true,
      conversationAnchor: null, scrollAnchor: { key: 'message:1', offset: 4 }
    };
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify(initialState))}`);
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const body = await request.clone().json();
      const offset = body.cursor ? Number(String(body.cursor).slice('page:'.length)) : 0;
      return Response.json(exploreResponse({
        rows: Array.from({ length: 500 }, (_, index) => entry(offset + index + 1)),
        total_count: 1500,
        ...(offset + 500 < 1500 ? { next_cursor: `page:${offset + 500}` } : {})
      }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    await screen.findByRole('complementary', { name: 'Reading pane: Synthetic subject 1200' });
    await fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    expect(screen.queryByRole('complementary', { name: 'Reading pane: Synthetic subject 1200' })).toBeNull();

    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    expect(await screen.findByRole('complementary', { name: 'Reading pane: Synthetic subject 1200' })).toBeDefined();
    const grid = screen.getByRole('grid', { name: 'Everything results' }) as HTMLDivElement;
    await waitFor(() => expect(requests).toHaveLength(6));
    await waitFor(() => expect(grid.getAttribute('aria-activedescendant')).toBe('everything-row-message-3a-1'));
    await waitFor(() => expect(grid.scrollTop).toBe(4));

    window.history.forward();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Settings' }).getAttribute('aria-current')).toBe('page'));
    expect(screen.queryByRole('complementary', { name: 'Reading pane: Synthetic subject 1200' })).toBeNull();
    rendered.unmount();
    state.destroy();
  });


  it('rehydrates an exact aggregate selection after the grouped rows are absent', async () => {
    const initialState = {
      schemaVersion: 1, workspace: 'everything', query: 'alpha', searchMode: 'full_text',
      filters: [{ dimension: 'domain', values: ['example.com'] }], groupingChain: [],
      presentation: 'table', sort: [{ field: 'occurred_at', direction: 'desc' }],
      columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'], columnWidths: {},
      activeRow: null, selectedRow: 'group:participant:42', inspectorPinned: true,
      conversationAnchor: null, scrollAnchor: null
    };
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify(initialState))}`);
    const groupRequests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname.endsWith('/groups')) {
        groupRequests.push(request);
        return Response.json({
          rows: [{ key: '42', label: 'Alice Example', count: 3, estimated_bytes: 512, latest_at: '2026-07-18T12:00:00Z' }],
          total_count: 1, cache_revision: 'cache-1', search_provenance: { lexical_index_revision: 'fts-1' }
        });
      }
      if (new URL(request.url).pathname.endsWith('/files')) {
        return Response.json({ files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} });
      }
      return Response.json(exploreResponse({ search_provenance: { lexical_index_revision: 'fts-1' } }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    const reading = await screen.findByRole('complementary', { name: 'Reading pane: Alice Example' });
    expect(within(reading).getByText(/3 items/)).toBeDefined();
    expect(groupRequests).toHaveLength(1);
    await expect(groupRequests[0]!.clone().json()).resolves.toEqual({
      filters: [
        { dimension: 'domain', values: ['example.com'] },
        { dimension: 'participant', values: ['42'] }
      ],
      query: 'alpha',
      search_mode: 'full_text',
      grouping: ['participant'],
      limit: 1,
      presentation: 'table'
    });
    rendered.unmount();
    state.destroy();
  });


  it('keeps missing concrete and aggregate selections visible as truthful inspector states', async () => {
    const state = new ExploreState(window);
    state.commitNavigation({ selectedRow: 'message:missing' });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname.endsWith('/groups')) return Response.json({
        rows: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {}
      });
      return Response.json(exploreResponse());
    });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect((await screen.findByRole('alert')).textContent).toContain('selected entry is no longer available');
    expect(state.current.selectedRow).toBe('message:missing');

    state.commitNavigation({ selectedRow: 'group:domain:missing.example' });
    await waitFor(() => {
      expect(screen.getByRole('alert').textContent).toContain('selected group is no longer available');
    });
    expect(state.current.selectedRow).toBe('group:domain:missing.example');
    rendered.unmount();
    state.destroy();
  });


  it('preserves cache-unavailable truth while restoring aggregate details', async () => {
    const state = new ExploreState(window);
    state.commitNavigation({ selectedRow: 'group:domain:example.com' });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname.endsWith('/groups')) return Response.json({
        error: 'cache_unavailable',
        message: 'The analytical cache is absent.',
        readiness: 'absent',
        recovery_action: 'msgvault build-cache'
      }, { status: 503 });
      return Response.json(exploreResponse());
    });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('The analytical cache is absent.');
    expect(alert.textContent).toContain('msgvault build-cache');
    expect(state.current.selectedRow).toBe('group:domain:example.com');
    rendered.unmount();
    state.destroy();
  });


  it('aborts superseded aggregate detail restoration and ignores a late stale response', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const pending: Array<{
      request: Request;
      resolve: (response: Response) => void;
    }> = [];
    const fetchFn = vi.fn<typeof fetch>((input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/groups')) {
        return new Promise<Response>((resolve) => pending.push({ request, resolve }));
      }
      if (path.endsWith('/files')) {
        return Promise.resolve(Response.json({
          files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {}
        }));
      }
      return Promise.resolve(Response.json(exploreResponse()));
    });
    const state = new ExploreState(window);
    state.commitNavigation({ selectedRow: 'group:domain:first.example' });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    await waitFor(() => expect(pending).toHaveLength(1));

    state.commitNavigation({ selectedRow: 'group:domain:second.example' });
    await waitFor(() => expect(pending).toHaveLength(2));
    expect(pending[0]!.request.signal.aborted).toBe(true);
    pending[1]!.resolve(Response.json({
      rows: [{ key: 'second.example', label: 'Second domain', count: 2 }],
      total_count: 1, cache_revision: 'cache-1', search_provenance: {}
    }));
    expect(await screen.findByRole('complementary', { name: 'Reading pane: Second domain' })).toBeDefined();

    pending[0]!.resolve(Response.json({
      rows: [{ key: 'first.example', label: 'Stale first domain', count: 9 }],
      total_count: 1, cache_revision: 'cache-1', search_provenance: {}
    }));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(screen.queryByText('Stale first domain')).toBeNull();
    expect(state.current.selectedRow).toBe('group:domain:second.example');
    rendered.unmount();
    state.destroy();
  });


  it('rejects a cursor page whose committed cache revision changed', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const body = await request.clone().json();
      return Response.json(
        body.cursor
          ? exploreResponse({ rows: [entry(2)], total_count: 2, cache_revision: 'cache-2' })
          : exploreResponse({ rows: [entry(1)], total_count: 2, next_cursor: 'page-2' })
      );
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Everything results' }) as HTMLDivElement;
    await screen.findByText('Synthetic subject 1');

    grid.focus();
    await fireEvent.keyDown(grid, { key: 'End' });

    expect((await screen.findByRole('alert')).textContent).toMatch(/results changed/i);
    expect(screen.queryByText('Synthetic subject 2')).toBeNull();
    expect(fetchFn).toHaveBeenCalledTimes(2);
    rendered.unmount();
    state.destroy();
  });


  it('stops End cursor draining after a network error without automatic retry', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const body = await request.clone().json();
      if (body.cursor) throw new Error('Network disconnected.');
      return Response.json(exploreResponse({ rows: [entry(1)], total_count: 2, next_cursor: 'page-2' }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Everything results' });
    await screen.findByText('Synthetic subject 1');

    grid.focus();
    await fireEvent.keyDown(grid, { key: 'End' });

    expect((await screen.findByRole('alert')).textContent).toContain('Network disconnected.');
    expect(fetchFn).toHaveBeenCalledTimes(2);
    rendered.unmount();
    state.destroy();
  });


  it('preserves a deep durable key when automatic restoration fails mid-page', async () => {
    const deepState = {
      schemaVersion: 1, workspace: 'everything', query: '', searchMode: 'full_text', filters: [],
      groupingChain: [], presentation: 'table', sort: [{ field: 'occurred_at', direction: 'desc' }],
      columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'], columnWidths: {},
      activeRow: 'message:5000', selectedRow: null, inspectorPinned: true,
      conversationAnchor: null, scrollAnchor: { key: 'message:4990', offset: 3 }
    };
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify(deepState))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const body = await request.clone().json();
      if (body.cursor) throw new Error('Restoration network failed.');
      return Response.json(exploreResponse({ rows: [entry(1)], total_count: 5000, next_cursor: 'page-2' }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect((await screen.findByRole('alert')).textContent).toContain('Restoration network failed.');
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(state.current.activeRow).toBe('message:5000');
    expect(state.current.scrollAnchor).toEqual({ key: 'message:4990', offset: 3 });
    rendered.unmount();
    state.destroy();
  });


  it('retries a failed deep restoration without replacing its durable focus', async () => {
    const deepState = {
      schemaVersion: 1, workspace: 'everything', query: '', searchMode: 'full_text', filters: [],
      groupingChain: [], presentation: 'table', sort: [{ field: 'occurred_at', direction: 'desc' }],
      columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'], columnWidths: {},
      activeRow: 'message:1200', selectedRow: null, inspectorPinned: true,
      conversationAnchor: null, scrollAnchor: { key: 'message:1190', offset: 3 }
    };
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify(deepState))}`);
    let failedOnce = false;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const body = await request.clone().json();
      const offset = body.cursor ? Number(String(body.cursor).slice('page:'.length)) : 0;
      if (offset === 500 && !failedOnce) {
        failedOnce = true;
        throw new Error('Restoration network failed.');
      }
      return Response.json(exploreResponse({
        rows: Array.from({ length: 500 }, (_, index) => entry(offset + index + 1)),
        total_count: 1500,
        ...(offset + 500 < 1500 ? { next_cursor: `page:${offset + 500}` } : {})
      }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect((await screen.findByRole('alert')).textContent).toContain('Restoration network failed.');
    expect(state.current.activeRow).toBe('message:1200');
    expect(state.current.scrollAnchor).toEqual({ key: 'message:1190', offset: 3 });
    await fireEvent.click(screen.getByRole('button', { name: 'Retry restoration' }));

    const grid = screen.getByRole('grid', { name: 'Everything results' }) as HTMLDivElement;
    await waitFor(() => expect(grid.getAttribute('aria-activedescendant')).toBe('everything-row-message-3a-1200'));
    await waitFor(() => expect(grid.scrollTop).toBe(1189 * 36 + 3));
    expect(state.current.activeRow).toBe('message:1200');
    expect(state.current.scrollAnchor).toEqual({ key: 'message:1190', offset: 3 });
    expect(fetchFn).toHaveBeenCalledTimes(5);
    rendered.unmount();
    state.destroy();
  });


  it('cancels an in-flight restoration when a newer committed navigation wins', async () => {
    const deepState = {
      schemaVersion: 1, workspace: 'everything', query: '', searchMode: 'full_text', filters: [],
      groupingChain: [], presentation: 'table', sort: [{ field: 'occurred_at', direction: 'desc' }],
      columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'], columnWidths: {},
      activeRow: 'message:1200', selectedRow: null, inspectorPinned: true,
      conversationAnchor: null, scrollAnchor: { key: 'message:1190', offset: 3 }
    };
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify(deepState))}`);
    let resolveSecond: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const body = await request.clone().json();
      if (body.cursor) return new Promise<Response>((resolve) => { resolveSecond = resolve; });
      return Response.json(exploreResponse({ rows: [entry(1)], total_count: 1200, next_cursor: 'page:500' }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(2));

    state.commitNavigation({ selectedRow: 'message:1' });
    expect(state.peekRestorationEpoch()).toBeUndefined();
    resolveSecond?.(Response.json(exploreResponse({ rows: [entry(2)], total_count: 1200 })));

    const grid = screen.getByRole('grid', { name: 'Everything results' });
    await waitFor(() => expect(grid.getAttribute('aria-activedescendant')).toBe('everything-row-message-3a-1'));
    expect(fetchFn).toHaveBeenCalledTimes(2);
    rendered.unmount();
    state.destroy();
  });


  it('stops End cursor draining at a page-level cache-unavailable response', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const body = await request.clone().json();
      if (body.cursor) return Response.json({
        error: 'cache_unavailable', message: 'Cache publication changed.', readiness: 'building',
        recovery_action: 'msgvault build-cache'
      }, { status: 503 });
      return Response.json(exploreResponse({ rows: [entry(1)], total_count: 2, next_cursor: 'page-2' }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Everything results' });
    await screen.findByText('Synthetic subject 1');

    grid.focus();
    await fireEvent.keyDown(grid, { key: 'End' });

    expect((await screen.findByRole('alert')).textContent).toContain('Cache publication changed.');
    expect(fetchFn).toHaveBeenCalledTimes(2);
    rendered.unmount();
    state.destroy();
  });


  it('stops End cursor draining after a repeated cursor without retrying forever', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const body = await request.clone().json();
      return Response.json(body.cursor
        ? exploreResponse({ rows: [entry(2)], total_count: 3, next_cursor: 'page-2' })
        : exploreResponse({ rows: [entry(1)], total_count: 3, next_cursor: 'page-2' }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Everything results' });
    await screen.findByText('Synthetic subject 1');

    grid.focus();
    await fireEvent.keyDown(grid, { key: 'End' });

    expect((await screen.findByRole('alert')).textContent).toMatch(/cursor|progress/i);
    expect(requests).toHaveLength(2);
    rendered.unmount();
    state.destroy();
  });


  it('stops End when an advancing cursor page adds no new stable keys', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const body = await request.clone().json();
      return Response.json(body.cursor
        ? exploreResponse({ rows: [entry(1)], total_count: 3, next_cursor: 'page-3' })
        : exploreResponse({ rows: [entry(1)], total_count: 3, next_cursor: 'page-2' }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Everything results' });
    await screen.findByText('Synthetic subject 1');

    grid.focus();
    await fireEvent.keyDown(grid, { key: 'End' });

    expect((await screen.findByRole('alert')).textContent).toMatch(/no row progress/i);
    expect(requests).toHaveLength(2);
    rendered.unmount();
    state.destroy();
  });


  it('caps End at LOAD_THROUGH_END_MAX_PAGES pages per press', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let explorePostCount = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path !== '/api/v1/explore') return Response.json(exploreResponse());
      explorePostCount += 1;
      const page = explorePostCount;
      return Response.json(exploreResponse({
        rows: [entry(page)], total_count: 10_000, next_cursor: `cursor-${page}`
      }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Everything results' });
    await screen.findByText('Synthetic subject 1');

    grid.focus();
    await fireEvent.keyDown(grid, { key: 'End' });

    await waitFor(() => expect(explorePostCount).toBe(1 + LOAD_THROUGH_END_MAX_PAGES));
    expect(await screen.findByText(/press End again to continue/)).toBeDefined();
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(explorePostCount).toBe(1 + LOAD_THROUGH_END_MAX_PAGES);
    rendered.unmount();
    state.destroy();
  });


  it('clears the End pause notice once a later press reaches the true end', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let explorePostCount = 0;
    const totalPages = 1 + LOAD_THROUGH_END_MAX_PAGES + 2;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path !== '/api/v1/explore') return Response.json(exploreResponse());
      explorePostCount += 1;
      const page = explorePostCount;
      return Response.json(exploreResponse({
        rows: [entry(page)], total_count: 10_000,
        ...(page < totalPages ? { next_cursor: `cursor-${page}` } : {})
      }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Everything results' });
    await screen.findByText('Synthetic subject 1');

    grid.focus();
    await fireEvent.keyDown(grid, { key: 'End' });
    await waitFor(() => expect(explorePostCount).toBe(1 + LOAD_THROUGH_END_MAX_PAGES));
    expect(await screen.findByText(/press End again to continue/)).toBeDefined();

    await fireEvent.keyDown(grid, { key: 'End' });
    await waitFor(() => expect(explorePostCount).toBe(totalPages));
    await waitFor(() => expect(
      screen.getByRole('status', { name: 'Sort status' }).textContent
    ).toBe('Newest first is the canonical Everything order.'));
    rendered.unmount();
    state.destroy();
  });


  it('does not restore a predicate-A key into local predicate B but Back restores A', async () => {
    const initialState = {
      schemaVersion: 1, workspace: 'everything', query: 'alpha', searchMode: 'full_text', filters: [],
      groupingChain: [], presentation: 'table', sort: [{ field: 'occurred_at', direction: 'desc' }],
      columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'], columnWidths: {},
      activeRow: 'message:1200', selectedRow: null, inspectorPinned: true,
      conversationAnchor: null, scrollAnchor: { key: 'message:1190', offset: 5 }
    };
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify(initialState))}`);
    const alphaRequests: Request[] = [];
    const betaRequests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const body = await request.clone().json();
      const target = body.query === 'beta' ? betaRequests : alphaRequests;
      target.push(request);
      const offset = body.cursor ? Number(String(body.cursor).slice('page:'.length)) : 0;
      const rows = Array.from({ length: 500 }, (_, index) => body.query === 'beta'
        ? { ...entry(offset + index + 1), key: `beta:${offset + index + 1}` }
        : entry(offset + index + 1));
      return Response.json(exploreResponse({
        rows,
        total_count: 1500,
        search_provenance: { lexical_index_revision: `fts-${body.query}` },
        ...(offset + 500 < 1500 ? { next_cursor: `page:${offset + 500}` } : {})
      }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    await waitFor(() => expect(alphaRequests).toHaveLength(3));
    expect(state.current.activeRow).toBe('message:1200');

    const search = screen.getByRole('searchbox', { name: 'Search everything' });
    await fireEvent.input(search, { target: { value: 'beta' } });
    await waitFor(() => expect(betaRequests.length).toBeGreaterThan(0));
    await new Promise((resolve) => setTimeout(resolve, 20));

    expect(betaRequests).toHaveLength(1);
    expect(state.current.activeRow).not.toBe('message:1200');
    expect(state.current.scrollAnchor).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: /^Search$/ }));

    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    await waitFor(() => expect(alphaRequests).toHaveLength(6));
    expect(state.current.activeRow).toBe('message:1200');
    expect(state.current.scrollAnchor).toEqual({ key: 'message:1190', offset: 5 });
    rendered.unmount();
    state.destroy();
  });


  it('joins an in-flight cursor request before End continues draining', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const requests: Request[] = [];
    let resolveSecond: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const body = await request.clone().json();
      if (!body.cursor) return Response.json(exploreResponse({ rows: [entry(1)], total_count: 3, next_cursor: 'page-2' }));
      if (body.cursor === 'page-2') return new Promise<Response>((resolve) => { resolveSecond = resolve; });
      return Response.json(exploreResponse({ rows: [entry(3)], total_count: 3 }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Everything results' }) as HTMLDivElement;
    await screen.findByText('Synthetic subject 1');
    await fireEvent.scroll(grid);
    await waitFor(() => expect(requests).toHaveLength(2));

    grid.focus();
    const end = fireEvent.keyDown(grid, { key: 'End' });
    resolveSecond?.(Response.json(exploreResponse({ rows: [entry(2)], total_count: 3, next_cursor: 'page-3' })));
    await end;

    await screen.findByText('Synthetic subject 3');
    expect(requests).toHaveLength(3);
    rendered.unmount();
    state.destroy();
  });


  it('accepts search provenance with canonical field order across cursor pages', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const body = await request.clone().json();
      return Response.json(body.cursor
        ? exploreResponse({ rows: [entry(2)], total_count: 2, search_provenance: { vector_generation: 4, lexical_index_revision: 'fts-1' } })
        : exploreResponse({ rows: [entry(1)], total_count: 2, next_cursor: 'page-2', search_provenance: { lexical_index_revision: 'fts-1', vector_generation: 4 } }));
    });
    const state = new ExploreState(window);
    state.replaceTransient({ query: 'alpha', searchMode: 'hybrid' });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Everything results' });
    await screen.findByText('Synthetic subject 1');
    await fireEvent.scroll(grid);

    await screen.findByText('Synthetic subject 2');
    expect(screen.queryByRole('alert')).toBeNull();
    rendered.unmount();
    state.destroy();
  });


  it('renders grouped cache failure as a named retry panel instead of empty groups', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let calls = 0;
    const fetchFn = vi.fn<typeof fetch>(async () => {
      calls += 1;
      if (calls === 1) return Response.json({
        error: 'cache_unavailable', message: 'Cache missing.', readiness: 'absent',
        recovery_action: 'msgvault build-cache'
      }, { status: 503 });
      return Response.json({ rows: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} });
    });
    const state = new ExploreState(window);
    state.replaceTransient({ groupingChain: ['source'] });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect((await screen.findByRole('alert')).textContent).toContain('Cache missing.');
    expect(screen.queryByText('No groups match this view.')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry cache check' }));
    await waitFor(() => expect(calls).toBe(2));
    rendered.unmount();
    state.destroy();
  });


  it('clears stale group rows while a changed grouping request is loading', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let resolveNext: ((response: Response) => void) | undefined;
    let calls = 0;
    const fetchFn = vi.fn<typeof fetch>(async () => {
      calls += 1;
      if (calls === 1) return Response.json({
        rows: [{ key: '7', label: 'Old source group', count: 2, estimated_bytes: 42, latest_at: '2026-07-18T12:00:00Z' }],
        total_count: 1, cache_revision: 'cache-1', search_provenance: {}
      });
      return new Promise<Response>((resolve) => { resolveNext = resolve; });
    });
    const state = new ExploreState(window);
    state.replaceTransient({ groupingChain: ['source'] });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    await screen.findByText('Old source group');

    state.commitNavigation({ groupingChain: ['month'] });
    await waitFor(() => expect(calls).toBe(2));

    expect(screen.queryByText('Old source group')).toBeNull();
    expect(screen.getByText('Loading grouped results…')).toBeDefined();
    resolveNext?.(Response.json({ rows: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} }));
    rendered.unmount();
    state.destroy();
  });


  it('restores the durable grouped active key and scroll anchor after drill Back', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const groupRows = Array.from({ length: 30 }, (_, index) => ({
      key: String(index + 1), label: `Source ${index + 1}`, count: index + 1,
      estimated_bytes: 42, latest_at: '2026-07-18T12:00:00Z'
    }));
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      return new URL(request.url).pathname.endsWith('/groups')
        ? Response.json({ rows: groupRows, total_count: 30, cache_revision: 'cache-1', search_provenance: {} })
        : Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.commitNavigation({ groupingChain: ['source'] });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Everything grouped by source' }) as HTMLDivElement;
    await screen.findByText('Source 2');
    Object.defineProperty(grid, 'scrollTop', { configurable: true, value: 36, writable: true });
    await fireEvent.scroll(grid);
    await waitFor(() => expect(state.current).toMatchObject({
      activeRow: 'group:source:2', scrollAnchor: { key: 'group:source:2', offset: 0 }
    }));
    await fireEvent.click(screen.getByRole('button', { name: 'Drill into Source 2' }));

    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    const restored = await screen.findByRole('grid', { name: 'Everything grouped by source' });
    await waitFor(() => expect(restored.getAttribute('aria-activedescendant')).toBe('everything-group-3Asource-3A2'));
    expect(state.current.activeRow).toBe('group:source:2');
    expect(state.current.scrollAnchor).toEqual({ key: 'group:source:2', offset: 0 });
    rendered.unmount();
    state.destroy();
  });


  it('restores an inspected drilled group across Back and Forward after grouped rows are cleared', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (!path.endsWith('/groups')) return Response.json(exploreResponse());
      const body = await request.clone().json() as { filters?: Array<{ dimension: string; values: string[] }> };
      const restoringDetail = body.filters?.some((filter) =>
        filter.dimension === 'source' && filter.values.includes('7'));
      return Response.json({
        rows: [{
          key: '7',
          label: restoringDetail ? 'Restored source detail' : 'Example source group',
          count: 12,
          estimated_bytes: 42,
          latest_at: '2026-07-18T12:00:00Z'
        }],
        total_count: 1,
        cache_revision: 'cache-1',
        search_provenance: {}
      });
    });
    const state = new ExploreState(window);
    state.commitNavigation({ groupingChain: ['source'] });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    await screen.findByText('Example source group');

    await fireEvent.click(screen.getByRole('button', { name: 'Drill into Example source group' }));
    expect(await screen.findByRole('complementary', { name: 'Reading pane: Restored source detail' })).toBeDefined();
    expect(screen.queryByRole('grid', { name: 'Everything grouped by source' })).toBeNull();
    expect(state.current.selectedRow).toBe('group:source:7');

    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    expect(await screen.findByRole('grid', { name: 'Everything grouped by source' })).toBeDefined();
    expect(state.current.selectedRow).toBeNull();

    window.history.forward();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    expect(await screen.findByRole('complementary', { name: 'Reading pane: Restored source detail' })).toBeDefined();
    expect(state.current.selectedRow).toBe('group:source:7');
    rendered.unmount();
    state.destroy();
  });


  it('reapplies a retained group anchor across same-count result generations', async () => {
    const initialState = {
      schemaVersion: 1, workspace: 'everything', query: '', searchMode: 'full_text', filters: [],
      groupingChain: ['source'], presentation: 'table', sort: [{ field: 'occurred_at', direction: 'desc' }],
      columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'], columnWidths: {},
      activeRow: 'group:source:20', selectedRow: null, inspectorPinned: true,
      conversationAnchor: null, scrollAnchor: { key: 'group:source:15', offset: 4 }
    };
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify(initialState))}`);
    let generation = 0;
    const fetchFn = vi.fn<typeof fetch>(async () => {
      generation += 1;
      return Response.json({
        rows: Array.from({ length: 30 }, (_, index) => ({
          key: String(index + 1), label: `Generation ${generation} Source ${index + 1}`,
          count: index + 1, estimated_bytes: 42, latest_at: '2026-07-18T12:00:00Z'
        })),
        total_count: 30, cache_revision: 'cache-1', search_provenance: {}
      });
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Everything grouped by source' }) as HTMLDivElement;
    await screen.findByText('Generation 1 Source 20');
    await waitFor(() => expect(grid.scrollTop).toBe(14 * 36 + 4));
    expect(grid.getAttribute('aria-activedescendant')).toBe('everything-group-3Asource-3A20');

    grid.scrollTop = 0;
    state.replaceTransient({ filters: [{ dimension: 'source', values: ['20'] }] });
    await screen.findByText('Generation 2 Source 20');

    await waitFor(() => expect(grid.scrollTop).toBe(14 * 36 + 4));
    expect(grid.getAttribute('aria-activedescendant')).toBe('everything-group-3Asource-3A20');
    expect(state.current.activeRow).toBe('group:source:20');
    expect(state.current.scrollAnchor).toEqual({ key: 'group:source:15', offset: 4 });
    rendered.unmount();
    state.destroy();
  });


  it('retries grouped deep restoration with exact focus and scroll state', async () => {
    const initialState = {
      schemaVersion: 1, workspace: 'everything', query: '', searchMode: 'full_text', filters: [],
      groupingChain: ['source'], presentation: 'table', sort: [{ field: 'occurred_at', direction: 'desc' }],
      columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'], columnWidths: {},
      activeRow: 'group:source:1200', selectedRow: null, inspectorPinned: true,
      conversationAnchor: null, scrollAnchor: { key: 'group:source:1190', offset: 3 }
    };
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify(initialState))}`);
    let failedOnce = false;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const body = await request.clone().json();
      const offset = body.cursor ? Number(String(body.cursor).slice('page:'.length)) : 0;
      if (offset === 500 && !failedOnce) {
        failedOnce = true;
        throw new Error('Grouped restoration interrupted.');
      }
      return Response.json({
        rows: Array.from({ length: 500 }, (_, index) => ({
          key: String(offset + index + 1), label: `Source ${offset + index + 1}`,
          count: offset + index + 1, estimated_bytes: 42, latest_at: '2026-07-18T12:00:00Z'
        })),
        total_count: 1500,
        cache_revision: 'cache-1',
        search_provenance: {},
        ...(offset + 500 < 1500 ? { next_cursor: `page:${offset + 500}` } : {})
      });
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect((await screen.findByRole('alert')).textContent).toContain('Grouped restoration interrupted.');
    expect(state.current.activeRow).toBe('group:source:1200');
    expect(state.current.scrollAnchor).toEqual({ key: 'group:source:1190', offset: 3 });
    await fireEvent.click(screen.getByRole('button', { name: 'Retry restoration' }));

    const grid = screen.getByRole('grid', { name: 'Everything grouped by source' }) as HTMLDivElement;
    await waitFor(() => expect(grid.getAttribute('aria-activedescendant')).toBe('everything-group-3Asource-3A1200'));
    await waitFor(() => expect(grid.scrollTop).toBe(1189 * 36 + 3));
    expect(state.current.activeRow).toBe('group:source:1200');
    expect(state.current.scrollAnchor).toEqual({ key: 'group:source:1190', offset: 3 });
    expect(fetchFn).toHaveBeenCalledTimes(5);
    rendered.unmount();
    state.destroy();
  });

  it.each([
    ['participant', '42', [{ dimension: 'participant', values: ['42'] }]],
    ['domain', 'example.com', [{ dimension: 'domain', values: ['example.com'] }]],
    ['message_type', 'email', [{ dimension: 'message_type', values: ['email'] }]],
    ['year', '2026', [
      { dimension: 'after', values: ['2026-01-01T00:00:00Z'] },
      { dimension: 'before', values: ['2027-01-01T00:00:00Z'] }
    ]],
    ['month', '2026-07', [
      { dimension: 'after', values: ['2026-07-01T00:00:00Z'] },
      { dimension: 'before', values: ['2026-08-01T00:00:00Z'] }
    ]],
    ['month', '2026-12', [
      { dimension: 'after', values: ['2026-12-01T00:00:00Z'] },
      { dimension: 'before', values: ['2027-01-01T00:00:00Z'] }
    ]]
  ] as const)('drills %s groups into a changed URL predicate', async (dimension, key, expectedFilters) => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname.endsWith('/groups')) return Response.json({
        rows: [{ key, label: key, count: 2, estimated_bytes: 42, latest_at: '2026-07-18T12:00:00Z' }],
        total_count: 1, cache_revision: 'cache-1', search_provenance: {}
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.replaceTransient({
      groupingChain: [dimension],
      activeRow: `group:${dimension}:${key}`,
      scrollAnchor: { key: `group:${dimension}:${key}`, offset: 4 }
    });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    await fireEvent.click(await screen.findByRole('button', { name: `Drill into ${key}` }));

    expect(state.current.filters).toEqual(expectedFilters);
    expect(parseExploreURLState(window.location.search).filters).toEqual(expectedFilters);
    expect(state.current.groupingChain).toEqual([]);
    expect(state.current.scrollAnchor).toBeNull();
    rendered.unmount();
    state.destroy();
  });


  it('removes all-matching promotion while a newer predicate generation is loading', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let resolveNext: ((response: Response) => void) | undefined;
    let calls = 0;
    const fetchFn = vi.fn<typeof fetch>(async () => {
      calls += 1;
      if (calls === 1) {
        return Response.json(exploreResponse({ rows: [entry(1), entry(2)], total_count: 2 }));
      }
      return new Promise<Response>((resolve) => { resolveNext = resolve; });
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Everything results' });
    await screen.findByText('Synthetic subject 2');
    grid.focus();
    await fireEvent.keyDown(grid, { key: 'A' });
    expect(screen.getByRole('button', { name: 'Select all 2 matching items' })).toBeDefined();

    state.commitSearch('new predicate', 'full_text');
    await waitFor(() => expect(calls).toBe(2));
    expect(screen.queryByRole('button', { name: 'Select all 2 matching items' })).toBeNull();

    resolveNext?.(Response.json(exploreResponse({
      rows: [entry(3)],
      total_count: 1,
      search_provenance: { lexical_index_revision: 'fts-2' }
    })));
    rendered.unmount();
    state.destroy();
  });


  it('opens, resizes, and closes the bottom reading pane with size persistence and focus restoration', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    window.localStorage.removeItem('msgvault.reading-pane.size');
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(exploreResponse({
      rows: [entry(1), entry(2)], total_count: 2
    })));
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Everything results' });
    await screen.findByText('Synthetic subject 1');
    grid.focus();

    await fireEvent.keyDown(grid, { key: 'Enter' });
    expect(await screen.findByRole('complementary', { name: 'Reading pane: Synthetic subject 1' })).toBeDefined();
    expect(state.current.selectedRow).toBe('message:1');
    // A bottom split, never a modal drawer over the results.
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.querySelector('.kit-detail-drawer-overlay')).toBeNull();

    // Keyboard resize on the split handle persists the size locally (not in the URL).
    await fireEvent.keyDown(screen.getByRole('button', { name: 'Resize reading pane' }), { key: 'ArrowUp' });
    const persisted = window.localStorage.getItem('msgvault.reading-pane.size');
    expect(persisted).not.toBeNull();
    expect(parseExploreURLState(window.location.search)).not.toHaveProperty('inspectorWidth');
    await fireEvent.click(screen.getByRole('button', { name: 'Close reading pane' }));

    await waitFor(() => expect(document.activeElement).toBe(grid));
    expect(state.current.selectedRow).toBeNull();
    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    expect(await screen.findByRole('complementary', { name: 'Reading pane: Synthetic subject 1' })).toBeDefined();
    expect(window.localStorage.getItem('msgvault.reading-pane.size')).toBe(persisted);
    window.localStorage.removeItem('msgvault.reading-pane.size');
    rendered.unmount();
    state.destroy();
  });


  it('carries the in-thread anchor in the URL by replacement so Back/Forward restore the same message', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const row = { ...entry(1), anchor_message_id: 1, conversation_id: 7 };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.startsWith('/api/v1/conversations/')) {
        const anchor = Number(new URL(request.url).searchParams.get('anchor'));
        return Response.json({
        id: 7,
        anchor_id: anchor,
        messages: [1, 2].map((id) => ({
          id, conversation_id: 7, subject: `${row.title} ${id}`, message_type: 'email',
          from: 'alice@example.com', to: ['bob@example.com'], sent_at: row.occurred_at,
          snippet: row.preview, labels: [], has_attachments: false, size_bytes: 10,
          body: `Archived body ${id}`, body_html: `<p>Archived body ${id}</p>`, attachments: []
        })),
        has_before: false, has_after: false, total: 2
      });
      }
      return Response.json(exploreResponse({ rows: [row], total_count: 1 }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Everything results' });
    await screen.findByText(row.title);
    grid.focus();
    const priorSearch = window.location.search;
    await fireEvent.keyDown(grid, { key: 'Enter' });

    // The thread renders directly in the reading pane: the anchor message is
    // expanded, its sibling collapsed.
    const reading = await screen.findByRole('complementary', { name: `Reading pane: ${row.title}` });
    expect(await within(reading).findByRole('button', { name: 'Collapse message 1 from alice@example.com' })).toBeDefined();

    // Expanding another message moves the in-thread anchor. The anchor is
    // committed by replacement onto the pane's own history entry, so a
    // single Back closes the pane and restores the pre-open URL exactly.
    await fireEvent.click(within(reading).getByRole('button', { name: 'Expand message 2 from alice@example.com' }));
    await waitFor(() => expect(state.current.conversationAnchor).toBe('2'));
    expect(within(reading).getByRole('button', { name: 'Collapse message 2 from alice@example.com' })).toBeDefined();

    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    await waitFor(() => expect(state.current.selectedRow).toBeNull());
    expect(state.current.conversationAnchor).toBeNull();
    expect(window.location.search).toBe(priorSearch);
    await waitFor(() => expect(document.activeElement).toBe(grid));

    // Forward restores the pane at the exact in-thread message it left off.
    window.history.forward();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    await waitFor(() => expect(state.current.conversationAnchor).toBe('2'));
    const restored = await screen.findByRole('complementary', { name: `Reading pane: ${row.title}` });
    expect(await within(restored).findByRole('button', { name: 'Collapse message 2 from alice@example.com' })).toBeDefined();

    await fireEvent.keyDown(restored, { key: 'Escape' });
    await waitFor(() => expect(state.current.selectedRow).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(grid));
    rendered.unmount();
    state.destroy();
  });


  it('suspends application shortcuts inside editable reading-pane content and keeps reader navigation live outside it', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(exploreResponse({
      rows: [entry(1), entry(2)], total_count: 2
    })));
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Everything results' });
    await screen.findByText('Synthetic subject 2');
    grid.focus();
    await fireEvent.keyDown(grid, { key: 'Enter' });
    const reading = await screen.findByRole('complementary', { name: 'Reading pane: Synthetic subject 1' });

    // Editable content inside the pane suspends application shortcuts.
    const editor = document.createElement('textarea');
    reading.append(editor);
    editor.focus();
    await waitFor(() => expect(appShortcuts.activeScope()).toBe('everything-editable'));
    const modified = new KeyboardEvent('keydown', {
      key: 'k', ctrlKey: true, bubbles: true, cancelable: true
    });
    editor.dispatchEvent(modified);
    expect(modified.defaultPrevented).toBe(false);
    editor.remove();
    await waitFor(() => expect(appShortcuts.activeScope()).toBe('root'));

    // Outside editable content, reader navigation stays live: 'l' advances
    // the open item without the grid holding focus.
    await fireEvent.keyDown(reading, { key: 'l' });
    await waitFor(() => expect(state.current.selectedRow).toBe('message:2'));
    expect(screen.getByRole('complementary', { name: 'Reading pane: Synthetic subject 2' })).toBeDefined();

    await fireEvent.keyDown(reading, { key: 'Escape' });
    await waitFor(() => expect(state.current.selectedRow).toBeNull());
    expect(appShortcuts.activeScope()).toBe('root');
    rendered.unmount();
    state.destroy();
  });

  it('advances the main load, inspector detail, and coverage effects exactly once each for a single filter commit', async () => {
    const initialState = {
      schemaVersion: 1, workspace: 'everything', query: '', searchMode: 'hybrid', filters: [],
      groupingChain: [], presentation: 'table', sort: [{ field: 'occurred_at', direction: 'desc' }],
      columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'], columnWidths: {},
      activeRow: null, selectedRow: 'group:source:7', inspectorPinned: true,
      conversationAnchor: null, scrollAnchor: null
    };
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify(initialState))}`);
    const counts = { explore: 0, groups: 0, coverage: 0 };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/explore/groups')) {
        counts.groups += 1;
        return Response.json({
          rows: [{ key: '7', label: 'Example source', count: 12, estimated_bytes: 42, latest_at: '2026-07-18T12:00:00Z' }],
          total_count: 1, cache_revision: 'cache-1', search_provenance: {}
        });
      }
      if (path.endsWith('/search/coverage')) {
        counts.coverage += 1;
        return Response.json({
          status: 'ready', eligible_count: 10, embedded_count: 10, percentage: 100,
          cache_revision: 'cache-1', actions: []
        });
      }
      counts.explore += 1;
      return Response.json(exploreResponse({ rows: [entry(1)], total_count: 1 }));
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    await screen.findByText('Synthetic subject 1');
    await screen.findByText('Example source');
    await waitFor(() => expect(counts).toEqual({ explore: 1, groups: 1, coverage: 1 }));

    state.commitNavigation({ filters: [{ dimension: 'source', values: ['99'] }] });
    await waitFor(() => expect(counts.explore).toBe(2));
    await waitFor(() => expect(counts).toEqual({ explore: 2, groups: 2, coverage: 2 }));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(counts).toEqual({ explore: 2, groups: 2, coverage: 2 });

    rendered.unmount();
    state.destroy();
  });
});
