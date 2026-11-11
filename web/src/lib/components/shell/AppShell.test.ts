import { directoryPerson, person, mergeResult } from '../../directory/entity-fixtures.test-support';
import { emptyProfile } from '../../directory/profile-fixtures.test-support';
import { lanes, visualStatus } from '../../../test/operations';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { appShortcuts } from '@kenn-io/kit-ui';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createRawSnippet, getContext, mount, tick, unmount, type ComponentProps } from 'svelte';

import { meetingFixtureResponse } from '../../meetings/fixtures.test-support';
import { createCaptureClient } from '../../telemetry/capture.test-support';
import { LOAD_THROUGH_END_MAX_PAGES } from '../../explore/paging';
import { sourceGroup } from '../../explore/fixtures.test-support';
import { ExploreState, serializeExploreURLState } from '../../explore/state.svelte';
import { chooseSelectOption, openTypeahead } from '../../../test/kit-ui';
import { startTelemetryReporting, stopTelemetryReporting } from '../../telemetry/actions';
import AppShell from './AppShell.svelte';
import { ShellData } from './shell-data.svelte';
import SettingsWorkspace from '../settings/SettingsWorkspace.svelte';
import type { CardDAVSettingsRequest } from '../../carddav/navigation';
import { IntegrationReadiness } from '../../kata/kata-ready.svelte';
import { mediaReadinessKey } from '../../search/media-ready.svelte';
import { visualReadinessKey } from '../../search/visual-ready.svelte';
import { SIDEBAR_COLLAPSED_KEY } from './navigation';
import { pair, view } from './fixtures.test-support';
import canonical from '../../../../tests/e2e/fixtures/operations-public.json';
import { jobRun, jobLane, sourceStatus } from '../../jobs/fixtures.test-support';

const telemetryEvents: Array<{ event: string; properties: { search_type: string } }> = [];
beforeEach(() => { telemetryEvents.length = 0; });
const createAPIClient = (fetchFn: typeof fetch) => createCaptureClient(async (input, init) => {
  const response = await fetchFn(input, init);
  const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
  const field = path === '/api/v1/operations/status' ? 'lanes' : path === '/api/v1/sources/status' ? 'sources' : undefined;
  // Generic archive handlers don't supply these independent shell endpoints.
  if (field && response.ok && !(field in await response.clone().json())) return Response.json({ [field]: [] });
  return response;
}, telemetryEvents);

function exploreResponse(overrides: Record<string, unknown> = {}) {
  return {
    rows: [],
    total_count: 0,
    cache_revision: 'cache-1',
    search_provenance: {},
    ...overrides
  };
}

const sourceRun = canonical.runs.find(run => run.kind === 'source_sync')!;
const OPERATION_RUN = sourceRun.id;

function operationAuthorityResponse(path: string): Response | undefined {
  if (path.endsWith('/multimodal/status')) return Response.json(visualStatus);
  return undefined;
}

describe('AppShell', () => {
  it('opens sources from an archive with no sources', async () => {
    window.history.replaceState(null, '', '/?workspace=everything');
    const state = new ExploreState(window);
    const client = createAPIClient(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path.endsWith('/carddav/connections')) return Response.json({ connections: [] });
      if (path.endsWith('/sources/status')) return Response.json({ sources: [] });
      return Response.json(exploreResponse());
    });
    const rendered = render(AppShell, { client, state });
    try {
      await fireEvent.click(await screen.findByRole('button', { name: 'Add source…' }));
      expect(state.current.workspace).toBe('sources');
    } finally {
      rendered.unmount();
      state.destroy();
    }
  });

  it('keeps source setup hidden when the archive has a source', async () => {
    const start = vi.spyOn(ShellData.prototype, 'start');
    window.history.replaceState(null, '', '/?workspace=everything');
    const state = new ExploreState(window);
    const client = createAPIClient(async input => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path === '/api/v1/sources/status') return Response.json({ sources: [sourceStatus()] });
      return Response.json(exploreResponse());
    });
    const rendered = render(AppShell, { client, state });
    try {
      await waitFor(() => expect((start.mock.contexts[0] as ShellData | undefined)?.jobs.loaded).toBe(true));
      expect(screen.queryByText('No mail yet')).toBeNull();
      expect(screen.queryByRole('button', { name: 'Add source…' })).toBeNull();
    } finally {
      start.mockRestore();
      rendered.unmount();
      state.destroy();
    }
  });

  it('keeps confirmation focus through held Back and its restoring traversal', async () => {
    window.history.replaceState({ exploreIndex: 0 }, '', '/?workspace=files');
    window.history.pushState({ exploreIndex: 1 }, '', '/?workspace=settings');
    const state = new ExploreState(window);
    const settings = createRawSnippet(() => ({
      render: () => '<main aria-label="Settings fixture"><button>Stay</button></main>'
    }));
    const rendered = render(AppShell, {
      client: createAPIClient(vi.fn()), state, enabled: false, settings
    });
    const stay = await screen.findByRole('button', { name: 'Stay' });
    state.holdLeave = () => { stay.focus(); return true; };
    const focused: Array<Element | null> = [];
    let finish!: () => void;
    const traversed = new Promise<void>(resolve => { finish = resolve; });
    const recordFocus = async () => {
      await tick();
      focused.push(document.activeElement);
      if (focused.length === 2) finish();
    };
    window.addEventListener('popstate', recordFocus);
    try {
      window.history.back();
      await traversed;
      expect(state.current.workspace).toBe('settings');
      expect(window.location.search).toBe('?workspace=settings');
      expect(focused).toEqual([stay, stay]);
    } finally {
      window.removeEventListener('popstate', recordFocus);
      rendered.unmount();
      state.destroy();
    }
  });

  it('rechecks inconclusive visual readiness on focus without remounting Files', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    window.history.replaceState(null, '', '/?workspace=files');
    const state = new ExploreState(window);
    const probe = vi.fn<() => Promise<boolean | undefined>>()
      .mockResolvedValueOnce(undefined)
      .mockResolvedValue(true);
    const visual = new IntegrationReadiness(probe);
    const client = createAPIClient(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path.endsWith('/files/search')) return Response.json({ files: [], total_count: 0, cache_revision: 'files-1', search_provenance: {} });
      return Response.json({});
    });
    const rendered = render(AppShell, { props: { client, state }, context: new Map([[visualReadinessKey, visual]]) });
    try {
      const grid = await screen.findByRole('grid', { name: 'Files results' });
      await waitFor(() => expect(probe).toHaveBeenCalledTimes(1));
      expect(screen.queryByRole('switch', { name: 'Visual search' })).toBeNull();

      await fireEvent(window, new Event('focus'));

      expect(await screen.findByRole('switch', { name: 'Visual search' })).toBeDefined();
      expect(probe).toHaveBeenCalledTimes(2);
      expect(screen.getByRole('grid', { name: 'Files results' })).toBe(grid);
    } finally {
      rendered.unmount();
      visual.dispose();
      state.destroy();
      vi.useRealTimers();
    }
  });

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


  it('searches the archive from People when a palette query matches no command', async () => {
    window.history.replaceState(null, '', '/?workspace=relationships');
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });
    await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
    const palette = await screen.findByRole('dialog', { name: 'Messages commands' });
    const input = within(palette).getByRole('combobox');
    await fireEvent.input(input, { target: { value: 'zzzxxyy' } });
    await fireEvent.keyDown(input, { key: 'Enter' });
    expect(state.current.workspace).toBe('everything');
    expect(state.current.query).toBe('zzzxxyy');
    rendered.unmount(); state.destroy();
  });

  it.each([
    { workspace: 'everything' as const, relationshipFacet: 'domains' as const, peopleDuplicates: false },
    { workspace: 'relationships' as const, relationshipFacet: 'people' as const, peopleDuplicates: true },
  ])('finds people using the palette query from $relationshipFacet with duplicates=$peopleDuplicates', async startingPoint => {
    window.history.replaceState(null, '', '/?workspace=relationships');
    const state = new ExploreState(window);
    state.commitNavigation({ ...startingPoint, relationshipTarget: 'cluster:42', directoryPersonID: 42, relationshipFiles: true });
    const rendered = render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });
    await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
    const palette = await screen.findByRole('dialog', { name: 'Messages commands' });
    await fireEvent.input(within(palette).getByRole('combobox'), { target: { value: 'Example' } });
    await fireEvent.click(within(palette).getByRole('option', { name: 'Find person…' }));
    expect(state.current.workspace).toBe('relationships');
    expect(state.current.identityQuery).toBe('Example');
    expect(state.current.relationshipFacet).toBe('people');
    expect(state.current.peopleDuplicates).toBe(false);
    expect(state.current.relationshipTarget).toBeNull();
    expect(state.current.directoryPersonID).toBeNull();
    expect(state.current.relationshipFiles).toBe(false);
    rendered.unmount(); state.destroy();
  });

  it('retries the same failed archive search from the palette', async () => {
    window.history.replaceState(null, '', '/?workspace=everything');
    const state = new ExploreState(window);
    let searchRequests = 0;
    const client = createAPIClient(async input => {
      const url = new URL((input as Request).url);
      if (url.pathname.endsWith('/explore') && (await (input as Request).json()).query === 'zzzxxyy') {
        searchRequests += 1;
        return searchRequests === 1
          ? Response.json({ message: 'Search temporarily failed' }, { status: 500 })
          : Response.json(exploreResponse({ rows: [entry(1)], total_count: 1 }));
      }
      return Response.json(exploreResponse());
    });
    const rendered = render(AppShell, { client, state });
    try {
      await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
      let palette = await screen.findByRole('dialog', { name: 'Messages commands' });
      await fireEvent.input(within(palette).getByRole('combobox'), { target: { value: 'zzzxxyy' } });
      await fireEvent.click(within(palette).getByRole('option', { name: /Search archive/ }));
      await screen.findByText('Search temporarily failed');
      await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
      palette = await screen.findByRole('dialog', { name: 'Messages commands' });
      await fireEvent.input(within(palette).getByRole('combobox'), { target: { value: 'zzzxxyy' } });
      await fireEvent.click(within(palette).getByRole('option', { name: /Search archive/ }));
      await screen.findByText('Synthetic subject 1');
      expect(searchRequests).toBe(2);
    } finally { rendered.unmount(); state.destroy(); }
  });

  it('keeps a recent saved view across reloads while views load late and another command runs', async () => {
    window.history.replaceState(null, '', '/?workspace=everything');
    const state = new ExploreState(window);
    let delayViews = false;
    let finishViews!: (response: Response) => void;
    const pendingViews = new Promise<Response>(resolve => { finishViews = resolve; });
    const client = createAPIClient(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path.endsWith('/saved-views')) return delayViews ? pendingViews : Response.json({ saved_views: [view(3, 'Project notes')] });
      return Response.json(exploreResponse());
    });
    let rendered = render(AppShell, { client, state });
    try {
      await screen.findByRole('button', { name: 'Project notes actions' });
      await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
      let palette = await screen.findByRole('dialog', { name: 'Messages commands' });
      await fireEvent.click(within(palette).getByRole('option', { name: 'Project notes' }));
      rendered.unmount();
      delayViews = true;
      rendered = render(AppShell, { client, state });
      await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
      palette = await screen.findByRole('dialog', { name: 'Messages commands' });
      await fireEvent.click(within(palette).getByRole('option', { name: 'People' }));
      finishViews(Response.json({ saved_views: [view(3, 'Project notes')] }));
      await screen.findByRole('button', { name: 'Project notes actions' });
      await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
      palette = await screen.findByRole('dialog', { name: 'Messages commands' });
      const options = within(palette).getAllByRole('option');
      expect(options[0].textContent?.trim()).toBe('People');
      expect(options[1].textContent?.trim()).toBe('Project notes');
      expect(JSON.parse(localStorage.getItem('msgvault.palette.recent')!)).toEqual(['go:relationships', 'view:3']);
    } finally {
      finishViews(Response.json({ saved_views: [] }));
      rendered.unmount(); state.destroy();
    }
  });

  it('stores only the five most recently run command ids', async () => {
    localStorage.setItem('msgvault.palette.recent', JSON.stringify(['go:everything', 'view:3', 'view:4', 'view:5', 'view:6']));
    window.history.replaceState(null, '', '/?workspace=everything');
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });
    try {
      await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
      let palette = await screen.findByRole('dialog', { name: 'Messages commands' });
      await fireEvent.click(within(palette).getByRole('option', { name: 'People' }));
      expect(JSON.parse(localStorage.getItem('msgvault.palette.recent')!)).toEqual(['go:relationships', 'go:everything', 'view:3', 'view:4', 'view:5']);
      await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
      palette = await screen.findByRole('dialog', { name: 'Messages commands' });
      await fireEvent.click(within(palette).getByRole('option', { name: 'Messages' }));
      expect(JSON.parse(localStorage.getItem('msgvault.palette.recent')!)).toEqual(['go:everything', 'go:relationships', 'view:3', 'view:4', 'view:5']);
    } finally { rendered.unmount(); state.destroy(); }
  });

  it('leaves applied grouping out of palette Recent and highlights the first runnable recent command', async () => {
    window.history.replaceState(null, '', '/?workspace=everything');
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });
    try {
      await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
      let palette = await screen.findByRole('dialog', { name: 'Messages commands' });
      await fireEvent.click(within(palette).getByRole('option', { name: 'Files' }));
      await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
      palette = await screen.findByRole('dialog', { name: 'Messages commands' });
      await fireEvent.input(within(palette).getByRole('combobox'), { target: { value: 'source' } });
      await fireEvent.click(within(palette).getByRole('option', { name: 'Group by… › Source' }));
      expect(state.current.groupingChain).toEqual(['source']);
      await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
      palette = await screen.findByRole('dialog', { name: 'Messages commands' });
      const first = within(palette).getAllByRole('option')[0];
      expect(first.textContent?.trim()).toBe('Files');
      expect(first.getAttribute('aria-selected')).toBe('true');
      expect(first.getAttribute('aria-disabled')).not.toBe('true');
      expect(within(palette).queryByRole('option', { name: 'Group by… › Source' })).toBeNull();
      expect(palette.querySelector('.kit-command-palette__section')?.textContent?.trim()).toBe('Recent');
    } finally { rendered.unmount(); state.destroy(); }
  });

  it('runs Group by Source with Enter after a full command search at the palette root', async () => {
    window.history.replaceState(null, '', '/?workspace=everything');
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });
    try {
      await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
      const palette = await screen.findByRole('dialog', { name: 'Messages commands' });
      const input = within(palette).getByRole('combobox');
      await fireEvent.input(input, { target: { value: 'group by source' } });
      expect(within(palette).getByRole('option', { name: 'Group by… › Source' }).getAttribute('aria-selected')).toBe('true');
      await fireEvent.keyDown(input, { key: 'Enter' });
      expect(state.current.groupingChain).toEqual(['source']);
      expect(screen.queryByRole('dialog', { name: 'Messages commands' })).toBeNull();
    } finally { rendered.unmount(); state.destroy(); }
  });

  it('carries the Help search into the command palette and filters its commands', async () => {
    window.history.replaceState(null, '', '/?workspace=everything');
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });
    await fireEvent.click(screen.getByRole('button', { name: 'Help' }));
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search keyboard shortcuts' }), { target: { value: 'sources' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Search commands instead' }));
    const palette = await screen.findByRole('dialog', { name: 'Messages commands' });
    expect((within(palette).getByRole('combobox') as HTMLInputElement).value).toBe('sources');
    rendered.unmount(); state.destroy();
  });

  it('opens the command palette with Ctrl K while typing in Help', async () => {
    window.history.replaceState(null, '', '/?workspace=everything');
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });
    await fireEvent.click(screen.getByRole('button', { name: 'Help' }));
    const search = screen.getByRole('searchbox', { name: 'Search keyboard shortcuts' });
    await fireEvent.input(search, { target: { value: 'shift' } });
    await fireEvent.keyDown(search, { key: 'k', ctrlKey: true });
    const palette = await screen.findByRole('dialog', { name: 'Messages commands' });
    expect((within(palette).getByRole('combobox') as HTMLInputElement).value).toBe('');
    expect(screen.queryByRole('dialog', { name: 'Keyboard shortcuts' })).toBeNull();
    rendered.unmount(); state.destroy();
  });

  it.each([
    { filtered: false, direct: false }, { filtered: true, direct: false }, { filtered: false, direct: true }
  ])('continues deep Messages restoration after closing Past runs with a later page pending, filtered=$filtered direct=$direct', async ({ filtered, direct }) => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ selectedRow: 'message:3', activeRow: 'message:1', scrollAnchor: { key: 'message:1', offset: 4 }, operationHistory: direct }))}`);
    let finishPage!: (response: Response) => void;
    const cursors: Array<string | undefined> = [];
    const client = createAPIClient(async input => {
      const request = input as Request;
      const path = new URL(request.url).pathname;
      if (path.endsWith('/explore')) {
        const { cursor } = await request.clone().json();
        cursors.push(cursor);
        if (cursor === 'page:1') return new Promise<Response>(resolve => { finishPage = resolve; });
        return Response.json(exploreResponse({ rows: [entry(cursor ? 3 : 1)], total_count: 3,
          ...(!cursor ? { next_cursor: 'page:1' } : {}) }));
      }
      if (path.endsWith('/operations/runs')) return Response.json({ runs: [], unavailable_kinds: [], membership_revision: 1 });
      if (path.endsWith('/operations/status')) return Response.json({ lanes: [] });
      if (path.endsWith('/sources/status')) return Response.json({ sources: [] });
      if (path.endsWith('/saved-views')) return Response.json({ saved_views: [] });
      return Response.json({});
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client, state });
    try {
      await waitFor(() => expect(cursors).toEqual([undefined, 'page:1']));
      if (!direct) {
        await fireEvent.click(await screen.findByRole('button', { name: 'Sync status: Idle' }));
        await fireEvent.click(await screen.findByRole('menuitem', { name: /^Past runs/ }));
      }
      const dialog = await screen.findByRole('dialog', { name: 'Past runs' });
      if (filtered) await chooseSelectOption(within(dialog).getByRole('combobox', { name: /^Kind/ }), 'Source sync');
      const closed = direct ? Promise.resolve() : new Promise(resolve => window.addEventListener('popstate', resolve, { once: true }));
      await fireEvent.click(within(dialog).getByRole('button', { name: 'Close' }));
      await closed;
      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Past runs' })).toBeNull());
      expect(screen.queryByText('The selected entry is no longer available in this context.')).toBeNull();
      finishPage(Response.json(exploreResponse({ rows: [entry(2)], total_count: 3, next_cursor: 'page:2' })));
      await waitFor(() => expect(screen.getByRole('complementary', { name: /^Reading pane:/ }).textContent).toContain('Synthetic subject 3'));
      expect(cursors).toEqual([undefined, 'page:1', 'page:2']);
    } finally {
      rendered.unmount(); state.destroy();
    }
  });

  it.each([{ query: 'needle', cache: false }, { query: '', cache: true }])('labels an Everything load failure for $query with cache=$cache', async ({ query, cache }) => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything', query }))}`);
    const client = createAPIClient(async (input) => {
      const request = input as Request, path = new URL(request.url).pathname;
      if (path.endsWith('/explore')) return Response.json(cache ? { error: 'analytical_cache_unavailable', message: 'Browse unavailable', readiness: 'absent', recovery_action: 'msgvault cache build' } : { message: 'Browse unavailable' }, { status: cache ? 503 : 500 });
      return Response.json({});
    });
    const state = new ExploreState(window);
    render(AppShell, { client, state });
    await screen.findByText('Browse unavailable');
    state.destroy();
    await waitFor(() => expect(telemetryEvents.filter(event => event.event === 'error_shown')).toEqual([{ event: 'error_shown', properties: { surface: 'web', kind: query ? 'search' : 'other' } }]));
  });

  it.each([{ query: 'needle', leave: false }, { query: '', leave: true }])('reports an export error only while its view is displayed, query=$query leave=$leave', async ({ query, leave }) => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything', query }))}`);
    const state = new ExploreState(window);
    let finishExport!: (response: Response) => void;
    const client = createAPIClient(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path.endsWith('/explore')) return Response.json(exploreResponse({ rows: [entry(1)], total_count: 1 }));
      if (path.endsWith('/explore/preflight')) return Response.json({
        count: 1, deletable_count: 1, estimated_bytes: 10, cache_revision: 'cache-1', search_provenance: {},
        unavailable_actions: [], action_targets: [{ action: 'export', message_id: 1, filename: 'message.eml' }]
      });
      if (path.endsWith('/cli/message/raw')) return new Promise<Response>(resolve => { finishExport = resolve; });
      return Response.json({});
    });
    const view = render(AppShell, { client, state });
    await screen.findByText('Synthetic subject 1');
    const grid = screen.getByRole('grid', { name: 'Messages results' });
    grid.focus();
    await fireEvent.keyDown(grid, { key: ' ' });
    await fireEvent.click(await screen.findByRole('button', { name: 'Export selection' }));
    await waitFor(() => expect(finishExport).toBeTypeOf('function'));
    if (leave) {
      await fireEvent.click(screen.getByRole('button', { name: 'Sources' }));
      await screen.findByRole('main', { name: 'Sources' });
    }
    const response = Response.json({ message: 'Synthetic export failure' }, { status: 500 });
    finishExport(response);
    await waitFor(() => expect(response.bodyUsed).toBe(true));
    await new Promise(resolve => setTimeout(resolve, 0));
    await tick();
    if (leave) {
      expect(screen.queryByText('The original message isn’t available.')).toBeNull();
      expect(telemetryEvents.filter(event => event.event === 'error_shown')).toEqual([]);
    } else {
      await screen.findByText('The original message isn’t available.');
      await waitFor(() => expect(telemetryEvents.filter(event => event.event === 'error_shown')).toEqual([
        { event: 'error_shown', properties: { surface: 'web', kind: 'other' } }
      ]));
    }
    view.unmount();
    state.destroy();
  });

  it('reports each displayed archived meeting request error once', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything', selectedRow: 'archive-meeting:42' }))}`);
    const state = new ExploreState(window);
    const client = createAPIClient(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path.endsWith('/messages/42')) return Response.json({ message: 'Meeting request failed' }, { status: 500 });
      if (path.endsWith('/explore')) return Response.json(exploreResponse());
      return Response.json({});
    });
    const view = render(AppShell, { client, state });
    await screen.findByText('Meeting request failed');
    await waitFor(() => expect(telemetryEvents.filter(event => event.event === 'error_shown')).toEqual([{ event: 'error_shown', properties: { surface: 'web', kind: 'message' } }]));
    await view.rerender({ client, state });
    expect(telemetryEvents.filter(event => event.event === 'error_shown')).toHaveLength(1);
    await fireEvent.click(screen.getByRole('button', { name: 'Retry archived meeting' }));
    await screen.findByText('Meeting request failed');
    await waitFor(() => expect(telemetryEvents.filter(event => event.event === 'error_shown')).toHaveLength(2));
    state.destroy();
  });

  it('focuses search with slash and leaves Escape to the search input', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });

    await fireEvent.keyDown(window, { key: '/' });
    const search = screen.getByRole('searchbox', { name: 'Search messages' });
    expect(document.activeElement).toBe(search);
    await fireEvent.keyDown(search, { key: 'Escape' });
    expect(document.activeElement).toBe(search);
    state.destroy();
  });

  it('suspends the shortcut registry without preventing defaults in every editable control', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });
    const handleKeydown = vi.spyOn(appShortcuts, 'handleKeydown');
    const search = screen.getByRole('searchbox', { name: 'Search messages' });
    const textarea = document.createElement('textarea');
    const select = document.createElement('select');
    const editable = document.createElement('div');
    editable.setAttribute('contenteditable', 'plaintext-only');
    editable.tabIndex = 0;
    const iframe = document.createElement('iframe');
    document.body.append(textarea, select, editable, iframe);

    for (const control of [search, textarea, select, editable, iframe]) {
      control.focus();
      await waitFor(() => expect(appShortcuts.activeScope()).toBe('everything-editable'));
      for (const init of [
        { key: 'k', ctrlKey: true },
        { key: 'k', metaKey: true },
        { key: 'd' },
        { key: 'D', shiftKey: true },
        { key: 'Escape' }
      ]) {
        const event = new KeyboardEvent('keydown', { ...init, bubbles: true, cancelable: true });
        control.dispatchEvent(event);
        expect(handleKeydown.mock.results.at(-1)?.value).toBe(false);
        expect(event.defaultPrevented).toBe(false);
      }
    }

    textarea.focus();
    textarea.remove();
    await waitFor(() => expect(appShortcuts.activeScope()).toBe('root'));

    const outside = document.createElement('button');
    document.body.append(outside);
    outside.focus();
    const slash = new KeyboardEvent('keydown', { key: '/', bubbles: true, cancelable: true });
    outside.dispatchEvent(slash);
    expect(handleKeydown.mock.results.at(-1)?.value).toBe(true);
    expect(slash.defaultPrevented).toBe(true);
    expect(document.activeElement).toBe(search);

    outside.focus();
    await fireEvent.keyDown(outside, { key: 'd' });
    expect(state.current.workspace).toBe('everything');

    select.remove();
    editable.remove();
    iframe.remove();
    outside.remove();
    rendered.unmount();
    expect(appShortcuts.activeScope()).toBe('root');
    handleKeydown.mockRestore();
    state.destroy();
  });

  it('does not reinstall an editable shortcut scope from a queued callback after unmount', async () => {
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });
    const textarea = document.createElement('textarea');
    document.body.append(textarea);
    textarea.focus();
    await waitFor(() => expect(appShortcuts.activeScope()).toBe('everything-editable'));
    textarea.dispatchEvent(new FocusEvent('focusout', { bubbles: true }));
    rendered.unmount();
    await Promise.resolve();
    expect(appShortcuts.activeScope()).toBe('root');
    textarea.remove();
    state.destroy();
  });

  it('guards slash for modifiers and every editable contenteditable value except false', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });
    const search = screen.getByRole('searchbox', { name: 'Search messages' });
    const host = document.createElement('div');
    document.body.append(host);

    for (const value of ['', 'true', 'plaintext-only']) {
      host.setAttribute('contenteditable', value);
      host.focus();
      await fireEvent.keyDown(host, { key: '/' });
      expect(document.activeElement).toBe(host);
    }
    host.setAttribute('contenteditable', 'false');
    host.focus();
    await waitFor(() => expect(appShortcuts.activeScope()).toBe('root'));
    await fireEvent.keyDown(host, { key: '/' });
    expect(document.activeElement).toBe(search);

    for (const modifier of ['ctrlKey', 'metaKey', 'altKey'] as const) {
      host.focus();
      await fireEvent.keyDown(host, { key: '/', [modifier]: true });
      expect(document.activeElement).toBe(host);
    }
    await fireEvent.keyDown(host, { key: '?', shiftKey: true });
    expect(screen.getByRole('dialog', { name: 'Keyboard shortcuts' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    host.remove();
    state.destroy();
  });

  it('does not steal table shortcuts from editable controls', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });
    const search = screen.getByRole('searchbox', { name: 'Search messages' });
    search.focus();

    await fireEvent.keyDown(search, { key: 'j' });

    expect(document.activeElement).toBe(search);
    state.destroy();
  });

  it('commits workspace navigation to URL history', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    const push = vi.spyOn(window.history, 'pushState');
    render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });

    await fireEvent.click(screen.getByRole('button', { name: 'Settings' }));

    expect(state.current.workspace).toBe('settings');
    expect(push).toHaveBeenCalledOnce();
    state.destroy();
  });

  it('acknowledges history restoration immediately in a workspace without a result grid', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    state.replaceTransient({
      workspace: 'settings', activeRow: 'message:stale',
      scrollAnchor: { key: 'message:stale', offset: 12 }
    });
    const rendered = render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });

    await waitFor(() => expect(state.peekRestorationEpoch()).toBeUndefined());
    const theme = screen.getByRole('button', { name: 'Display' });
    theme.focus();
    await Promise.resolve();
    expect(document.activeElement).toBe(theme);

    rendered.unmount();
    state.destroy();
  });

  it('uses Display for temporary appearance and restores Settings defaults', async () => {
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false, appearanceDefaults: { theme: 'dark', density: 'compact' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Display' }));
    await fireEvent.click(screen.getByRole('menuitemradio', { name: 'Light' }));
    expect(document.documentElement.classList.contains('dark')).toBe(false);
    await fireEvent.click(screen.getByRole('menuitemradio', { name: 'Comfortable' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Display' }));
    for (const kind of ['theme', 'density']) {
      await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
      await fireEvent.click(await screen.findByRole('option', { name: `Use Settings ${kind}` }));
      expect(document.documentElement.classList.contains('dark')).toBe(true);
      expect(document.documentElement.dataset.density).toBe(kind === 'theme' ? 'comfortable' : 'compact');
    }
    await fireEvent.click(screen.getByRole('button', { name: 'Display' }));
    await fireEvent.click(screen.getByRole('menuitemradio', { name: 'Light' }));
    await fireEvent.click(screen.getByRole('menuitemradio', { name: 'Comfortable' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Use Settings defaults' }));
    expect(screen.queryByRole('menuitem', { name: 'Use Settings defaults' })).toBeNull();
    expect(document.documentElement.classList.contains('dark')).toBe(true);
    expect(document.documentElement.dataset.density).toBe('compact');
    rendered.unmount(); state.destroy();
  });

  it.each([
    ['everything', 'Messages'],
    ['relationships', 'People'], ['relationships', 'Possible duplicates'], ['deletions', 'Staged deletions'], ['settings', 'Settings']
  ])('reaches %s through the command palette', async (workspace, label) => {
    window.history.replaceState(null, '', `/?workspace=${workspace === 'everything' ? 'sources' : 'everything'}`);
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(async () => Response.json(exploreResponse())), state, enabled: false });
    await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
    await fireEvent.click(await screen.findByRole('option', { name: label }));
    expect(state.current.workspace).toBe(workspace);
    if (label === 'Possible duplicates') expect(state.current.peopleDuplicates).toBe(true);
    rendered.unmount(); state.destroy();
  });

  it.each([['everything', 'Messages', 'Cancel'], ['files', 'Files', 'Escape']] as const)(
    'offers Save view… in the %s header (%s); closing with %s returns focus to it',
    async (workspace, title, closeWith) => {
      window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace }))}`);
      const state = new ExploreState(window);
      const rendered = render(AppShell, {
        client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json(exploreResponse()))), state, enabled: false
      });
      const header = (await screen.findByRole('heading', { level: 1, name: title })).closest('header')!;
      const open = within(header).getByRole('button', { name: 'Save view…' });
      open.focus();
      await fireEvent.click(open);
      expect(screen.getByRole('dialog', { name: 'Save view' })).toBeDefined();
      if (closeWith === 'Cancel') await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
      else await fireEvent.keyDown(document.activeElement!, { key: 'Escape' });
      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Save view' })).toBeNull());
      expect(document.activeElement).toBe(open);
      rendered.unmount();
      state.destroy();
    }
  );

  it('saves a Files view from its header', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'files' }))}`);
    const stored: Record<string, unknown>[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/saved-views') && request.method === 'POST') {
        const body = await request.json();
        const view = {
          ...body, id: 5, revision: 1,
          created_at: '2026-07-19T10:00:00Z', updated_at: '2026-07-19T10:00:00Z'
        };
        stored.push(view);
        return Response.json(view, { status: 201 });
      }
      if (path.endsWith('/saved-views')) return Response.json({ saved_views: stored });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state, enabled: false });

    await fireEvent.click(await screen.findByRole('button', { name: 'Save view…' }));
    const dialog = screen.getByRole('dialog', { name: 'Save view' });
    await fireEvent.input(within(dialog).getByLabelText('Name'), { target: { value: 'Demo view' } });
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Save view' })).toBeNull());
    expect(screen.getByRole('status', { name: 'Operation status' }).textContent).toContain('Saved view Demo view.');
    const savedViews = await screen.findByRole('list', { name: 'Files saved views' });
    expect(await within(savedViews).findByRole('button', { name: 'Demo view' })).toBeDefined();

    rendered.unmount();
    state.destroy();
  });

  function savedFilesViewFetch(fileSearches: Request[]) {
    return vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/saved-views')) return Response.json({ saved_views: [{
        id: 5, name: 'Demo view', canonical_state: { presentation: 'files' },
        schema_version: 1, revision: 1,
        created_at: '2026-07-19T10:00:00Z', updated_at: '2026-07-19T10:00:00Z'
      }] });
      if (path.endsWith('/files/search')) {
        fileSearches.push(request);
        return Response.json({
          files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {}
        });
      }
      return Response.json(exploreResponse());
    });
  }

  async function openDemoView(): Promise<void> {
    await fireEvent.click(await screen.findByRole('button', { name: 'Demo view' }));
  }

  it.each([
    { workspace: 'files', via: 'sidebar', outcome: 'clears stale file filters and sort' },
    { workspace: 'everything', via: 'sidebar', outcome: 'focuses the Files grid' },
    { workspace: 'everything', via: 'palette', outcome: 'focuses the Files grid' },
  ] as const)('opens a saved Files view from $workspace via $via and $outcome', async ({ workspace, via }) => {
    window.history.replaceState(null, '', `/?workspace=${workspace}&mode=full_text`);
    const fileSearches: Request[] = [];
    const state = new ExploreState(window);
    if (workspace === 'files') state.replaceCommittedNavigation({
      workspace: 'files', fileFilenameQuery: 'invoice', fileMIMEFamilies: ['pdf'],
      fileSort: { field: 'filename', direction: 'asc' }
    });
    const rendered = render(AppShell, { client: createAPIClient(savedFilesViewFetch(fileSearches)), state });
    try {
      if (workspace === 'files') {
        await fireEvent.click(await screen.findByRole('button', { name: 'Filter' }));
        expect((await screen.findByLabelText<HTMLInputElement>('Filter filename')).value).toBe('invoice');
      }
      if (via === 'palette') {
        await screen.findByRole('button', { name: 'Demo view' });
        await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
        await fireEvent.click(await screen.findByRole('option', { name: 'Demo view' }));
      } else await openDemoView();
      const grid = await screen.findByRole('grid', { name: 'Files results' });
      if (workspace === 'everything') await waitFor(() => expect(document.activeElement).toBe(grid));
      if (workspace === 'files') {
        expect(screen.getByLabelText<HTMLInputElement>('Filter filename').value).toBe('');
        await waitFor(async () => {
          const body = await fileSearches.at(-1)!.clone().json();
          expect(body).not.toHaveProperty('filename_query');
          expect(body).not.toHaveProperty('mime_families');
          expect(body.sort).toEqual({ field: 'occurred_at', direction: 'desc' });
        });
      }
    } finally {
      rendered.unmount();
      state.destroy();
    }
  });

  it('focuses the Files grid when Escape removes a Files grouping level', async () => {
    window.history.replaceState(null, '', '/?workspace=files');
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path.endsWith('/files/groups')) return Response.json({
        rows: [{
          key: '7', label: 'Example source', count: 1, estimated_bytes: 1,
          latest_at: '2026-07-18T12:00:00Z'
        }],
        total_count: 1, cache_revision: 'cache-1', search_provenance: {}
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.replaceTransient({ workspace: 'files', groupingChain: ['source', 'year'] });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      await screen.findByRole('grid', { name: 'Files grouped by source' });
      await screen.findByText('Example source');
      (document.activeElement as HTMLElement | null)?.blur();
      const escape = { key: 'Escape', bubbles: true, cancelable: true };
      window.dispatchEvent(new KeyboardEvent('keydown', escape));
      await waitFor(() => expect(state.current.groupingChain).toEqual(['source']));
      const grid = screen.getByRole('grid', { name: 'Files grouped by source' });
      await waitFor(() => expect(document.activeElement).toBe(grid));
    } finally {
      rendered.unmount();
      state.destroy();
    }
  });

  describe('shell chrome', () => {
    const states: ExploreState[] = [];

    function shellState(workspace: string): ExploreState {
      window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace }))}`);
      const state = new ExploreState(window);
      states.push(state);
      return state;
    }

    function exploreClient() {
      return createAPIClient(vi.fn<typeof fetch>(async () => Response.json(exploreResponse())));
    }

    afterEach(() => {
      cleanup();
      for (const state of states.splice(0)) state.destroy();
      localStorage.removeItem(SIDEBAR_COLLAPSED_KEY);
      window.innerWidth = 1024;
    });

    it('names the browser tab after the workspace', async () => {
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
        if (path.endsWith('/sources/status')) return Response.json({ sources: [] });
        return Response.json(exploreResponse());
      });
      render(AppShell, { client: createAPIClient(fetchFn), state: shellState('sources'), enabled: false });
      expect(await screen.findByRole('main', { name: 'Sources' })).toBeDefined();
      await waitFor(() => expect(document.title).toBe('Sources · msgvault'));
      await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
      await fireEvent.click(await screen.findByRole('option', { name: 'People' }));
      await waitFor(() => expect(document.title).toBe('People · msgvault'));
    });

    it.each([
      ['relationships', 'People'],
      ['everything', 'Messages'], ['files', 'Files'],
      ['sources', 'Sources'], ['deletions', 'Staged deletions']
    ])('shows one visible page title in %s', async (workspace, title) => {
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
        if (path.endsWith('/saved-views')) return Response.json({ saved_views: [] });
        if (path.endsWith('/sources/status')) return Response.json({ sources: [] });
        if (path.endsWith('/operations/status')) return Response.json({ lanes: lanes() });
        if (path.endsWith('/operations/runs')) return Response.json({ runs: [], unavailable_kinds: [], membership_revision: 1 });
        if (path.endsWith('/deletions')) return Response.json({ manifests: [] });
        return Response.json(exploreResponse());
      });
      render(AppShell, { client: createAPIClient(fetchFn), state: shellState(workspace), enabled: false });
      const headings = await screen.findAllByRole('heading', { level: 1 });
      expect(headings.map((heading) => heading.textContent?.trim())).toEqual([title]);
      expect(headings[0]!.closest('.kit-sr-only')).toBeNull();
      expect(screen.queryByText(/archive workspace|archive operations/i)).toBeNull();
    });

    it('puts the Files title above the Everything context bar', async () => {
      render(AppShell, { client: exploreClient(), state: shellState('files'), enabled: false });
      const heading = await screen.findByRole('heading', { level: 1, name: 'Files' });
      const bar = screen.getByRole('region', { name: 'Active analytical context' });
      expect(heading.compareDocumentPosition(bar) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    });

    it.each([
      ['ungrouped', []],
      ['grouped', ['source']]
    ])('keeps the %s Files title inside one Files main landmark', async (_case, groupingChain) => {
      window.history.replaceState(
        null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'files', groupingChain }))}`
      );
      const state = new ExploreState(window);
      states.push(state);
      render(AppShell, { client: exploreClient(), state, enabled: false });
      const heading = await screen.findByRole('heading', { level: 1, name: 'Files' });
      const mains = screen.getAllByRole('main', { name: 'Files' });
      expect(mains).toHaveLength(1);
      expect(mains[0]!.contains(heading)).toBe(true);
      expect(screen.getAllByRole('main')).toHaveLength(1);
      // testing-library maps every <header> to banner; browsers only do so outside sectioning content.
      const banners = [...document.querySelectorAll('header')].filter((header) => !header.closest('main, section, article, aside, nav'));
      expect(banners).toHaveLength(1);
    });

    it('shows the file count once, in the toolbar, without a second heading', async () => {
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
        if (path.endsWith('/files/search')) {
          return Response.json({ files: [], total_count: 7, cache_revision: 'cache-1', search_provenance: {} });
        }
        return Response.json(exploreResponse());
      });
      render(AppShell, { client: createAPIClient(fetchFn), state: shellState('files'), enabled: false });
      const bar = screen.getByRole('region', { name: 'Active analytical context' });
      expect(await within(bar).findByText('7 files')).toBeDefined();
      expect(screen.getAllByText('7 files')).toHaveLength(1);
      expect(screen.queryByText('Count pending')).toBeNull();
      expect(screen.getAllByRole('heading', { level: 1 })).toHaveLength(1);
    });

    it('changes the Files order from the Sort menu and points r at it', async () => {
      const state = shellState('files');
      render(AppShell, { client: exploreClient(), state, enabled: false });
      await chooseSelectOption(screen.getByRole('combobox', { name: 'Sort: Newest first' }), 'Largest first');
      expect(state.current.fileSort).toEqual({ field: 'size', direction: 'desc' });
      expect(screen.getByRole('combobox', { name: 'Sort: Largest first' })).toBeDefined();

      await fireEvent.keyDown(window, { key: 'r' });
      expect(screen.getByRole('status', { name: 'Sort status' }).textContent)
        .toBe('Use the Sort menu to change the order.');
    });

    it('hides the Everything Sort menu while grouped and says why sorting is unavailable', async () => {
      const state = shellState('everything');
      state.replaceTransient({ groupingChain: ['source'] });
      render(AppShell, { client: exploreClient(), state, enabled: false });
      await screen.findByRole('region', { name: 'Active analytical context' });
      expect(screen.queryByRole('combobox', { name: /^Sort:/ })).toBeNull();

      await fireEvent.keyDown(window, { key: 's' });
      expect(screen.queryByRole('listbox')).toBeNull();
      await fireEvent.keyDown(window, { key: 'r' });
      expect(screen.getByRole('status', { name: 'Sort status' }).textContent)
        .toBe('Sorting isn’t available while grouped.');
    });

    it('hides the Files Sort menu while grouped, where file order does not apply', async () => {
      const state = shellState('files');
      state.replaceTransient({ groupingChain: ['source'] });
      render(AppShell, { client: exploreClient(), state, enabled: false });
      await screen.findByRole('region', { name: 'Active analytical context' });
      expect(screen.queryByRole('combobox', { name: /^Sort:/ })).toBeNull();
      expect(screen.getByRole('combobox', { name: 'Add grouping' })).toBeDefined();
    });

    it('lists every file order in the Files Sort menu', async () => {
      render(AppShell, { client: exploreClient(), state: shellState('files'), enabled: false });
      await fireEvent.click(screen.getByRole('combobox', { name: 'Sort: Newest first' }));
      const options = await screen.findAllByRole('option');
      expect(options.map((option) => option.textContent?.trim())).toEqual([
        'Newest first', 'Oldest first', 'Filename A–Z', 'Filename Z–A', 'Largest first', 'Smallest first'
      ]);
    });

    it('switches Messages to Files through Show as and returns through the sidebar, keeping query and filters', async () => {
      const state = shellState('everything');
      state.commitNavigation({ query: 'pasta', filters: [{ dimension: 'source', values: ['1'] }] });
      render(AppShell, { client: exploreClient(), state, enabled: false });

      await chooseSelectOption(screen.getByRole('combobox', { name: /^Show as:/ }), 'Files');
      expect(await screen.findByRole('main', { name: 'Files' })).toBeDefined();
      expect(state.current).toMatchObject({
        workspace: 'files', presentation: 'files', query: 'pasta',
        filters: [{ dimension: 'source', values: ['1'] }],
        activeRow: null, selectedRow: null, scrollAnchor: null
      });
      expect(screen.queryByRole('grid', { name: 'Files in current context' })).toBeNull();

      await fireEvent.click(screen.getByRole('button', { name: 'Messages' }));
      expect(await screen.findByRole('main', { name: 'Messages' })).toBeDefined();
      expect(state.current).toMatchObject({
        workspace: 'everything', presentation: 'table', query: 'pasta',
        filters: [{ dimension: 'source', values: ['1'] }]
      });
    });

    it('searches Files live as the global search is typed', async () => {
      const searches: Request[] = [];
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        const request = input instanceof Request ? input : new Request(input);
        if (new URL(request.url).pathname.endsWith('/files/search')) {
          searches.push(request);
          return Response.json({ files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} });
        }
        return Response.json(exploreResponse());
      });
      render(AppShell, { client: createAPIClient(fetchFn), state: shellState('files'), enabled: false });
      await screen.findByRole('grid', { name: 'Files results' });

      await fireEvent.input(screen.getByRole('searchbox', { name: 'Search messages' }), { target: { value: 'invoice' } });

      await waitFor(async () => {
        const body = await searches.at(-1)!.clone().json() as { predicate: { query?: string } };
        expect(body.predicate.query).toBe('invoice');
      });
    });

    it('does not flash the previous file count when returning to Files', async () => {
      let resolveSearch = true;
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
        if (path.endsWith('/files/search')) {
          if (!resolveSearch) return new Promise<Response>(() => {});
          return Response.json({ files: [], total_count: 7, cache_revision: 'cache-1', search_provenance: {} });
        }
        return Response.json(exploreResponse());
      });
      const state = shellState('files');
      render(AppShell, { client: createAPIClient(fetchFn), state, enabled: false });
      expect(await screen.findByText('7 files')).toBeDefined();
      const nav = screen.getByRole('navigation', { name: 'Primary' });
      await fireEvent.click(within(nav).getByRole('button', { name: 'Messages' }));
      await screen.findByRole('main', { name: 'Messages' });

      resolveSearch = false;
      const shown: string[] = [];
      const observer = new MutationObserver((records) => {
        for (const record of records) if (record.oldValue) shown.push(record.oldValue);
      });
      observer.observe(document.body, { subtree: true, characterData: true, characterDataOldValue: true });
      await fireEvent.click(within(nav).getByRole('button', { name: 'Files' }));
      const bar = await screen.findByRole('region', { name: 'Active analytical context' });
      await waitFor(() => expect(within(bar).getByText('Counting…')).toBeDefined());
      observer.disconnect();
      expect(shown.filter((text) => /\d+ files?$/.test(text))).toEqual([]);
      expect(within(bar).queryByText('7 files')).toBeNull();
    });

    it.each([
      ['everything', '/api/v1/explore'],
      ['files', '/api/v1/files/search']
    ])('leaves the %s count empty instead of Counting… when the request fails', async (workspace, failingPath) => {
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
        if (path === failingPath) return Response.json({ error: 'internal', message: 'Synthetic failure' }, { status: 500 });
        return Response.json(exploreResponse());
      });
      render(AppShell, { client: createAPIClient(fetchFn), state: shellState(workspace), enabled: true });
      await screen.findByText(/Synthetic failure/);
      const bar = screen.getByRole('region', { name: 'Active analytical context' });
      expect(within(bar).queryByText('Counting…')).toBeNull();
    });

    it('shows the Size column after selecting it in Display', async () => {
      const fetchFn = vi.fn<typeof fetch>(async () => Response.json(exploreResponse({
        rows: [entry(1)], total_count: 1
      })));
      render(AppShell, { client: createAPIClient(fetchFn), state: shellState('everything') });
      await screen.findByText('Synthetic subject 1');
      expect(screen.queryByRole('columnheader', { name: 'Size' })).toBeNull();

      await fireEvent.click(screen.getByRole('button', { name: 'Display' }));
      await fireEvent.click(screen.getByRole('menuitem', { name: 'Size' }));

      expect(await screen.findByRole('columnheader', { name: 'Size' })).toBeDefined();
    });

    it('opens Everything with the query when searching from another workspace', async () => {
      const state = shellState('sources');
      const length = window.history.length;
      render(AppShell, { client: exploreClient(), state, enabled: false });

      const search = screen.getByRole('searchbox', { name: 'Search messages' });
      await fireEvent.input(search, { target: { value: 'pipeline' } });
      expect(state.current.workspace).toBe('sources');
      await fireEvent.submit(screen.getByRole('search', { name: 'Search Messages' }));

      expect(state.current.workspace).toBe('everything');
      expect(state.current.query).toBe('pipeline');
      expect(window.history.length).toBe(length + 1);
    });

    it('moves focus to the Files results after submitting search on Files', async () => {
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
        if (path.endsWith('/files/search')) {
          return Response.json({ files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} });
        }
        return Response.json(exploreResponse());
      });
      const state = shellState('files');
      render(AppShell, { client: createAPIClient(fetchFn), state, enabled: false });
      const grid = await screen.findByRole('grid', { name: 'Files results' });

      const search = screen.getByRole('searchbox', { name: 'Search messages' });
      search.focus();
      await fireEvent.input(search, { target: { value: 'invoice' } });
      await fireEvent.submit(screen.getByRole('search', { name: 'Search Messages' }));

      expect(state.current.query).toBe('invoice');
      expect(document.activeElement).toBe(grid);
    });

    it('drops an unsubmitted search draft when the sidebar opens another workspace', async () => {
      const state = shellState('sources');
      render(AppShell, { client: exploreClient(), state, enabled: false });

      const search = screen.getByRole('searchbox', { name: 'Search messages' }) as HTMLInputElement;
      await fireEvent.input(search, { target: { value: 'pipeline' } });
      await fireEvent.click(within(screen.getByRole('navigation', { name: 'Primary' })).getByRole('button', { name: 'Messages' }));

      expect(state.current.workspace).toBe('everything');
      expect(state.current.query).toBe('');
      await waitFor(() => expect(search.value).toBe(''));
    });

    it.each([false, true, 'pending'] as const)('gates empty search reporting with media readiness %s', async (ready) => {
      const state = shellState('everything'), events = telemetryEvents;
      const client = createAPIClient(vi.fn<typeof fetch>(async () => {
        return Response.json(exploreResponse());
      }));
      const media = new IntegrationReadiness(async () => {
        if (ready === 'pending') return new Promise(() => {});
        return ready;
      });
      // The reporter sends each search key once a day, so its calls show what each search reported.
      const daily = vi.spyOn(startTelemetryReporting(client), 'reportDaily');
      const reported = () => daily.mock.calls.map(([key]) => key);
      render(AppShell, { props: { client, state }, context: new Map([[mediaReadinessKey, media]]) });
      if (ready === true) await waitFor(() => expect(media.ready).toBe(true));
      if (ready === false) await waitFor(() => expect(media.absent).toBe(true));
      const search = screen.getByRole('searchbox', { name: 'Search messages' });
      await fireEvent.input(search, { target: { value: 'missing' } });
      await waitFor(() => expect(reported()).toContain('search_run:text'));
      expect(reported()).not.toContain('search_empty:text');
      daily.mockClear();
      await fireEvent.submit(search.closest('form')!);
      await waitFor(() => expect(reported()).toContain('search_run:text'));
      media.dispose();
      if (ready !== false) {
        expect(reported()).not.toContain('search_empty:text');
        return;
      }
      await waitFor(() => expect(events.filter((event) => event.event === 'search_empty')).toHaveLength(1));
      expect(events.find((event) => event.event === 'search_empty')?.properties).toEqual({ surface: 'web', search_type: 'text' });
    });

    it.each(['visible', 'destroy'])('reports an accepted Everything search after %s', async (trigger) => {
      const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue(trigger === 'visible' ? 'visible' : 'hidden');
      const state = shellState('everything');
      const events = () => telemetryEvents.filter(event => event.event === 'search_run').map(event => event.properties.search_type);
      const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
        const request = input as Request;
        const predicate = request.method === 'POST' ? await request.clone().json() : {};
        return Response.json(exploreResponse({ rows: predicate.query === 'gas' ? [entry(1)] : [] }));
      }));
      const daily = vi.spyOn(startTelemetryReporting(client), 'reportDaily');
      const view = render(AppShell, { client, state });
      await fireEvent.input(screen.getByRole('searchbox', { name: 'Search messages' }), { target: { value: 'gas' } });
      expect(state.current.query).toBe('gas');
      await screen.findByText('Synthetic subject 1');
      if (trigger !== 'visible') {
        expect(daily).not.toHaveBeenCalled();
        if (trigger === 'destroy') view.unmount();
        visibility.mockReturnValue('visible');
        await fireEvent(window, new Event('focus'));
      }
      if (trigger === 'destroy') expect(daily).not.toHaveBeenCalled();
      else {
        await waitFor(() => expect(events()).toEqual(['text']));
        await fireEvent(window, new Event('focus'));
        expect(daily).toHaveBeenCalledTimes(1);
      }
      stopTelemetryReporting(client);
      visibility.mockRestore();
    });

    it('remembers the collapsed sidebar across reloads', async () => {
      const state = shellState('everything');
      const first = render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });
      await fireEvent.click(screen.getByRole('button', { name: 'Collapse sidebar' }));
      first.unmount();
      render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });
      expect(screen.getByRole('button', { name: 'Expand sidebar' })).toBeTruthy();
    });

    it('opens a modal navigation menu on narrow screens and closes it on Escape', async () => {
      window.innerWidth = 480;
      render(AppShell, { client: createAPIClient(vi.fn()), state: shellState('everything'), enabled: false });

      expect(screen.queryByRole('navigation', { name: 'Primary' })).toBeNull();
      const opener = screen.getByRole('button', { name: 'Open navigation' });
      await fireEvent.click(opener);
      const current = await screen.findByRole('button', { name: 'Messages' });
      await waitFor(() => expect(document.activeElement).toBe(current));

      await fireEvent.keyDown(current, { key: 'Escape' });
      await waitFor(() => expect(screen.queryByRole('navigation', { name: 'Primary' })).toBeNull());
      expect(document.activeElement).toBe(opener);
    });

    it('closes the narrow navigation menu after choosing a workspace and returns focus to its opener', async () => {
      window.innerWidth = 480;
      const state = shellState('everything');
      render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });

      const opener = screen.getByRole('button', { name: 'Open navigation' });
      await fireEvent.click(opener);
      await fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));

      expect(state.current.workspace).toBe('settings');
      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Navigation' })).toBeNull());
      expect(document.activeElement).toBe(opener);
    });

    it('returns focus to the sidebar entry when the shortcuts dialog closes', async () => {
      render(AppShell, { client: createAPIClient(vi.fn()), state: shellState('everything'), enabled: false });
      const entry = screen.getByRole('button', { name: 'Help' });
      await fireEvent.click(entry);
      const search = await screen.findByRole('searchbox', { name: 'Search keyboard shortcuts' });

      await fireEvent.keyDown(search, { key: 'Escape' });
      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Keyboard shortcuts' })).toBeNull());
      await waitFor(() => expect(document.activeElement).toBe(entry));
    });

    it('returns focus to the prior control when the shortcuts dialog was opened with ?', async () => {
      render(AppShell, { client: createAPIClient(vi.fn()), state: shellState('settings'), enabled: false });
      const theme = screen.getByRole('button', { name: 'Display' });
      theme.focus();
      await fireEvent.keyDown(theme, { key: '?', shiftKey: true });
      await fireEvent.click(await screen.findByRole('button', { name: 'Close' }));

      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Keyboard shortcuts' })).toBeNull());
      await waitFor(() => expect(document.activeElement).toBe(theme));
    });

    it('returns focus to the navigation opener when shortcuts opened from the narrow menu close', async () => {
      window.innerWidth = 480;
      render(AppShell, { client: createAPIClient(vi.fn()), state: shellState('everything'), enabled: false });
      const opener = screen.getByRole('button', { name: 'Open navigation' });
      await fireEvent.click(opener);
      await fireEvent.click(await screen.findByRole('button', { name: 'Help' }));
      const search = await screen.findByRole('searchbox', { name: 'Search keyboard shortcuts' });

      await fireEvent.keyDown(search, { key: 'Escape' });
      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Keyboard shortcuts' })).toBeNull());
      await waitFor(() => expect(document.activeElement).toBe(opener));
    });

    it('focuses the workspace heading after Forward returns to Staged deletions on desktop', async () => {
      window.innerWidth = 1280;
      const state = shellState('everything');
      const client = createAPIClient(async input => {
        const path = new URL((input as Request).url).pathname;
        if (path.endsWith('/deletions')) return Response.json({ manifests: [] });
        if (path.endsWith('/saved-views')) return Response.json({ saved_views: [] });
        return Response.json(exploreResponse({ rows: [entry(1)], total_count: 1 }));
      });
      render(AppShell, { client, state });
      await screen.findByText('Synthetic subject 1');
      await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
      await fireEvent.click(await screen.findByRole('option', { name: 'Staged deletions' }));
      await screen.findByRole('heading', { level: 1, name: 'Staged deletions' });

      window.history.back();
      await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('grid', { name: 'Messages results' })));

      window.history.forward();
      const heading = await screen.findByRole('heading', { level: 1, name: 'Staged deletions' });
      await waitFor(() => expect(document.activeElement).toBe(heading));
    });

    it('focuses the navigation opener after Back on narrow screens', async () => {
      window.innerWidth = 480;
      const state = shellState('sources');
      render(AppShell, { client: exploreClient(), state, enabled: false });
      await fireEvent.click(screen.getByRole('button', { name: 'Open navigation' }));
      await fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Navigation' })).toBeNull());
      (document.activeElement as HTMLElement).blur();

      window.history.back();
      await waitFor(() => expect(state.current.workspace).toBe('sources'));
      await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Open navigation' })));
    });
  });

  it('passes the URL Settings category to Settings and commits a chosen one', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'settings', settingsCategory: 'search'
    }))}`);
    const categories: string[] = [];
    let choose: ((categoryID: string) => void) | undefined;
    const settings = createRawSnippet<
      [unknown, (key: number) => void, unknown, string, (categoryID: string) => void]
    >((_request, _consumed, _target, getCategory, getChoose) => ({
      render: () => '<main aria-label="Settings category fixture"></main>',
      setup: () => {
        categories.push(getCategory());
        choose = getChoose();
      }
    }));
    const state = new ExploreState(window);
    const rendered = render(AppShell, {
      client: createAPIClient(vi.fn()), state, enabled: false, settings: settings as never
    });
    try {
      expect(await screen.findByRole('main', { name: 'Settings category fixture' })).toBeDefined();
      expect(categories.at(-1)).toBe('search');
      choose!('server');
      expect(state.current).toMatchObject({
        workspace: 'settings', settingsCategory: 'server', settingsAuthority: ''
      });
    } finally {
      rendered.unmount();
      state.destroy();
    }
  });

  it.each(['constructor', 'toString', '__proto__'] as const)(
    'does not pass inherited Settings authority %s to the Settings workspace',
    async (settingsAuthority) => {
      window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
        workspace: 'settings', settingsAuthority
      }))}`);
      const settingsTargets: unknown[] = [];
      const settings = createRawSnippet<[unknown, (key: number) => void, unknown]>(
        (_getRequest, _getConsumed, getTarget) => ({
          render: () => '<main aria-label="Settings target fixture"></main>',
          setup: () => { settingsTargets.push(getTarget?.()); }
        })
      );
      const state = new ExploreState(window);
      const rendered = render(AppShell, {
        client: createAPIClient(vi.fn()), state, enabled: false, settings: settings as never
      });

      expect(await screen.findByRole('main', { name: 'Settings target fixture' })).toBeDefined();
      expect(settingsTargets.at(-1)).toBeUndefined();
      rendered.unmount();
      state.destroy();
    }
  );







  function directoryPersonFetch(): typeof fetch {
    return vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      const meetingResponse = meetingFixtureResponse(path);
      if (meetingResponse) return meetingResponse;
      if (path === '/api/v1/people/directory') return Response.json({ people: [{
        id: 7, revision: 1, display_name: 'Synthetic Person', contact_state: 'active',
        categories: [], organizations: []
      }] });
      const one = { id: 7, revision: 1, display_name: 'Synthetic Person', participant_ids: [42, 19], vcard_uid: '', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z' };
      if (path === '/api/v1/people/7') return Response.json(one);
      if (path === '/api/v1/people/7/network') return Response.json({ root_person_id: 7, depth: 1, truncated: false, nodes: [], edges: [] });
      if (path === '/api/v1/people/7/contact-state') return Response.json({
        person_id: 7, cadence_status: 'ok', interaction_count: 0, computed_at: '2026-08-01T00:00:00Z', stale: false
      });
      if (/\/api\/v1\/participants\/(42|19)$/.test(path)) return Response.json({ id: Number(path.split('/').at(-1)), display_label: 'Synthetic Person', profile: one, identifiers: [], activity_count: 0, file_count: 0, source_counts: [], first_at: '2026-08-01T00:00:00Z', last_at: '2026-08-01T00:00:00Z' });
      if (path === '/api/v1/person-fact-targets') return Response.json({ fingerprint: 'safe', version: 'safe', targets: [] });
      if (path.endsWith('/fact-claims')) return Response.json({ claims: [] });
      return Response.json(exploreResponse());
    });
  }

  it('owns an ephemeral CardDAV conflict handoff and Browser Back restores the prior Directory person', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'relationships', directoryPersonID: 7
    }))}`);
    let publicationReads = 0;
    let restoredPublicationSignal: AbortSignal | undefined;
    let resolveRestoredPublication!: (response: Response) => void;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const meetingResponse = meetingFixtureResponse(new URL(request.url).pathname);
      if (meetingResponse) return meetingResponse;
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/people/directory') return Response.json({ people: [{
        id: 7, revision: 1, display_name: 'Synthetic Person', contact_state: 'active',
        categories: [], organizations: []
      }] });
      if (path === '/api/v1/carddav/publications/7') {
        publicationReads += 1;
        if (publicationReads === 1) return Response.json({
          person_id: 7, state: 'conflict', desired: true, conflict_id: 41,
          address_book: { id: 5, name: 'Synthetic contacts' }
        });
        restoredPublicationSignal = request.signal;
        return new Promise<Response>((resolve) => { resolveRestoredPublication = resolve; });
      }
      if (path === '/api/v1/people/7') return Response.json({
        id: 7, revision: 1, display_name: 'Synthetic Person', participant_ids: [], vcard_uid: '',
        created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z'
      });
      if (path === '/api/v1/people/7/profile') return Response.json({
        person: { id: 7, revision: 1, display_name: 'Synthetic Person' },
        names: [], contact_points: [], addresses: [], dates: [], categories: [], media: []
      });
      if (path === '/api/v1/people/7/attributes') return Response.json({ person_id: 7, attributes: [] });
      if (path === '/api/v1/people/7/network') return Response.json({ root_person_id: 7, depth: 1, truncated: false, nodes: [], edges: [] });
      if (path === '/api/v1/people/7/contact-state') return Response.json({
        person_id: 7, cadence_status: 'ok', interaction_count: 0,
        computed_at: '2026-08-28T10:00:00Z', stale: false
      });
      if (path === '/api/v1/people/7/employments') return Response.json({ employments: [] });
      if (path === '/api/v1/people/7/relationships') return Response.json({ relationships: [] });
      if (path === '/api/v1/people/7/files/search') return Response.json({
        files: [], total_count: 0, cache_revision: 'synthetic', search_provenance: {}
      });
      if (path === '/api/v1/people/7/merges') return Response.json({ merges: [], limit: 100, offset: 0 });
      return Response.json(exploreResponse());
    });
    const settingsHandoffs: Array<{ request: unknown; target: unknown }> = [];
    const settings = createRawSnippet<[unknown, (key: number) => void, unknown]>((getRequest, _getConsumed, getTarget) => ({
      render: () => '<main aria-label="CardDAV Settings fixture"></main>',
      setup: () => { settingsHandoffs.push({ request: getRequest(), target: getTarget?.() }); }
    }));
    const state = new ExploreState(window);
    const rendered = render(AppShell, {
      client: createAPIClient(fetchFn), state, enabled: false, settings: settings as never
    });

    await fireEvent.click(await screen.findByRole('button', { name: 'More actions' }));
    await fireEvent.click(await screen.findByRole('menuitem', { name: 'Publish to address book…' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Review CardDAV conflict 41' }));
    expect(state.current).toMatchObject({ workspace: 'settings', settingsCategory: 'carddav' });
    expect(settingsHandoffs.at(-1)?.request).toMatchObject({ conflictID: 41 });
    expect(settingsHandoffs.at(-1)?.target).toBeUndefined();
    expect(screen.getByRole('status', { name: 'Operation status' }).textContent)
      .toBe('Opening CardDAV conflict 41 in Settings.');

    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    await waitFor(() => expect(state.current).toMatchObject({ workspace: 'relationships', directoryPersonID: 7 }));
    expect(await screen.findByRole('heading', { name: 'Synthetic Person' })).toBeDefined();
    expect(screen.getAllByRole('status', { name: 'Operation status' })).toHaveLength(1);
    await fireEvent.click(screen.getByRole('button', { name: 'More actions' }));
    await fireEvent.click(await screen.findByRole('menuitem', { name: 'Publish to address book…' }));
    await waitFor(() => expect(restoredPublicationSignal).toBeDefined());

    await fireEvent.click(within(screen.getByRole('dialog', { name: 'Publish to address book' })).getByRole('button', { name: 'Close' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    expect(restoredPublicationSignal?.aborted).toBe(true);
    resolveRestoredPublication(Response.json({
      person_id: 7, state: 'published', desired: true,
      address_book: { id: 6, name: 'Old contacts' }
    }));
    await Promise.resolve();
    expect(screen.queryByText('Old contacts')).toBeNull();

    rendered.unmount();
    state.destroy();
  });





  it('starts a newly selected person on Messages and restores the prior Files view on Back', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'relationships', relationshipTarget: 'cluster:1'
    }))}`);
    const when = '2026-07-19T10:00:00Z';
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      const meetingResponse = meetingFixtureResponse(path);
      if (meetingResponse) return meetingResponse;
      if (path === '/api/v1/relationships') return Response.json({
        rows: [1, 2].map((id) => ({
          canonical_id: id, display_label: id === 1 ? 'Alice Example' : 'Bob Example',
          last_at: when, member_ids: [id], score: 1,
          signals: { last_interaction_at: when, meeting_count: 0, meetings_together: 0,
            modalities: 1, received_from_them: 1, sent_count: 1, sent_to_them: 1 }
        }))
      });
      if (path === '/api/v1/participants/1' || path === '/api/v1/participants/2') {
        const id = Number(path.at(-1));
        return Response.json({
          id, display_label: id === 1 ? 'Alice Example' : 'Bob Example', partial_label: false,
          identifiers: [], activity_count: 1, meeting_count: 0, file_count: 1,
          source_counts: [], first_at: when, last_at: when, cache_revision: 'cache-rel'
        });
      }
      if (path.endsWith('/timeline')) return Response.json({
        canonical_id: Number(path.split('/')[4]), identity_revision: 1,
        cache_revision: 'cache-rel', rows: [], total_count: 0
      });
      if (path.endsWith('/files/search')) return Response.json({
        files: [], total_count: 0, cache_revision: 'cache-rel', search_provenance: {}
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect(await screen.findByRole('heading', { name: 'Alice Example' })).toBeDefined();
    await fireEvent.click(screen.getByRole('radio', { name: 'Files' }));
    expect(state.current.relationshipFiles).toBe(true);
    await fireEvent.click((await screen.findByText('Bob Example')).closest('[role="row"]')!);
    await waitFor(() => expect(state.current).toMatchObject({
      relationshipTarget: 'cluster:2', relationshipFiles: false
    }));
    expect(await screen.findByRole('heading', { name: 'Bob Example' })).toBeDefined();
    expect(screen.getByRole('radio', { name: 'Messages' }).getAttribute('aria-checked')).toBe('true');

    window.history.back();
    await waitFor(() => expect(state.current).toMatchObject({
      relationshipTarget: 'cluster:1', relationshipFiles: true
    }));
    expect(await screen.findByRole('heading', { name: 'Alice Example' })).toBeDefined();
    expect(within(screen.getByRole('radiogroup', { name: 'Person tabs' })).getByRole('radio', { name: 'Files' }).getAttribute('aria-checked')).toBe('true');

    rendered.unmount();
    state.destroy();
  });

  it('restores a legacy workspace=people URL into the hub with the facet set', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'people', analysisTarget: 'person:42'
    }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path === '/api/v1/participants/42') return Response.json({
        id: 42, display_label: 'Legacy Person', partial_label: false, identifiers: [],
        activity_count: 1, file_count: 0, source_counts: [], first_at: '2026-07-19T10:00:00Z',
        last_at: '2026-07-19T10:00:00Z', cache_revision: 'cache-legacy'
      });
      if (path === '/api/v1/participants/42/timeline') return Response.json({
        cache_revision: 'cache-legacy', rows: [], total_count: 0, search_provenance: {}
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect(await screen.findByRole('main', { name: 'People' })).toBeDefined();
    expect(state.current.workspace).toBe('relationships');
    expect(state.current.relationshipFacet).toBe('people');
    expect(state.current.relationshipTarget).toBe('cluster:42');
    expect(screen.getByRole('radio', { name: 'People' }).getAttribute('aria-checked')).toBe('true');
    expect(await screen.findByRole('heading', { name: 'Legacy Person' })).toBeDefined();

    rendered.unmount();
    state.destroy();
  });

  it('opens an old Everything-as-Files link in Files with its attachment viewer', async () => {
    window.history.replaceState(null, '', `/?workspace=everything&explore=${encodeURIComponent(JSON.stringify({
      presentation: 'files', selectedRow: 'attachment:5'
    }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path.endsWith('/files/5')) return Response.json({
        id: 5, message_id: 1, conversation_id: 11, filename: 'legacy-report.pdf',
        mime_type: 'application/pdf', size_bytes: 2048,
        content_state: 'missing_blob', content_available: false
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      expect(await screen.findByRole('main', { name: 'Files' })).toBeDefined();
      expect(await screen.findByRole('dialog', { name: 'View legacy-report.pdf' })).toBeDefined();
      expect(state.current).toMatchObject({
        workspace: 'files', presentation: 'files', selectedRow: 'attachment:5'
      });
      expect(window.location.search).toContain('workspace=files');
    } finally {
      rendered.unmount();
      state.destroy();
    }
  });

  it.each([
    ['the Close button', async () => {
      await fireEvent.click(screen.getByRole('button', { name: 'Close file viewer' }));
    }],
    ['Escape', async () => {
      window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));
    }]
  ])('clears a legacy attachment selection when its viewer closes with %s', async (_label, close) => {
    window.history.replaceState(null, '', `/?workspace=everything&explore=${encodeURIComponent(JSON.stringify({
      presentation: 'files', selectedRow: 'attachment:5'
    }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path.endsWith('/files/5')) return Response.json({
        id: 5, message_id: 1, conversation_id: 11, filename: 'legacy-report.pdf',
        mime_type: 'application/pdf', size_bytes: 2048,
        content_state: 'missing_blob', content_available: false
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      expect(await screen.findByRole('dialog', { name: 'View legacy-report.pdf' })).toBeDefined();
      await close();
      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'View legacy-report.pdf' })).toBeNull());
      expect(state.current.selectedRow).toBeNull();
      expect(new URLSearchParams(window.location.search).get('explore') ?? '').not.toContain('attachment:5');
    } finally {
      rendered.unmount();
      state.destroy();
    }
  });

  it.each([
    ['the Close button', async () => {
      await fireEvent.click(screen.getByRole('button', { name: 'Close file viewer' }));
    }],
    ['Escape', async () => {
      const escape = { key: 'Escape', bubbles: true, cancelable: true };
      window.dispatchEvent(new KeyboardEvent('keydown', escape));
    }]
  ])('focuses the Files grid when a legacy file viewer closes with %s', async (_label, close) => {
    const explore = encodeURIComponent(JSON.stringify({ selectedRow: 'attachment:5' }));
    window.history.replaceState(null, '', `/?workspace=files&explore=${explore}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path.endsWith('/files/5')) return Response.json({
        id: 5, message_id: 1, conversation_id: 11, filename: 'legacy-report.pdf',
        mime_type: 'application/pdf', size_bytes: 2048,
        content_state: 'missing_blob', content_available: false
      });
      if (path.endsWith('/files/search')) return Response.json({
        files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {}
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      const viewer = await screen.findByRole('dialog', { name: 'View legacy-report.pdf' });
      const grid = await screen.findByRole('grid', { name: 'Files results' });
      // Let the Files list finish restoring so its settle-time focus cannot mask the close.
      await waitFor(() => expect(state.peekRestorationEpoch()).toBeUndefined());
      await close();
      await waitFor(() => expect(viewer.isConnected).toBe(false));
      await waitFor(() => expect(document.activeElement).toBe(grid));
    } finally {
      rendered.unmount();
      state.destroy();
    }
  });

  it('opens an old attachment link with one Files page instead of paging to find it', async () => {
    window.history.replaceState(null, '', `/?workspace=files&explore=${encodeURIComponent(JSON.stringify({
      selectedRow: 'attachment:5'
    }))}`);
    let filePages = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path.endsWith('/files/5')) return Response.json({
        id: 5, message_id: 1, conversation_id: 11, filename: 'legacy-report.pdf',
        mime_type: 'application/pdf', size_bytes: 2048,
        content_state: 'missing_blob', content_available: false
      });
      if (path.endsWith('/files/search')) {
        filePages += 1;
        return Response.json({
          files: [{
            id: 100 + filePages, key: `file:${100 + filePages}`, entry_key: `message:${filePages}`,
            message_id: filePages, conversation_id: filePages, occurred_at: '2026-07-18T12:00:00Z',
            source_id: 1, source_type: 'synthetic', source_identifier: 'archive@example.com',
            containing_title: 'Containing item', filename: `page-${filePages}.pdf`,
            mime_type: 'application/pdf', mime_family: 'pdf', size_bytes: 1,
            content_state: 'unverified', content_available: null
          }],
          total_count: 5, cache_revision: 'cache-1', search_provenance: {},
          ...(filePages < 5 ? { next_cursor: `page-${filePages}` } : {})
        });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      expect(await screen.findByRole('dialog', { name: 'View legacy-report.pdf' })).toBeDefined();
      const grid = await screen.findByRole('grid', { name: 'Files results' });
      expect(await screen.findByText('page-1.pdf')).toBeDefined();
      await waitFor(() => expect(grid.getAttribute('aria-busy')).toBe('false'));
      expect(filePages).toBe(1);
    } finally {
      rendered.unmount();
      state.destroy();
    }
  });

  it('focuses the Files grid once a restored Files list settles on its fallback row', async () => {
    window.history.replaceState(null, '', `/?workspace=files&explore=${encodeURIComponent(JSON.stringify({
      activeRow: 'source:1:message:m9:file:9'
    }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path.endsWith('/files/search')) return Response.json({
        files: [{
          id: 1, key: 'source:1:message:m1:file:1', entry_key: 'source:1:message:m1', message_id: 1,
          conversation_id: 1, occurred_at: '2026-07-18T12:00:00Z', source_id: 1, source_type: 'synthetic',
          source_identifier: 'archive@example.com', containing_title: 'Containing item',
          filename: 'fallback.pdf', mime_type: 'application/pdf', mime_family: 'pdf', size_bytes: 1,
          content_state: 'unverified', content_available: null
        }],
        total_count: 1, cache_revision: 'cache-1', search_provenance: {}
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      const grid = await screen.findByRole('grid', { name: 'Files results' });
      expect(await screen.findByText('fallback.pdf')).toBeDefined();
      await waitFor(() => expect(document.activeElement).toBe(grid));
    } finally {
      rendered.unmount();
      state.destroy();
    }
  });

  it('focuses the Files grid when Back returns to Files from the reading pane', async () => {
    window.history.replaceState(null, '', '/?workspace=files');
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path.endsWith('/files/search')) return Response.json({
        files: [{
          id: 1, key: 'source:1:message:m1:file:1', entry_key: 'message:1', message_id: 1,
          conversation_id: 1, occurred_at: '2026-07-18T12:00:00Z', source_id: 1, source_type: 'synthetic',
          source_identifier: 'archive@example.com', containing_title: 'Containing item',
          filename: 'returned.pdf', mime_type: 'application/pdf', mime_family: 'pdf', size_bytes: 1,
          content_state: 'unverified', content_available: null
        }],
        total_count: 1, cache_revision: 'cache-1', search_provenance: {}
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      expect(await screen.findByText('returned.pdf')).toBeDefined();
      await fireEvent.click(screen.getByRole('button', { name: 'Open containing item Containing item' }));
      expect(await screen.findByRole('main', { name: 'Messages' })).toBeDefined();
      const returned = new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
      window.history.back();
      await returned;
      const grid = await screen.findByRole('grid', { name: 'Files results' });
      await waitFor(() => expect(document.activeElement).toBe(grid));
    } finally {
      rendered.unmount();
      state.destroy();
    }
  });

  it('keeps Relationships and shows its degraded state when the URL explicitly names it', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'relationships' }))}`);
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      error: 'analytical_cache_unavailable', message: 'The committed analytical cache is unavailable',
      readiness: 'stale_schema', recovery_action: 'Rebuild the analytical cache'
    }, { status: 503 }));
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect(await screen.findByText('The people list needs the analytics cache')).toBeDefined();
    expect(screen.getByRole('main', { name: 'People' })).toBeDefined();
    expect(state.current.workspace).toBe('relationships');
    expect(screen.queryByRole('main', { name: 'Messages' })).toBeNull();

    rendered.unmount();
    state.destroy();
  });

  it('clears the hub detail pane when the URL target becomes null (Esc / Back)', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'relationships', relationshipTarget: 'cluster:1'
    }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path === '/api/v1/participants/1') return Response.json({
        id: 1, display_label: 'Alice Example', partial_label: false, identifiers: [],
        activity_count: 1, file_count: 0, source_counts: [], first_at: '2026-07-19T10:00:00Z',
        last_at: '2026-07-19T10:00:00Z', cache_revision: 'cache-rel'
      });
      if (path === '/api/v1/participants/1/timeline') return Response.json({
        cache_revision: 'cache-rel', rows: [], total_count: 0, search_provenance: {}
      });
      if (path === '/api/v1/relationships') return Response.json({ rows: [] });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect(await screen.findByRole('heading', { name: 'Alice Example' })).toBeDefined();

    state.commitNavigation({ relationshipTarget: null });

    await waitFor(() => expect(screen.queryByRole('heading', { name: 'Alice Example' })).toBeNull());
    expect(screen.getByText('Select a person or domain')).toBeDefined();
    expect(state.current.workspace).toBe('relationships');

    rendered.unmount();
    state.destroy();
  });

  it('keeps a promoted participant saved after Back and reselecting it', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'relationships', relationshipTarget: 'cluster:11'
    }))}`);
    const requests: Request[] = [];
    let promoted = false;
    const savedPerson = { id: 42, revision: 1, display_name: 'Synthetic Candidate', participant_ids: [11], vcard_uid: 'synthetic-42', created_at: '2026-07-19T10:00:00Z', updated_at: '2026-07-19T10:00:00Z' };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const meetingResponse = meetingFixtureResponse(new URL(request.url).pathname);
      if (meetingResponse) return meetingResponse;
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/relationships') return Response.json({ rows: [{
        canonical_id: 11, display_label: 'Synthetic Candidate', member_ids: [11], score: 1, last_at: '2026-07-19T10:00:00Z',
        signals: { last_interaction_at: '2026-07-19T10:00:00Z', sent_count: 1, sent_to_them: 1, received_from_them: 0, meeting_count: 0, meetings_together: 0, modalities: 1 }
      }] });
      if (path === '/api/v1/participants/11') return Response.json({
        id: 11, display_label: 'Synthetic Candidate', partial_label: false, identifiers: [],
        ...(promoted ? { profile: savedPerson } : {}),
        activity_count: 1, file_count: 0, source_counts: [], first_at: '2026-07-19T10:00:00Z',
        last_at: '2026-07-19T10:00:00Z', cache_revision: 'cache-rel'
      });
      if (path === '/api/v1/participants/11/timeline') return Response.json({
        cache_revision: 'cache-rel', rows: [], total_count: 0, search_provenance: {}
      });
      if (path === '/api/v1/people/directory') return Response.json({
        people: promoted ? [{
          id: 42, display_name: 'Synthetic Candidate', revision: 1, primary_channel: 'email',
          contact_state: 'active', categories: [], organizations: []
        }] : []
      });
      if (path === '/api/v1/people' && request.method === 'POST') {
        promoted = true;
        return Response.json(savedPerson, { status: 201 });
      }
      if (path === '/api/v1/people/42') return Response.json(savedPerson);
      if (path === '/api/v1/people/42/profile') return Response.json(emptyProfile(savedPerson));
      if (path === '/api/v1/people/42/attributes') return Response.json({ person_id: 42, attributes: [] });
      if (path === '/api/v1/people/42/network') return Response.json({ root_person_id: 42, depth: 1, truncated: false, nodes: [], edges: [] });
      if (path === '/api/v1/people/42/contact-state') return Response.json({
        person_id: 42, cadence_status: 'ok', computed_at: '2026-07-19T10:00:00Z',
        interaction_count: 1, stale: false
      });
      if (path === '/api/v1/people/42/employments') return Response.json({ employments: [] });
      if (path === '/api/v1/people/42/relationships') return Response.json({ relationships: [] });
      if (path === '/api/v1/people/42/files/search') return Response.json({
        files: [], total_count: 0, cache_revision: 'cache-person', search_provenance: {}
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    expect(await screen.findByRole('heading', { name: 'Synthetic Candidate' })).toBeDefined();
    await fireEvent.click(screen.getByRole('radio', { name: 'Details' }));
    expect(await screen.findByRole('main', { name: 'People' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(state.current.directoryPersonID).toBe(42));
    expect(state.current.workspace).toBe('relationships');
    expect(new URL(window.location.href).searchParams.get('explore')).toContain('directoryPersonID');
    const promotion = requests.find((request) =>
      new URL(request.url).pathname === '/api/v1/people' && request.method === 'POST'
    );
    expect(promotion).toBeDefined();
    await expect(promotion!.clone().json()).resolves.toMatchObject({ participant_id: 11 });
    await screen.findByRole('button', { name: 'More actions' });
    expect(screen.getByRole('radio', { name: 'Details' }).getAttribute('aria-checked')).toBe('true');

    window.history.back();
    await waitFor(() => expect(state.current.directoryPersonID).toBeNull());
    await fireEvent.click(within(screen.getByRole('grid', { name: 'People results' })).getByText('Synthetic Candidate'));
    await screen.findByRole('heading', { name: 'Synthetic Candidate' });
    expect(await screen.findByRole('button', { name: 'More actions' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Save' })).toBeNull();

    rendered.unmount();
    state.destroy();
  });

  it('refetches filtered Saved results when Settings reports a directory change', async () => {
    let renamed = false;
    const directoryReads: URL[] = [];
    const client = createAPIClient(async input => {
      const request = input instanceof Request ? input : new Request(input);
      const url = new URL(request.url);
      const path = url.pathname;
      if (path === '/api/v1/operations/status') return Response.json({ lanes: lanes() });
      if (path === '/api/v1/sources/status') return Response.json({ sources: [] });
      if (path === '/api/v1/people/directory') {
        directoryReads.push(url);
        return Response.json({ people: renamed || url.searchParams.get('organization') !== 'Old Org' ? [] : [{ id: 7, revision: 1, display_name: 'Synthetic Person', organizations: ['Old Org'], categories: [], contact_state: 'active' }] });
      }
      if (path === '/api/v1/saved-views') return Response.json({ saved_views: [] });
      return Response.json({ rows: [], total_count: 0, cache_revision: 'cache', search_provenance: {} });
    });
    const settings = createRawSnippet<Parameters<NonNullable<ComponentProps<typeof AppShell>['settings']>>>((_request, _consumed, _target, _category, _chooseCategory, _sources, _openStatus, _statusChange, _operations, getDirectoryChange) => ({
      render: () => '<main aria-label="Settings fixture"><button>Report directory change</button></main>',
      setup: element => {
        const button = element.querySelector('button')!;
        const report = () => { renamed = true; getDirectoryChange()(); };
        button.addEventListener('click', report);
        return () => button.removeEventListener('click', report);
      }
    }));
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'relationships', relationshipSaved: true, directoryOrganization: 'Old Org' }))}`);
    const state = new ExploreState(window);
    const view = render(AppShell, { client, state, settings });
    try {
      await screen.findByText('Synthetic Person');
      const readsBeforeSettings = directoryReads.length;
      await fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
      await fireEvent.click(await screen.findByRole('button', { name: 'Report directory change' }));
      await waitFor(() => expect(directoryReads).toHaveLength(readsBeforeSettings + 1));
      expect(directoryReads.at(-1)?.searchParams.get('organization')).toBe('Old Org');
    } finally { view.unmount(); state.destroy(); window.history.replaceState(null, '', '/'); }
  });

  it.each(['reader next', 'Filter shortcut'])('supports %s in the person Messages table', async (control) => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'relationships', relationshipTarget: 'cluster:1' }))}`);
    const client = createAPIClient(async input => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/participants/1') return Response.json({
        id: 1, display_label: 'Example Person', identifiers: [], activity_count: 2, file_count: 0,
        source_counts: [], first_at: '2026-07-18T12:00:00Z', last_at: '2026-07-18T12:00:00Z', cache_revision: 'cache-1'
      });
      if (path === '/api/v1/participants/1/timeline') return Response.json(exploreResponse({
        rows: [1, 2].map(id => ({ ...entry(id), conversation_id: id, anchor_message_id: id })), total_count: 2
      }));
      if (path === '/api/v1/relationships/1/calendar') return Response.json({
        participant_id: 1, canonical_id: 1, year: (await request.json()).year, timezone: 'UTC',
        annual: [], days: [], cache_revision: 'cache-1', identity_revision: 1, score_version: 1,
        effective_date: '2026-07-18', scoring_timezone: 'UTC', peak_temperature: 50, peak_year: 2026,
        current: { temperature: 50, rank: 1, population: 1, raw_score: 1, signals: { sent_signal: 1, received_volume: 0, meeting_signal: 0, modalities: 1 } }
      });
      if (/\/conversations\/[12]$/.test(path)) {
        const id = Number(path.split('/').at(-1));
        return Response.json({ id, messages: [{ id, conversation_id: id, subject: `Synthetic subject ${id}`,
          sent_at: '2026-07-18T12:00:00Z', message_type: 'email', from: 'sender@example.com', to: [],
          body: `Reader body ${id}`, snippet: `Reader body ${id}`, attachments: [] }],
          anchor_id: id, total: 1, has_before: false, has_after: false });
      }
      return meetingFixtureResponse(path) ?? Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client, state });
    try {
      const grid = await screen.findByRole('grid', { name: 'Messages results' });
      await within(grid).findByText('Synthetic subject 1');
      if (control === 'reader next') {
        await fireEvent.click(within(grid).getByText('Synthetic subject 1'));
        await screen.findByText('Reader body 1');
        grid.focus();
        await fireEvent.keyDown(grid, { key: 'l' });
        await screen.findByText('Reader body 2');
      } else {
        grid.focus();
        await fireEvent.keyDown(grid, { key: 'f' });
        const dialog = await screen.findByRole('dialog', { name: 'Filter' });
        expect(within(dialog).getByLabelText('From')).toBeDefined();
      }
    } finally { rendered.unmount(); state.destroy(); }
  });

  it('re-opens the hub target when the predicate changes but the target string does not', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'relationships', relationshipTarget: 'cluster:1'
    }))}`);
    const timelineBodies: unknown[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      const meetingResponse = meetingFixtureResponse(path); if (meetingResponse) return meetingResponse;
      if (path === '/api/v1/participants/1') return Response.json({
        id: 1, display_label: 'Alice Example', partial_label: false, identifiers: [],
        activity_count: 1, file_count: 0, source_counts: [], first_at: '2026-07-19T10:00:00Z',
        last_at: '2026-07-19T10:00:00Z', cache_revision: 'cache-rel'
      });
      if (path === '/api/v1/participants/1/timeline') {
        timelineBodies.push(await request.clone().json());
        return Response.json({
          cache_revision: 'cache-rel', rows: [], total_count: 0, search_provenance: {}
        });
      }
      if (path === '/api/v1/relationships') return Response.json({ rows: [] });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    await waitFor(() => expect(timelineBodies).toHaveLength(1));
    expect((timelineBodies[0] as { filters?: unknown[] }).filters).toEqual([]);

    // The target string is unchanged; only the predicate (a filter picked
    // up from elsewhere in the app) changes.
    state.commitNavigation({ filters: [{ dimension: 'source', values: ['1'] }] });

    await waitFor(() => expect(timelineBodies).toHaveLength(2));
    expect((timelineBodies[1] as { filters?: unknown[] }).filters).toEqual([{ dimension: 'source', values: ['1'] }]);

    rendered.unmount();
    state.destroy();
  });

  it('opening a file from the hub\'s embedded Files pane navigates to Everything with the item selected', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
      workspace: 'relationships', relationshipTarget: 'cluster:1', relationshipFiles: true
    }))}`);
    const fileRow = {
      id: 9, key: 'file:9', entry_key: 'message:42', message_id: 42, conversation_id: 70,
      occurred_at: '2026-07-19T10:00:00Z', source_id: 1, source_type: 'gmail',
      source_identifier: 'alice@example.com', containing_title: 'Subject', filename: 'report.pdf',
      mime_type: 'application/pdf', mime_family: 'pdf', size_bytes: 100,
      content_state: 'unverified', content_available: null
    };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      const meetingResponse = meetingFixtureResponse(path);
      if (meetingResponse) return meetingResponse;
      if (path === '/api/v1/participants/1') return Response.json({
        id: 1, display_label: 'Alice Example', partial_label: false, identifiers: [],
        activity_count: 1, file_count: 1, source_counts: [], first_at: '2026-07-19T10:00:00Z',
        last_at: '2026-07-19T10:00:00Z', cache_revision: 'cache-rel'
      });
      if (path === '/api/v1/participants/1/timeline') return Response.json({
        cache_revision: 'cache-rel', rows: [], total_count: 0, search_provenance: {}
      });
      if (path === '/api/v1/participants/1/files/search') return Response.json({
        files: [fileRow], total_count: 1, cache_revision: 'cache-rel', search_provenance: {}
      });
      if (path === '/api/v1/files/9') return Response.json({
        id: 9, filename: 'report.pdf', mime_type: 'application/pdf', content_state: 'local_content',
        message_id: 42, conversation_id: 70
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    const row = (await screen.findByText('report.pdf')).closest('[role="row"]')!;
    await fireEvent.click(row);
    await fireEvent.click(await screen.findByRole('button', { name: 'Open message' }));

    await waitFor(() => expect(state.current.workspace).toBe('everything'));
    expect(state.current.selectedRow).toBe('message:42');

    rendered.unmount();
    state.destroy();
  });

  it('does not mutate Everything state when Escape bubbles up from an empty Relationships hub', async () => {
    window.history.replaceState(null, '', '/?workspace=relationships');
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path === '/api/v1/relationships') return Response.json({ rows: [] });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    await screen.findByRole('main', { name: 'People' });
    expect(state.current.workspace).toBe('relationships');

    // A groupingChain left behind by a prior Everything session: commitWorkspace
    // does not reset it, and Relationships never uses it — a bubbled Escape
    // from the hub must not silently pop it.
    state.replaceTransient({ groupingChain: ['source'], selectedRow: 'message:1' });

    const grid = screen.getByRole('grid', { name: 'People results' });
    grid.focus();
    await fireEvent.keyDown(grid, { key: 'Escape' });

    expect(state.current.groupingChain).toEqual(['source']);
    expect(state.current.selectedRow).toBe('message:1');
    expect(state.current.workspace).toBe('relationships');

    rendered.unmount();
    state.destroy();
  });

  it('debounces filename search typing into one committed state write', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const searchRequests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname === '/api/v1/files/search') {
        searchRequests.push(request);
        return Response.json({ files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.replaceTransient({ workspace: 'files' });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      await fireEvent.click(await screen.findByRole('button', { name: 'Filter' }));
      const input = await screen.findByLabelText('Filter filename');
      await screen.findByRole('grid', { name: 'Files results' });
      const initialRequestCount = searchRequests.length;
      await fireEvent.input(input, { target: { value: 'in' } });
      await fireEvent.input(input, { target: { value: 'invoice' } });
      expect(state.current.fileFilenameQuery).toBe('');
      expect(searchRequests.length).toBe(initialRequestCount);
      await vi.advanceTimersByTimeAsync(250);
      expect(state.current.fileFilenameQuery).toBe('invoice');
      expect(searchRequests.length).toBe(initialRequestCount + 1);
      const lastRequest = searchRequests[searchRequests.length - 1]!;
      await expect(lastRequest.clone().json()).resolves.toMatchObject({ filename_query: 'invoice' });
    } finally {
      rendered.unmount();
      state.destroy();
      vi.useRealTimers();
    }
  });

  it('flushes a pending debounced filename patch before a MIME-filter navigation commits', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const searchRequests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname === '/api/v1/files/search') {
        searchRequests.push(request);
        return Response.json({ files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.replaceTransient({ workspace: 'files' });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      await fireEvent.click(await screen.findByRole('button', { name: 'Filter' }));
      const input = await screen.findByLabelText('Filter filename');
      await screen.findByRole('grid', { name: 'Files results' });
      const pdfs = await screen.findByRole('checkbox', { name: 'PDFs' });
      const initialRequestCount = searchRequests.length;
      // Start a debounced filename-search patch (queued for 250ms) and then,
      // still inside that window, commit a navigation.
      await fireEvent.input(input, { target: { value: 'invoice' } });
      await fireEvent.click(pdfs);
      // The pending patch flushes immediately so the typed text is not lost;
      // the navigation commit applies on top and wins for fileMIMEFamilies.
      expect(state.current.fileFilenameQuery).toBe('invoice');
      expect(state.current.fileMIMEFamilies).toEqual(['pdf']);
      expect(searchRequests.length).toBe(initialRequestCount + 1);
      const lastRequest = searchRequests[searchRequests.length - 1]!;
      await expect(lastRequest.clone().json()).resolves.toMatchObject({
        filename_query: 'invoice', mime_families: ['pdf']
      });

      await vi.advanceTimersByTimeAsync(250);
      // Flushing cleared the pending timer, so nothing fires again later.
      expect(state.current.fileFilenameQuery).toBe('invoice');
      expect(searchRequests.length).toBe(initialRequestCount + 1);
    } finally {
      rendered.unmount();
      state.destroy();
      vi.useRealTimers();
    }
  });

  it('discards a pending debounced filename patch on Back instead of letting it clobber restored state', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const searchRequests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname === '/api/v1/files/search') {
        searchRequests.push(request);
        return Response.json({ files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.commitNavigation({ workspace: 'files' });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    try {
      await fireEvent.click(await screen.findByRole('button', { name: 'Filter' }));
      const input = await screen.findByLabelText('Filter filename');
      await screen.findByRole('grid', { name: 'Files results' });
      const initialRequestCount = searchRequests.length;
      // Queue a debounced filename-search patch, then navigate back before
      // its timer fires. The restored state must not be clobbered later.
      await fireEvent.input(input, { target: { value: 'invoice' } });
      window.history.back();
      await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));

      expect(state.current.fileFilenameQuery).toBe('');
      await vi.advanceTimersByTimeAsync(250);
      expect(state.current.fileFilenameQuery).toBe('');
      expect(searchRequests.length).toBe(initialRequestCount);
    } finally {
      rendered.unmount();
      state.destroy();
      vi.useRealTimers();
    }
  });

  it('drills a Files group into a canonical filter without opening a stale group selection', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/groups')) return Response.json({
        rows: [sourceGroup],
        total_count: 1, cache_revision: 'cache-1', search_provenance: {}
      });
      if (path.endsWith('/files/search')) return Response.json({
        files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {}
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.replaceTransient({
      workspace: 'files', groupingChain: ['source'], fileFilenameQuery: 'invoice', fileMIMEFamilies: ['pdf']
    });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    await screen.findByRole('grid', { name: 'Files grouped by source' });
    await screen.findByText('Example source');
    const groupRequest = requests.find((request) => new URL(request.url).pathname.endsWith('/groups'))!;
    expect(new URL(groupRequest.url).pathname).toBe('/api/v1/files/groups');
    await expect(groupRequest.clone().json()).resolves.toMatchObject({
      filename_query: 'invoice', mime_families: ['pdf'], grouping: ['source']
    });
    await fireEvent.click(screen.getByRole('button', { name: 'Drill into Example source' }));
    await screen.findByRole('grid', { name: 'Files results' });

    expect(state.current.workspace).toBe('files');
    expect(state.current.filters).toEqual([{ dimension: 'source', values: ['7'] }]);
    expect(state.current.groupingChain).toEqual([]);
    expect(state.current.selectedRow).toBeNull();
    rendered.unmount();
    state.destroy();
  });

  it('shows filename and type chips in grouped and ungrouped Files', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const groupRequests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/files/search') return Response.json({ files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} });
      if (path.endsWith('/groups')) {
        groupRequests.push(request);
        return Response.json({
          rows: [sourceGroup],
          total_count: 1, cache_revision: 'cache-1', search_provenance: {}
        });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.replaceTransient({
      workspace: 'files', groupingChain: ['source'], fileFilenameQuery: 'invoice', fileMIMEFamilies: ['pdf', 'image']
    });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });

    await screen.findByText('Example source');
    const chips = screen.getByRole('region', { name: 'Active analytical context' });
    expect(within(chips).getByText('Filename: “invoice”').textContent).toContain('invoice');
    expect(within(chips).getByText('Type: PDFs, Images').textContent).toContain('PDFs');

    await fireEvent.click(screen.getByRole('button', { name: 'Remove filename filter' }));
    await waitFor(() => expect(groupRequests).toHaveLength(2));
    expect(state.current.fileFilenameQuery).toBe('');
    const afterFilename = await groupRequests[1]!.clone().json();
    expect(afterFilename).not.toHaveProperty('filename_query');
    expect(afterFilename).toMatchObject({ mime_families: ['pdf', 'image'] });

    await fireEvent.click(screen.getByRole('button', { name: 'Remove type filter' }));
    await waitFor(() => expect(groupRequests).toHaveLength(3));
    expect(state.current.fileMIMEFamilies).toEqual([]);
    await expect(groupRequests[2]!.clone().json()).resolves.not.toHaveProperty('mime_families');
    expect(screen.queryByText(/^Filename:|^Type:/)).toBeNull();
    state.replaceTransient({ groupingChain: [], fileFilenameQuery: 'invoice', fileMIMEFamilies: ['pdf'] });
    await screen.findByRole('grid', { name: 'Files results' });
    expect(screen.getByText('Filename: “invoice”')).toBeDefined();
    expect(screen.getByText('Type: PDFs')).toBeDefined();
    rendered.unmount();
    state.destroy();
  });

  it('clears a stale Everything sortNotice when the workspace changes', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/files/search') return Response.json({
        files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {}
      });
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    await screen.findByRole('grid', { name: 'Messages results' });

    await fireEvent.keyDown(window, { key: 'r' });
    expect(screen.getByRole('status', { name: 'Sort status' }).textContent)
      .toBe('Messages is always shown newest first.');

    await fireEvent.click(screen.getByRole('button', { name: 'Files' }));
    expect(screen.getByRole('status', { name: 'Sort status' }).textContent)
      .toBe('');
    rendered.unmount();
    state.destroy();
  });

  it('keeps the selected message open when Filter clears its filters', async () => {
    window.history.replaceState(null, '', '/?workspace=everything');
    const state = new ExploreState(window);
    const client = createAPIClient(async () => Response.json(exploreResponse({ rows: [entry(1)], total_count: 1 })));
    state.commitNavigation({ filters: [{ dimension: 'source', values: ['1'] }] });
    const rendered = render(AppShell, { client, state });
    try {
      const grid = await screen.findByRole('grid', { name: 'Messages results' });
      await screen.findByText('Synthetic subject 1');
      grid.focus();
      await fireEvent.keyDown(grid, { key: 'Enter' });
      await screen.findByRole('complementary', { name: 'Reading pane: Synthetic subject 1' });
      await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
      await fireEvent.click(screen.getByRole('button', { name: 'Clear filters' }));
      expect(state.current.filters).toEqual([]);
      expect(state.current.selectedRow).toBe('message:1');
      expect(await screen.findByRole('complementary', { name: 'Reading pane: Synthetic subject 1' })).toBeDefined();
    } finally { rendered.unmount(); state.destroy(); }
  });

  it('focuses the fixed Messages Sort with s and shows its count once', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(exploreResponse({
      rows: [entry(1), entry(2)], total_count: 2
    })));
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    await screen.findByText('Synthetic subject 1');

    expect(screen.getByRole('status', { name: 'Sort status' }).textContent).toBe('');
    const main = screen.getByRole('main', { name: 'Messages' });
    await waitFor(() => expect(within(main).getAllByText('2 items')).toHaveLength(1));
    await fireEvent.keyDown(document.body, { key: 's' });
    const sort = screen.getByRole('combobox', { name: 'Sort: Newest first' });
    expect(document.activeElement).toBe(sort);
    expect(await screen.findByRole('option', { name: 'Other orders aren’t available yet' })).toHaveProperty('disabled', true);
    rendered.unmount();
    state.destroy();
  });

  it('announces the End cap outside Everything in the files-shell grouped workspace', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let groupPostCount = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path !== '/api/v1/files/groups') return Response.json(exploreResponse());
      groupPostCount += 1;
      const page = groupPostCount;
      return Response.json({
        rows: [{ key: String(page), label: `Source ${page}`, count: 1, estimated_bytes: 42, latest_at: '2026-07-18T12:00:00Z' }],
        total_count: 10_000, cache_revision: 'cache-1', search_provenance: {}, next_cursor: `cursor-${page}`
      });
    });
    const state = new ExploreState(window);
    state.replaceTransient({ workspace: 'files', groupingChain: ['source'] });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Files grouped by source' });
    await screen.findByText('Source 1');

    grid.focus();
    await fireEvent.keyDown(grid, { key: 'End' });

    await waitFor(() => expect(groupPostCount).toBe(1 + LOAD_THROUGH_END_MAX_PAGES));
    await waitFor(() =>
      expect(screen.getByRole('status', { name: 'Sort status' }).textContent).toMatch(/press End again to continue/)
    );
    rendered.unmount();
    state.destroy();
  });

  it('clears a stale Files End-cap notice when switching presentation to Everything, but not on mere paging', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    let groupPostCount = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/files/groups') {
        groupPostCount += 1;
        const page = groupPostCount;
        return Response.json({
          rows: [
            { key: `${page}a`, label: `Source ${page}a`, count: 1, estimated_bytes: 42, latest_at: '2026-07-18T12:00:00Z' },
            { key: `${page}b`, label: `Source ${page}b`, count: 1, estimated_bytes: 42, latest_at: '2026-07-18T12:00:00Z' }
          ],
          total_count: 10_000, cache_revision: 'cache-1', search_provenance: {}, next_cursor: `cursor-${page}`
        });
      }
      return Response.json(exploreResponse());
    });
    const state = new ExploreState(window);
    state.replaceTransient({ workspace: 'files', groupingChain: ['source'] });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Files grouped by source' });
    await screen.findByText('Source 1a');

    grid.focus();
    await fireEvent.keyDown(grid, { key: 'End' });
    await waitFor(() => expect(groupPostCount).toBe(1 + LOAD_THROUGH_END_MAX_PAGES));
    await waitFor(() =>
      expect(screen.getByRole('status', { name: 'Sort status' }).textContent).toMatch(/press End again to continue/)
    );

    // Paging/navigating within the same workspace must not clear the notice.
    await fireEvent.keyDown(grid, { key: 'ArrowDown' });
    expect(screen.getByRole('status', { name: 'Sort status' }).textContent).toMatch(/press End again to continue/);

    await chooseSelectOption(screen.getByRole('combobox', { name: /^Show as:/ }), 'Table');
    await waitFor(() => expect(state.current.workspace).toBe('everything'));
    expect(screen.getByRole('status', { name: 'Sort status' }).textContent)
      .toBe('');
    rendered.unmount();
    state.destroy();
  });

  it('shares nested grouping between the context picker and command palette', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    const rendered = render(AppShell, {
      client: createAPIClient(vi.fn()), state, enabled: false
    });

    await fireEvent.keyDown(window, { key: 'g' });
    const picker = screen.getByRole('combobox', { name: /Group by/ });
    expect(document.activeElement).toBe(picker);
    expect(picker.getAttribute('aria-expanded')).toBe('true');
    await fireEvent.click(screen.getByRole('option', { name: 'People' }));
    expect(state.current.groupingChain).toEqual(['participant']);

    await fireEvent.keyDown(window, { key: 'g' });
    await fireEvent.click(screen.getByRole('option', { name: 'Year' }));
    expect(state.current.groupingChain).toEqual(['participant', 'year']);
    expect(screen.getByLabelText('Active analytical context').textContent).toContain('Grouped by Person');
    expect(screen.getByLabelText('Active analytical context').textContent).toContain('Year');

    await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
    const palette = screen.getByRole('dialog', { name: 'Messages commands' });
    expect(palette).toBeDefined();
    await fireEvent.click(within(palette).getByRole('option', { name: /Group by…/ }));
    expect(within(palette).getByRole('option', { name: 'People' }).getAttribute('aria-disabled')).toBe('true');
    const paletteInput = within(palette).getByRole('combobox');
    paletteInput.focus();
    await waitFor(() => expect(appShortcuts.activeScope()).toBe('everything-editable'));
    await fireEvent.input(paletteInput, { target: { value: 'group' } });
    await fireEvent.keyDown(paletteInput, { key: 'Escape' });
    expect(screen.getByRole('dialog', { name: 'Messages commands' })).toBeDefined();
    expect((paletteInput as HTMLInputElement).value).toBe('');
    expect(appShortcuts.activeScope()).toBe('everything-editable');
    await fireEvent.keyDown(paletteInput, { key: 'Escape' });
    await fireEvent.keyDown(paletteInput, { key: 'Escape' });
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Messages commands' })).toBeNull());
    await waitFor(() => expect(appShortcuts.activeScope()).toBe('root'));

    await fireEvent.keyDown(window, { key: 'f' });
    expect(screen.getByRole('button', { name: 'Filter' }).getAttribute('aria-expanded')).toBe('true');
    await fireEvent.keyDown(window, { key: 'r' });
    expect(screen.getByRole('status', { name: 'Sort status' }).textContent)
      .toBe('Sorting isn’t available while grouped.');
    rendered.unmount();
    state.destroy();
  });

  it('selects three rows when Shift+Space is pressed outside the grid', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    const rows = [0, 1, 2].map((index) => entry(index));
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path.endsWith('/explore/preflight')) {
        return Response.json({
          count: 3, deletable_count: 3, estimated_bytes: 30, cache_revision: 'cache-1',
          search_provenance: {}, unavailable_actions: [], action_targets: []
        });
      }
      return Response.json(exploreResponse({ rows, total_count: 3 }));
    });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Messages results' });
    await screen.findByText('Synthetic subject 0');
    grid.focus();
    await fireEvent.keyDown(grid, { key: ' ' });
    await fireEvent.keyDown(grid, { key: 'j' });
    await fireEvent.keyDown(grid, { key: 'j' });
    await waitFor(() => expect(selectionStatus().textContent).toBe('1 selected'));
    grid.blur();
    expect(document.activeElement).toBe(document.body);

    await fireEvent.keyDown(document.body, { key: ' ', shiftKey: true });

    await waitFor(() => expect(selectionStatus().textContent).toBe('3 selected'));
    rendered.unmount();
    state.destroy();
  });

  it('preserves Messages selection on Stay and clears it when a saved view is applied', async () => {
    window.history.replaceState(null, '', '/?workspace=everything');
    const settingsClient = settingsLeaveClient();
    const client = createAPIClient(async (input, init) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path.endsWith('/explore')) return Response.json(exploreResponse({ rows: [entry(0), entry(1)], total_count: 2 }));
      return settingsClient.fetch(input, init);
    });
    const state = new ExploreState(window);
    const rendered = render(AppShell, { client, state, settings: settingsWithLeaveGuard(client) });
    try {
      const grid = await screen.findByRole('grid', { name: 'Messages results' });
      await screen.findByText('Synthetic subject 0');
      grid.focus();
      await fireEvent.keyDown(grid, { key: ' ' });
      expect(selectionStatus().textContent).toBe('1 selected');
      await fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
      await fireEvent.input(await screen.findByLabelText('Stop when idle after'), { target: { value: '15m' } });
      await fireEvent.click(await screen.findByRole('button', { name: 'Budget threads' }));
      const stay = await screen.findByRole('button', { name: 'Stay' });
      await tick();
      expect(document.activeElement).toBe(stay);
      await fireEvent.click(stay);
      expect((screen.getByLabelText('Stop when idle after') as HTMLInputElement).value).toBe('15m');
      await fireEvent.click(screen.getByRole('button', { name: 'Discard' }));
      await fireEvent.click(screen.getByRole('button', { name: 'Messages' }));
      await waitFor(() => expect(selectionStatus().textContent).toBe('1 selected'));
      await fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
      await fireEvent.input(await screen.findByLabelText('Stop when idle after'), { target: { value: '15m' } });
      await fireEvent.click(await screen.findByRole('button', { name: 'Budget threads' }));
      await fireEvent.click(await screen.findByRole('button', { name: 'Discard and leave' }));
      await screen.findByRole('grid', { name: 'Messages results' });
      expect(screen.queryByRole('button', { name: 'Clear selection' })).toBeNull();
    } finally {
      rendered.unmount(); state.destroy();
    }
  });

  it('returns focus to the results grid after Clear selection', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    const fetchFn = vi.fn<typeof fetch>(async () =>
      Response.json(exploreResponse({ rows: [entry(0), entry(1)], total_count: 2 })));
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Messages results' });
    await screen.findByText('Synthetic subject 0');
    grid.focus();
    await fireEvent.keyDown(grid, { key: ' ' });
    await waitFor(() => expect(selectionStatus().textContent).toBe('1 selected'));

    const clear = screen.getByRole('button', { name: 'Clear selection' });
    clear.focus();
    await fireEvent.click(clear);

    await waitFor(() => expect(screen.queryByRole('button', { name: 'Clear selection' })).toBeNull());
    expect(document.activeElement).toBe(screen.getByRole('grid', { name: 'Messages results' }));
    rendered.unmount();
    state.destroy();
  });

  it('opens deletion review from the selection bar like the d shortcut', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    let preflights = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path.endsWith('/explore/preflight')) {
        preflights += 1;
        return Response.json({
          count: 1, deletable_count: 1, estimated_bytes: 10, cache_revision: 'cache-1',
          search_provenance: {}, unavailable_actions: [], action_targets: [],
          operation_token: 'operation-1', expires_at: '2026-07-19T10:05:00Z'
        });
      }
      if (path.endsWith('/deletions')) return Response.json({ manifests: [] });
      return Response.json(exploreResponse({ rows: [entry(1)], total_count: 1 }));
    });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Messages results' });
    await screen.findByText('Synthetic subject 1');
    expect(screen.queryByRole('button', { name: 'Review deletion…' })).toBeNull();
    grid.focus();
    await fireEvent.keyDown(grid, { key: ' ' });
    await waitFor(() => expect(preflights).toBe(1));

    await fireEvent.click(await screen.findByRole('button', { name: 'Review deletion…' }));

    expect(await screen.findByRole('dialog', { name: 'Review deletion' })).toBeDefined();
    expect(state.current.workspace).toBe('everything');
    await waitFor(() => expect(preflights).toBe(2));
    rendered.unmount();
    state.destroy();
  });

  it('selects the visible rows once when plain a is pressed outside the grid', async () => {
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
    const state = new ExploreState(window);
    const rows = [0, 1, 2].map((index) => entry(index));
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path.endsWith('/explore/preflight')) {
        return Response.json({
          count: 3, deletable_count: 3, estimated_bytes: 30, cache_revision: 'cache-1',
          search_provenance: {}, unavailable_actions: [], action_targets: []
        });
      }
      return Response.json(exploreResponse({ rows, total_count: 3 }));
    });
    const rendered = render(AppShell, { client: createAPIClient(fetchFn), state });
    const grid = await screen.findByRole('grid', { name: 'Messages results' });
    await screen.findByText('Synthetic subject 0');
    const gridKeys: string[] = [];
    grid.addEventListener('keydown', (event) => gridKeys.push(event.key));

    await fireEvent.keyDown(document.body, { key: 'a' });

    await waitFor(() => expect(selectionStatus().textContent).toBe('3 selected'));
    expect(gridKeys).toEqual(['A']);

    gridKeys.length = 0;
    grid.focus();
    await fireEvent.keyDown(grid, { key: 'a' });
    expect(gridKeys).toEqual(['a']);
    rendered.unmount();
    state.destroy();
  });

  describe('Escape over an open reading pane', () => {
    const states: ExploreState[] = [];

    afterEach(() => {
      cleanup();
      for (const state of states.splice(0)) state.destroy();
      window.innerWidth = 1024;
    });

    async function openReadingPane(): Promise<ExploreState> {
      window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
      const state = new ExploreState(window);
      states.push(state);
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
        if (path.endsWith('/groups')) return Response.json(exploreResponse({ rows: [], total_count: 0 }));
        if (path.endsWith('/explore/preflight')) {
          return Response.json({
            count: 1, deletable_count: 1, estimated_bytes: 10, cache_revision: 'cache-1', search_provenance: {},
            unavailable_actions: [{ action: 'open_in_source', reason: 'trusted_source_link_unavailable' }],
            action_targets: []
          });
        }
        return Response.json(exploreResponse({ rows: [entry(1), entry(2)], total_count: 2 }));
      });
      render(AppShell, { client: createAPIClient(fetchFn), state });
      const grid = await screen.findByRole('grid', { name: 'Messages results' });
      await screen.findByText('Synthetic subject 1');
      grid.focus();
      await fireEvent.keyDown(grid, { key: 'Enter' });
      await screen.findByRole('complementary', { name: 'Reading pane: Synthetic subject 1' });
      return state;
    }

    async function clickFocused(button: HTMLElement): Promise<void> {
      button.focus();
      await fireEvent.click(button);
    }

    it.each([
      {
        layer: 'Sort',
        open: () => clickFocused(screen.getByRole('combobox', { name: 'Sort: Newest first' })),
        isOpen: () => screen.queryByRole('listbox') !== null
      },
      {
        layer: 'Filter',
        open: () => clickFocused(screen.getByRole('button', { name: 'Filter' })),
        isOpen: () => screen.queryByRole('dialog', { name: 'Filter' }) !== null
      },
      {
        layer: 'Filter', outer: 'grouping',
        openOuter: async () => {
          const state = await openReadingPane();
          await fireEvent.click(screen.getByRole('combobox', { name: 'Group by' }));
          await fireEvent.click(screen.getByRole('option', { name: 'Source' }));
          state.replaceTransient({ selectedRow: null });
          return state;
        },
        isOuterOpen: (state: ExploreState) => state.current.groupingChain.join() === 'source',
        isOuterClosed: (state: ExploreState) => state.current.groupingChain.length === 0,
        open: () => clickFocused(screen.getByRole('button', { name: 'Filter' })),
        isOpen: () => screen.queryByRole('dialog', { name: 'Filter' }) !== null,
      },
      {
        layer: 'Display',
        open: async () => {
          await clickFocused(screen.getByRole('button', { name: 'Display' }));
          await waitFor(() => expect(document.activeElement?.getAttribute('role')).toBe('menuitemradio'));
        },
        isOpen: () => screen.queryByRole('menu', { name: 'Display' }) !== null
      },
      {
        layer: 'Save view',
        open: async () => {
          await clickFocused(screen.getByRole('button', { name: 'Save view…' }));
          screen.getByRole('button', { name: 'Cancel' }).focus();
        },
        isOpen: () => screen.queryByRole('dialog', { name: 'Save view' }) !== null
      }
    ].map(row => ({
      outer: 'reading pane', openOuter: openReadingPane,
      isOuterOpen: (state: ExploreState) => state.current.selectedRow === 'message:1' && screen.queryByRole('complementary', { name: 'Reading pane: Synthetic subject 1' }) !== null,
      isOuterClosed: (state: ExploreState) => state.current.selectedRow === null,
      ...row,
    })))('closes $layer first and leaves $outer for the next Escape', async ({ open, isOpen, openOuter, isOuterOpen, isOuterClosed }) => {
      const state = await openOuter();
      await open();
      expect(isOpen()).toBe(true);

      await fireEvent.keyDown(document.activeElement!, { key: 'Escape' });

      await waitFor(() => expect(isOpen()).toBe(false));
      expect(isOuterOpen(state)).toBe(true);

      await fireEvent.keyDown(document.activeElement ?? document.body, { key: 'Escape' });
      await waitFor(() => expect(isOuterClosed(state)).toBe(true));
    });

    it('closes the reading pane again once the narrow navigation menu is gone', async () => {
      window.innerWidth = 480;
      const state = await openReadingPane();
      const opener = screen.getByRole('button', { name: 'Open navigation' });
      await clickFocused(opener);
      await screen.findByRole('dialog', { name: 'Navigation' });

      await fireEvent.keyDown(document.activeElement!, { key: 'Escape' });
      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Navigation' })).toBeNull());
      expect(state.current.selectedRow).toBe('message:1');
      expect(appShortcuts.activeScope()).toBe('root');

      await fireEvent.keyDown(opener, { key: 'Escape' });
      await waitFor(() => expect(state.current.selectedRow).toBeNull());
    });
  });

});

function selectionStatus(): HTMLElement {
  return screen.getByRole('status', { name: 'Selection status' });
}

it('wires sidebar deletion through pagehide', async () => {
  window.history.replaceState(null, '', '/?workspace=everything');
  const width = window.innerWidth;
  window.innerWidth = 1024;
  const expire = vi.spyOn(ShellData.prototype, 'expireDelete').mockResolvedValue(undefined);
  const client = createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    if (path.endsWith('/saved-views')) return Response.json({ saved_views: [view(3, 'Project notes')] });
    return Response.json(exploreResponse());
  });
  const rendered = render(AppShell, { client });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: 'Project notes actions' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Delete' }));
    window.dispatchEvent(new Event('pagehide'));
    expect(expire).toHaveBeenCalled();
    expect((expire.mock.contexts.at(-1) as ShellData).pending?.id).toBe(3);
  } finally {
    rendered.unmount();
    const expired = expire.mock.calls.length;
    window.dispatchEvent(new Event('pagehide'));
    expect(expire).toHaveBeenCalledTimes(expired);
    expire.mockRestore();
    window.innerWidth = width;
  }
});

it('commits a saved-view deletion after the eight-second undo window', async () => {
  window.history.replaceState(null, '', '/?workspace=everything');
  const width = window.innerWidth;
  window.innerWidth = 1024;
  let deleted = false;
  const deletes: Request[] = [];
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path.endsWith('/saved-views/3') && request.method === 'DELETE') {
      deletes.push(request);
      deleted = true;
      return new Response(null, { status: 204 });
    }
    if (path.endsWith('/saved-views')) return Response.json({ saved_views: deleted ? [] : [view(3, 'Project notes')] });
    return Response.json(exploreResponse());
  });
  const rendered = render(AppShell, { client });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: 'Project notes actions' }));
    vi.useFakeTimers();
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Delete' }));
    await vi.advanceTimersByTimeAsync(7999);
    expect(deletes).toHaveLength(0);
    expect(screen.getByRole('button', { name: 'Undo' })).toBeDefined();
    await vi.advanceTimersByTimeAsync(1);
    expect(deletes).toHaveLength(1);
    expect(screen.queryByRole('button', { name: 'Undo' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Project notes' })).toBeNull();
  } finally {
    rendered.unmount();
    vi.useRealTimers();
    window.innerWidth = width;
  }
});

it('opens CardDAV Past runs with its kind through the Settings context', async () => {
  window.history.replaceState(null, '', '/?workspace=settings');
  const settings = createRawSnippet(() => {
    const open = getContext<() => void>('msgvault:open-carddav-operations');
    return { render: () => '<button type="button">View CardDAV operations</button>', setup: element => {
      element.addEventListener('click', open);
      return () => element.removeEventListener('click', open);
    } };
  });
  const client = createAPIClient(async input => {
    const url = new URL(input instanceof Request ? input.url : String(input));
    if (url.pathname.endsWith('/operations/status')) return Response.json({ lanes: lanes() });
    if (url.pathname.endsWith('/operations/runs')) return Response.json({ runs: [], unavailable_kinds: [], membership_revision: 1 });
    return Response.json(exploreResponse());
  });
  const state = new ExploreState(window);
  const rendered = render(AppShell, { client, state, settings, enabled: false });
  await fireEvent.click(screen.getByRole('button', { name: 'View CardDAV operations' }));
  expect(await screen.findByRole('dialog', { name: 'Past runs' })).toBeDefined();
  expect(state.current.operationKind).toBe('carddav_sync');
  rendered.unmount(); state.destroy();
});

it('preserves selected run filters in the URL and restores detail on remount', async () => {
  window.history.replaceState(null, '', '/?workspace=sources');
  const client = createAPIClient(async input => {
    const url = new URL(input instanceof Request ? input.url : String(input));
    const path = url.pathname;
    if (path.endsWith('/operations/status')) return Response.json({ lanes: lanes() });
    if (path.endsWith('/operations/runs')) return Response.json({ runs: [sourceRun], unavailable_kinds: [], membership_revision: 1 });
    if (path.includes('/operations/runs/')) return Response.json({ ...sourceRun, supported_actions: [] });
    if (path.endsWith('/sources/status')) return Response.json({ sources: [] });
    if (path.endsWith('/saved-views')) return Response.json({ saved_views: [] });
    return Response.json(exploreResponse());
  });
  let state = new ExploreState(window);
  let rendered = render(AppShell, { client, state });
  await fireEvent.click(await screen.findByRole('button', { name: 'Sync history' }));
  await fireEvent.click(await screen.findByRole('button', { name: 'Open Source sync run' }));
  expect(state.current.operationKind).toBe('source_sync');
  expect(state.current.operationRunID).toBe(OPERATION_RUN);
  const sharedURL = window.location.href;
  rendered.unmount(); state.destroy();
  window.history.replaceState(null, '', sharedURL);
  state = new ExploreState(window);
  rendered = render(AppShell, { client, state });
  await screen.findByRole('region', { name: 'Operation run detail' });
  expect(state.current.operationKind).toBe('source_sync');
  rendered.unmount(); state.destroy();
});

it.each([
  { kind: 'source_sync', authority: undefined, status: 'Needs attention',
    lane: undefined, sources: [sourceStatus({ display_name: 'Source sync', scheduler_last_error: 'Sync failed' })],
    action: { role: 'menuitem', name: /Source sync/ }, expected: { workspace: 'sources', operationHistory: false } },
  { kind: 'visual_embedding', authority: 'getVisualAttachmentStatus', status: 'Running',
    lane: jobLane('visual_embedding', { active: { ...jobRun('visual_embedding'), state: 'running', error: undefined, finished_at: undefined } }), sources: [],
    action: { role: 'menuitem', name: /Visual embedding/ },
    expected: { workspace: 'everything', operationHistory: true, operationStatus: 'getVisualAttachmentStatus' } },
  { kind: 'person_sweep', authority: undefined, status: 'Needs attention',
    lane: jobLane('person_sweep'), sources: [],
    action: { role: 'menuitem', name: /Person fact sweep/ },
    expected: { workspace: 'everything', operationHistory: true, operationKind: 'person_sweep', operationRunID: jobRun('person_sweep').id } },
] as const)('opens $kind with one history entry', async ({ kind, authority, status, lane, sources, action, expected }) => {
  window.history.replaceState(null, '', '/?workspace=everything&mode=full_text');
  const run = lane ? jobRun(kind) : undefined;
  const client = createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    if (path.endsWith('/operations/status')) return Response.json({ lanes: lanes(...(lane ? [lane] : [])) });
    if (path.endsWith('/sources/status')) return Response.json({ sources });
    if (run && path.endsWith('/operations/runs')) return Response.json({ runs: [run], unavailable_kinds: [], membership_revision: 1 });
    if (run && lane && path.includes('/operations/runs/')) return Response.json({ ...run, related_status: authority, supported_actions: lane.supported_actions });
    if (path.endsWith('/saved-views')) return Response.json({ saved_views: [] });
    return operationAuthorityResponse(path) ?? Response.json(exploreResponse());
  });
  const state = new ExploreState(window);
  const rendered = render(AppShell, { client, state });
  await fireEvent.click(await screen.findByRole('button', { name: `Sync status: ${status}` }));
  const target = await screen.findByRole(action.role, { name: action.name });
  const push = vi.spyOn(window.history, 'pushState').mockClear();
  await fireEvent.click(target);
  expect(push).toHaveBeenCalledTimes(1);
  expect(state.current).toMatchObject(expected);
  push.mockRestore();
  rendered.unmount(); state.destroy();
});

function settingsLeaveClient() {
  return createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    if (path.endsWith('/settings')) return Response.json({
      groups: [{ id: 'server', label: 'Server' }],
      settings: [{ key: 'server.daemon_idle_timeout', group: 'server', kind: 'string',
        label: 'Idle timeout', value: { string: '20m0s' }, restart_required: false }],
      person_enrichment_providers: [], pending_restart: false,
    }, { headers: { ETag: '"settings-1"' } });
    if (path.endsWith('/operations/runs')) return Response.json({ runs: [sourceRun], unavailable_kinds: [], membership_revision: 1 });
    if (path.includes('/operations/runs/')) return Response.json({ ...sourceRun, related_status: 'listSourceStatus', supported_actions: [] });
    if (path.endsWith('/saved-views')) return Response.json({ saved_views: [view(1, 'Budget threads')] });
    if (path.endsWith('/operations/status')) return Response.json({ lanes: lanes() });
    if (path.endsWith('/sources/status')) return Response.json({ sources: [] });
    return Response.json(exploreResponse());
  });
}

function settingsWithLeaveGuard(client: ReturnType<typeof createAPIClient>) {
  return createRawSnippet<Parameters<NonNullable<ComponentProps<typeof AppShell>['settings']>>>((...args) => {
    const registerLeave = getContext('msgvault:hold-settings-leave');
    return { render: () => '<div></div>', setup: element => {
      const component = mount(SettingsWorkspace, { target: element, props: { client, operationsController: args[8](), category: 'server' },
        context: new Map([['msgvault:hold-settings-leave', registerLeave]]) });
      return () => { void unmount(component); };
    } };
  });
}

it.each(['Stay', 'Discard and leave'])('restores held Back before a saved view replaces the Settings leave choice, then %s', async (choice) => {
  window.history.replaceState(null, '', '/?workspace=files');
  window.history.pushState({ exploreIndex: 3 }, '', '/?workspace=settings');
  const state = new ExploreState(window);
  const client = settingsLeaveClient();
  const rendered = render(AppShell, { client, state, settings: settingsWithLeaveGuard(client) });
  try {
    await fireEvent.input(await screen.findByLabelText('Stop when idle after'), { target: { value: '15m' } });
    const popped = new Promise(resolve => window.addEventListener('popstate', resolve, { once: true }));
    window.history.back();
    await popped;
    await screen.findByRole('button', { name: 'Stay' });
    expect(state.current.workspace).toBe('settings');
    expect(window.location.search).toBe('?workspace=files');

    await fireEvent.click(await screen.findByRole('button', { name: 'Budget threads' }));
    expect(window.location.search).toBe('?workspace=settings&mode=full_text');
    await fireEvent.click(await screen.findByRole('button', { name: choice }));
    if (choice === 'Stay') {
      expect(state.current.workspace).toBe('settings');
      expect(window.location.search).toBe('?workspace=settings&mode=full_text');
      expect(screen.getByLabelText('Stop when idle after')).toHaveProperty('value', '15m');
    } else {
      await screen.findByRole('grid', { name: 'Messages results' });
      expect(state.current.workspace).toBe('everything');
      expect(new URLSearchParams(window.location.search).get('workspace')).toBe('everything');
    }
  } finally {
    rendered.unmount(); state.destroy();
  }
});

it('keeps the Settings leave choice after Past runs closes through Source status', async () => {
  window.history.replaceState(null, '', '/?workspace=sources');
  const state = new ExploreState(window);
  state.commitWorkspace('settings');
  const client = settingsLeaveClient();
  const rendered = render(AppShell, { client, state, settings: settingsWithLeaveGuard(client) });
  try {
    await fireEvent.input(await screen.findByLabelText('Stop when idle after'), { target: { value: '15m' } });
    await fireEvent.click(await screen.findByRole('button', { name: 'Sync status: Idle' }));
    await fireEvent.click(await screen.findByRole('menuitem', { name: /^Past runs/ }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Open Source sync run' }));
    const depart = async () => {
      await fireEvent.click(await screen.findByRole('button', { name: 'Open Sources status' }));
    };
    await depart();
    await waitFor(() => expect(state.current.operationHistory).toBe(false));
    await fireEvent.click(await screen.findByRole('button', { name: 'Stay' }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(state.current.operationHistory).toBe(false);
    expect(state.current.workspace).toBe('settings');
    expect((screen.getByLabelText('Stop when idle after') as HTMLInputElement).value).toBe('15m');
    await fireEvent.click(screen.getByRole('button', { name: 'Sync status: Idle' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: /^Past runs/ }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Open Source sync run' }));
    await depart();
    await waitFor(() => expect(state.current.operationHistory).toBe(false));
    await fireEvent.click(await screen.findByRole('button', { name: 'Discard and leave' }));
    await waitFor(() => expect(state.current.workspace).toBe('sources'));
    expect(state.current.operationHistory).toBe(false);
  } finally {
    rendered.unmount(); state.destroy();
  }
});

it('reloads a failed source job when Sources is already open', async () => {
  window.history.replaceState(null, '', '/?workspace=sources');
  let failed = false;
  let statusReads = 0;
  const client = createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    if (path.endsWith('/sources/status')) {
      statusReads += 1;
      return Response.json({ sources: [sourceStatus(failed ? { scheduler_last_error: 'Scheduled sync failed' } : {})] });
    }
    if (path.endsWith('/operations/status')) return Response.json({ lanes: lanes() });
    if (path.endsWith('/saved-views')) return Response.json({ saved_views: [] });
    return Response.json(exploreResponse());
  });
  const state = new ExploreState(window);
  const rendered = render(AppShell, { client, state });
  try {
    await screen.findByRole('button', { name: 'Sync now Example archive' });
    failed = true;
    await fireEvent.click(await screen.findByRole('button', { name: 'Sync status: Idle' }));
    const job = await screen.findByRole('menuitem', { name: /Example archive/ });
    const readsBeforeOpen = statusReads;
    await fireEvent.click(job);
    await waitFor(() => expect(statusReads).toBeGreaterThan(readsBeforeOpen));
    await fireEvent.click(await screen.findByRole('button', { name: 'Details for Example archive' }));
    await screen.findByText('Scheduled sync failed');
    expect(state.current.workspace).toBe('sources');
  } finally {
    rendered.unmount(); state.destroy();
  }
});

it.each(['sources', 'carddav', 'search'])('opens %s settings from its workspace with the requested authority', async category => {
  window.history.replaceState(null, '', category === 'search' ? '/?workspace=files' : '/?workspace=sources');
  const state = new ExploreState(window);
  state.current.settingsAuthority = category === 'search' ? '' : 'visual_attachments';
  const client = createAPIClient(async input => {
    const path = new URL((input as Request).url).pathname;
    if (path.endsWith('/sources/status')) return Response.json({ sources: [sourceStatus({ source_type: 'beeper', identifier: 'beeper', display_name: 'Example chats' }), sourceStatus({ id: 2, source_type: 'slack', identifier: 'slack', display_name: 'Example Slack' })] });
    if (path.endsWith('/carddav/connections')) return Response.json({ connections: [{ connection: 'work', oauth_app: 'team-app', status: { configured: true, available: true, account: { username: 'contacts@example.com' }, repair_reason: 'google_authorization_required' } }] });
    if (path.endsWith('/files/search')) return Response.json({ files: [], total_count: 0, cache_revision: 'files-1', search_provenance: {} });
    if (path.endsWith('/settings')) return Response.json({ settings: [], groups: [], pending_restart: false });
    return Response.json({});
  });
  const requests: unknown[] = [];
  const settings = createRawSnippet<Parameters<NonNullable<ComponentProps<typeof AppShell>['settings']>>>((getRequest) => ({
    render: () => '<div></div>',
    setup: () => { requests.push(getRequest()); }
  }));
  const visual = new IntegrationReadiness(async () => undefined);
  const view = render(AppShell, { props: { client, state, ...(category === 'carddav' ? { settings: settings as never } : {}) }, context: new Map([[visualReadinessKey, visual]]) });
  try {
    if (category !== 'search') await fireEvent.click(await screen.findByRole('button', { name: category === 'sources' ? 'Details for Example Slack' : 'Details for contacts@example.com' }));
    await fireEvent.click(await screen.findByRole('button', { name: category === 'search' ? 'Set up' : category === 'sources' ? 'Edit in Settings › Sources' : 'Open Settings › Contacts' }));
    await waitFor(() => expect(state.current).toMatchObject({ workspace: 'settings', settingsCategory: category, settingsAuthority: category === 'search' ? 'visual_attachments' : '' }));
    if (category === 'carddav') await waitFor(() => expect(requests).toEqual([expect.objectContaining({ connection: 'work' })]));
  } finally { view.unmount(); visual.dispose(); state.destroy(); }
});

it.each(['', 'invoice'])('clears Files filters and a pending filename edit from %j', async filename => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  window.history.replaceState(null, '', '/?workspace=files');
  const state = new ExploreState(window);
  state.replaceTransient({ fileFilenameQuery: filename, fileMIMEFamilies: ['pdf'], filters: [{ dimension: 'after', values: ['2026-07-01T00:00:00Z'] }] });
  const view = render(AppShell, { client: createAPIClient(async input => {
    const path = new URL((input as Request).url).pathname;
    if (path === '/api/v1/files/search') return Response.json({ files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} });
    if (path === '/api/v1/cli/accounts') return Response.json({ accounts: [] });
    if (path === '/api/v1/deletions') return Response.json({ manifests: [] });
    if (path === '/api/v1/saved-views') return Response.json({ saved_views: [] });
    if (path === '/api/v1/operations/status') return Response.json({ lanes: lanes() });
    if (path === '/api/v1/sources/status') return Response.json({ sources: [] });
    if (path === '/api/v1/explore/groups') return Response.json({ rows: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} });
    return Response.json({}, { status: 404 });
  }), state });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: /^Filter$/ }));
    await fireEvent.input(await screen.findByLabelText('Filter filename'), { target: { value: 'pending' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Clear filters' }));
    await vi.advanceTimersByTimeAsync(300);
    expect(state.current).toMatchObject({ filters: [], fileFilenameQuery: '', fileMIMEFamilies: [] });
    expect((screen.getByLabelText('Filter filename') as HTMLInputElement).value).toBe('');
    expect((screen.getByRole('checkbox', { name: 'PDFs' }) as HTMLInputElement).checked).toBe(false);
  } finally { view.unmount(); state.destroy(); vi.useRealTimers(); }
});

it('opens the surviving person and keeps the merged pair instruction after resolving duplicates', async () => {
  let merged = false;
  const profile = (id: number) => person(id, id === 7 ? 4 : 2, [id === 7 ? 1 : 2]);
  const conflict = { error: 'person_merge_required', message: 'Choose a survivor', profiles: [7, 9].map(id => ({ person: profile(id), etag: id === 7 ? '"person-7-r4"' : '"person-9-r2"' })) };
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const url = new URL(request.url), path = url.pathname;
    if (path.endsWith('/people/directory')) return Response.json({ people: merged ? [directoryPerson(7)] : [directoryPerson(7), directoryPerson(9)] });
    if (path.endsWith('/identity/match-candidates')) return Response.json({ candidates: [{ ...pair, review_token: merged ? 'refreshed-17' : 'review-17', actionable: merged, ...(merged ? {} : { blocker: 'person_merge_required' }) }], limit: Number(url.searchParams.get('limit')), offset: 0 });
    if (path.endsWith('/17/review/accept')) return Response.json(conflict, { status: 409 });
    if (path.endsWith('/people/7/merge')) { merged = true; return Response.json(mergeResult({ ...profile(7), revision: 5, participant_ids: [1, 2] }), { headers: { ETag: '"person-7-r5"' } }); }
    if (/\/people\/(7|9)\/profile$/.test(path)) { const id = Number(path.split('/').at(-2)); if (merged && id === 9) return Response.json({ message: 'Person no longer exists' }, { status: 404 }); return Response.json({ person: profile(id), names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] }); }
    if (/\/people\/(7|9)$/.test(path)) { const id = Number(path.split('/').at(-1)); if (merged && id === 9) return Response.json({ message: 'Person no longer exists' }, { status: 404 }); return Response.json(profile(id), { headers: { ETag: id === 7 ? '"person-7-r4"' : '"person-9-r2"' } }); }
    if (/\/participants\/(1|2)$/.test(path)) { const id = Number(path.split('/').at(-1)); return Response.json({ id, display_label: id === 1 ? 'Synthetic One' : 'Synthetic Two', identifiers: [], activity_count: 3 }); }
    if (path.endsWith('/operations/status')) return Response.json({ lanes: [] });
    if (path.endsWith('/sources/status')) return Response.json({ sources: [] });
    if (path.endsWith('/saved-views')) return Response.json({ saved_views: [] });
    if (path.endsWith('/deletions')) return Response.json({ manifests: [] });
    return Response.json({ rows: [] });
  });
  const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true });
  const view = render(AppShell, { client, state });
  try {
    const grid = await screen.findByRole('grid', { name: 'People results' });
    await fireEvent.click(await within(grid).findByText('Synthetic Two'));
    await waitFor(() => expect(state.current.directoryPersonID).toBe(9));
    await fireEvent.click(within(screen.getByRole('complementary', { name: 'People' })).getByRole('button', { name: 'Filter' }));
    await fireEvent.click(screen.getByText('Possible duplicates', { exact: true }));
    await screen.findAllByRole('button', { name: 'Merge' });
    await fireEvent.click(within(document.getElementById('duplicate-17')!).getByRole('button', { name: 'Merge' }));
    await fireEvent(window, new Event('pagehide'));
    const modal = await screen.findByRole('dialog', { name: 'Resolve person merge' });
    await fireEvent.click(within(modal).getByRole('radio', { name: 'Synthetic One (Person 7)' }));
    await fireEvent.click(within(modal).getByRole('checkbox', { name: /I understand/ }));
    await fireEvent.click(within(modal).getByRole('button', { name: 'Merge into selected survivor' }));
    await waitFor(() => expect(state.current.directoryPersonID).toBe(7));
    await waitFor(() => expect(document.getElementById('duplicate-17')?.textContent).toContain('Choose Merge to record this pair.'));
  } finally { view.unmount(); state.destroy(); }
});

function duplicateReviewArchive(savedViews: ReturnType<typeof view>[] = []) {
  const server = { changed: false, deleted: false, decisions: [] as unknown[] };
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const url = new URL(request.url), path = url.pathname;
    if (path.endsWith('/17/review/reject')) { server.decisions.push(await request.json()); return Response.json({ candidate: { ...pair, state: 'rejected' }, cache_state: 'ready' }); }
    if (path.endsWith('/saved-views/3') && request.method === 'DELETE') { server.deleted = true; return new Response(null, { status: 204 }); }
    if (path.endsWith('/saved-views')) return Response.json({ saved_views: server.deleted ? [] : savedViews });
    if (path.endsWith('/identity/match-candidates')) return Response.json({ candidates: [{ ...pair, review_token: server.changed ? 'fresh-17' : pair.review_token }], limit: Number(url.searchParams.get('limit')), offset: 0 });
    if (/\/participants\/(1|2)$/.test(path)) return Response.json({ id: Number(path.split('/').at(-1)), display_label: 'Synthetic person', identifiers: [], activity_count: 3 });
    if (path.endsWith('/operations/status')) return Response.json({ lanes: [] });
    if (path.endsWith('/sources/status')) return Response.json({ sources: [] });
    if (path.endsWith('/deletions')) return Response.json({ manifests: [] });
    return Response.json(exploreResponse());
  });
  return { client, server };
}

it('keeps duplicate and saved-view Undo actions independent', async () => {
  const { client, server } = duplicateReviewArchive([view(3, 'Project notes')]);
  const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', peopleDuplicates: true });
  const rendered = render(AppShell, { client, state });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: 'Project notes actions' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Delete' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Keep separate' }));
    await waitFor(() => expect(screen.getAllByRole('button', { name: 'Undo' })).toHaveLength(2));
    const duplicateFlash = screen.getByText('Keep separate queued').closest('[role="status"]') as HTMLElement;
    await fireEvent.click(within(duplicateFlash).getByRole('button', { name: 'Undo' }));
    const restored = await screen.findByRole('button', { name: 'Keep separate' });
    expect(server.decisions).toHaveLength(0);
    expect(screen.getAllByRole('button', { name: 'Undo' })).toHaveLength(1);
    expect(restored.closest('li')).toBe(document.activeElement);
    await fireEvent(window, new Event('pagehide'));
    await waitFor(() => expect(server.deleted).toBe(true));
    expect(server.decisions).toHaveLength(0);
  } finally { rendered.unmount(); state.destroy(); }
});

it.each([
  { outcome: 'success', decision: 'reject', action: 'Keep separate' },
  { outcome: 'pagehide', decision: 'reject', action: 'Keep separate' },
  { outcome: '503', decision: 'reject', action: 'Keep separate' },
  { outcome: 'person_merge_required', decision: 'accept', action: 'Merge' },
])('preserves shell focus and recovery when a duplicate settles: $outcome', async ({ outcome, decision, action }) => {
  let finish!: (response: Response) => void;
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const url = new URL(request.url), path = url.pathname;
    if (path.endsWith(`/17/review/${decision}`)) { return new Promise<Response>(resolve => { finish = resolve; }); }
    if (path.endsWith('/identity/match-candidates')) return Response.json({ candidates: [pair], limit: Number(url.searchParams.get('limit')), offset: 0 });
    if (/\/participants\/(1|2)$/.test(path)) return Response.json({ id: Number(path.split('/').at(-1)), display_label: 'Synthetic person', identifiers: [], activity_count: 3 });
    if (path.endsWith('/saved-views')) return Response.json({ saved_views: [] });
    return Response.json(exploreResponse());
  });
  const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', peopleDuplicates: true });
  const rendered = render(AppShell, { client, state });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: action }));
    const undo = await screen.findByRole('button', { name: 'Undo' });
    undo.focus();
    const messages = within(screen.getByRole('navigation', { name: 'Primary' })).getByRole('button', { name: 'Messages' });
    if (outcome === 'pagehide') await fireEvent(window, new Event('pagehide'));
    else {
      messages.focus();
      await fireEvent.click(messages);
      await screen.findByRole('main', { name: 'Messages' });
    }
    await waitFor(() => expect(finish).toBeTypeOf('function'));
    expect(screen.queryByRole('button', { name: 'Undo' })).toBeNull();
    if (outcome === '503') {
      finish(Response.json({ error: 'unavailable', message: 'Decision unavailable' }, { status: 503 }));
      await waitFor(() => expect(document.querySelector('.kit-flash-banner')?.textContent ?? '').toContain('Decision unavailable'));
      const returnAction = document.querySelector<HTMLButtonElement>('.kit-flash-banner__action');
      expect(returnAction?.textContent).toBe('Review duplicates');
      await fireEvent.click(returnAction!);
      await screen.findByRole('heading', { name: 'Possible duplicates' });
      await screen.findByRole('button', { name: 'Keep separate' });
    } else if (outcome === 'person_merge_required') {
      vi.useFakeTimers();
      finish(Response.json({ error: 'person_merge_required', message: 'Choose a survivor', profiles: [7, 9].map(id => ({ person: person(id), etag: id === 7 ? '"person-7-r4"' : '"person-9-r2"' })) }, { status: 409 }));
      await vi.advanceTimersByTimeAsync(0);
      vi.useRealTimers();
      await tick();
      expect(screen.queryByRole('dialog', { name: 'Resolve person merge' })).toBeNull();
      await fireEvent.click(within(screen.getByRole('navigation', { name: 'Primary' })).getByRole('button', { name: /^People(?: |$)/ }));
      await screen.findByRole('dialog', { name: 'Resolve person merge' });
    } else {
      finish(Response.json({ candidate: { ...pair, state: 'rejected' }, cache_state: 'ready' }));
      await waitFor(() => expect(document.activeElement).toBe(outcome === 'pagehide' ? screen.getByRole('radio', { name: 'To review' }) : messages));
    }
  } finally { finish?.(Response.json({ candidate: { ...pair, state: 'rejected' }, cache_state: 'ready' })); rendered.unmount(); state.destroy(); vi.useRealTimers(); }
});

it('refreshes cached duplicate cards after explicit reentry', async () => {
  const { client, server } = duplicateReviewArchive();
  const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', peopleDuplicates: true, identityQuery: 'Synthetic' });
  const rendered = render(AppShell, { client, state });
  try {
    await screen.findByRole('button', { name: 'Keep separate' });
    expect(screen.queryByRole('searchbox', { name: 'Filter people' })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Back to People' }));
    expect((await screen.findByRole('searchbox', { name: 'Filter people' }) as HTMLInputElement).value).toBe('Synthetic');
    server.changed = true;
    await fireEvent.click(within(screen.getByRole('complementary', { name: 'People' })).getByRole('button', { name: 'Filter' }));
    await fireEvent.click(screen.getByText('Possible duplicates', { exact: true }));
    expect(screen.queryByRole('searchbox', { name: 'Filter people' })).toBeNull();
    await fireEvent.click(await screen.findByRole('button', { name: 'Keep separate' }));
    await fireEvent(window, new Event('pagehide'));
    await waitFor(() => expect(server.decisions).toEqual([{ review_token: 'fresh-17' }]));
  } finally { rendered.unmount(); state.destroy(); }
});
