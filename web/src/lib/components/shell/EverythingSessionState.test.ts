import { render, screen, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { ExploreState } from '../../explore/state.svelte';
import AppShell from './AppShell.svelte';

vi.mock('../../telemetry/screen-views', () => ({ startScreenViewReporting: () => () => {} }));

describe('EverythingSessionState', () => {
  it('hides unconfigured recording search and keeps the ordinary results layout', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything', query: 'quarterly', searchMode: 'full_text' }))}`);
    const fetchFn = vi.fn<typeof fetch>(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path.endsWith('/media/search/status')) return Response.json({ configured: false });
      return Response.json({ rows: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} });
    });
    const state = new ExploreState(window);
    const view = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      await waitFor(() => expect(fetchFn.mock.calls.filter(([input]) => new URL((input as Request).url).pathname.endsWith('/media/search/status'))).toHaveLength(1));
      await waitFor(() => expect(screen.queryByRole('region', { name: 'Recordings' })).toBeNull());
      await screen.findByText('0 items');
      expect(screen.queryByRole('heading', { name: /^Results/ })).toBeNull();
      expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
      expect(document.querySelector('.results-primary--recordings')).toBeNull();
      vi.useFakeTimers();
      state.commitSearch('different', 'full_text');
      await tick();
      await vi.advanceTimersByTimeAsync(300);
      expect(fetchFn.mock.calls.filter(([input]) => new URL((input as Request).url).pathname.endsWith('/media/search'))).toHaveLength(0);
      state.commitNavigation({ workspace: 'files' });
      await tick();
      state.commitNavigation({ workspace: 'everything' });
      await tick();
      await vi.advanceTimersByTimeAsync(300);
      expect(fetchFn.mock.calls.filter(([input]) => new URL((input as Request).url).pathname.endsWith('/media/search'))).toHaveLength(0);
      expect(fetchFn.mock.calls.filter(([input]) => new URL((input as Request).url).pathname.endsWith('/media/search/status'))).toHaveLength(1);
      expect(screen.queryByRole('region', { name: 'Recordings' })).toBeNull();
      expect(screen.queryByRole('heading', { name: /^Results/ })).toBeNull();
      expect(document.querySelector('.results-primary--recordings')).toBeNull();
    } finally {
      view.unmount();
      state.destroy();
      vi.useRealTimers();
    }
  });

});
