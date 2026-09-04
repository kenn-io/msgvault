import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import FilesWorkspace from './FilesWorkspace.svelte';

function response() {
  return {
    files: [{
      id: 7, key: 'file:7', entry_key: 'message:11', message_id: 11, conversation_id: 21,
      occurred_at: '2026-07-18T12:00:00Z', source_id: 1, source_type: 'synthetic',
      source_identifier: 'archive@example.com', containing_title: 'Containing item',
      filename: 'fixture.pdf', mime_type: 'application/pdf', mime_family: 'pdf', size_bytes: 2048,
      participant_labels: ['Alice Example'], participant_domains: ['example.com'],
      content_state: 'local_content', content_available: true
    }],
    total_count: 1, cache_revision: 'cache-files', search_provenance: {}
  };
}

function personResponse() {
  const result = response();
  return {
    ...result,
    files: result.files.map((file) => ({
      ...file,
      person_provenance: {
        participant_ids: [42],
        roles: ['from', 'conversation_member'],
        directions: ['from_person', 'group']
      }
    }))
  };
}

function installResizeObservers() {
  const records: Array<{ observed: Element[]; disconnected: boolean }> = [];
  class ResizeObserverStub {
    private record = { observed: [] as Element[], disconnected: false };
    constructor(_callback: ResizeObserverCallback) { records.push(this.record); }
    observe(target: Element): void { this.record.observed.push(target); }
    unobserve(): void {}
    disconnect(): void { this.record.disconnected = true; }
  }
  vi.stubGlobal('ResizeObserver', ResizeObserverStub);
  return records;
}

describe('FilesWorkspace', () => {
  it.each([
    {
      state: 'loading',
      fetchFn: vi.fn<typeof fetch>(() => new Promise<Response>(() => {})),
      expected: null,
      rendered: 'Loading files…'
    },
    {
      state: 'empty',
      fetchFn: vi.fn<typeof fetch>(async () => Response.json({
        files: [], total_count: 0, cache_revision: 'empty', search_provenance: {}
      })),
      expected: '2',
      rendered: 'No files match this view.'
    },
    {
      state: 'error',
      fetchFn: vi.fn<typeof fetch>(async () => Response.json(
        { error: 'internal_error', message: 'Synthetic file failure.' }, { status: 500 }
      )),
      expected: null,
      rendered: 'Synthetic file failure.'
    },
    {
      state: 'degraded',
      fetchFn: vi.fn<typeof fetch>(async () => Response.json({
        error: 'analytical_cache_unavailable', message: 'Synthetic cache unavailable.',
        readiness: 'absent', recovery_action: 'msgvault build-cache'
      }, { status: 503 })),
      expected: null,
      rendered: 'Synthetic cache unavailable.'
    }
  ])('reports an honest row count for the $state state', async ({ fetchFn, expected, rendered }) => {
    render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' }
    });

    const grid = await screen.findByRole('grid', { name: 'Files results' });
    await screen.findByText(rendered);
    expect(grid.getAttribute('aria-rowcount')).toBe(expected);
  });

  it('owns headers and virtual rows in one focusable grid', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(response()));
    render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' }
    });

    const grid = await screen.findByRole('grid', { name: 'Files results' });
    expect(screen.getAllByRole('grid')).toHaveLength(1);
    expect(grid.contains(screen.getByRole('columnheader', { name: 'Filename' }))).toBe(true);
    expect(grid.contains(await screen.findByRole('row', { name: /fixture.pdf/ }))).toBe(true);
  });

  it('retries a building analytical cache and renders files when it becomes ready', async () => {
    vi.useFakeTimers();
    let calls = 0;
    const fetchFn = vi.fn<typeof fetch>(async () => {
      calls += 1;
      if (calls === 1) return Response.json({
        error: 'analytical_cache_unavailable', message: 'The analytical cache is being prepared',
        readiness: 'building', recovery_action: ''
      }, { status: 503 });
      return Response.json(response());
    });
    const rendered = render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' }
    });

    expect(await screen.findByText('Preparing analytical cache…')).toBeDefined();
    expect(screen.queryByText('Analytical cache unavailable')).toBeNull();
    await vi.advanceTimersByTimeAsync(1_000);
    expect(await screen.findByText('fixture.pdf')).toBeDefined();
    expect(calls).toBe(2);

    rendered.unmount();
    vi.useRealTimers();
  });

  it('preserves a later-page file restoration across initial cache preparation', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const first = response();
    first.files[0] = { ...first.files[0]!, id: 1, key: 'file:1', filename: 'first.pdf' };
    Object.assign(first, { total_count: 2, next_cursor: 'page-2' });
    const second = response();
    second.files[0] = { ...second.files[0]!, id: 900, key: 'file:900', filename: 'deep.pdf' };
    second.total_count = 2;
    let initialCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url, document.baseURI).pathname;
      if (path === '/api/v1/files/900') return Response.json({
        id: 900, message_id: 11, conversation_id: 21, filename: 'deep.pdf', mime_type: 'application/pdf',
        size_bytes: 2048, content_state: 'missing_blob', content_available: false
      });
      const body = await request.clone().json() as { cursor?: string };
      if (body.cursor === 'page-2') return Response.json(second);
      initialCalls += 1;
      if (initialCalls === 1) return Response.json({
        error: 'analytical_cache_unavailable', message: 'The analytical cache is being prepared',
        readiness: 'building', recovery_action: ''
      }, { status: 503 });
      return Response.json(first);
    });
    const onRestorationComplete = vi.fn();
    const rendered = render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' }, selectedKey: 'file:900',
      activeKey: 'file:900', restorationEpoch: 11, onRestorationComplete
    });
    try {
      expect(await screen.findByText('Preparing analytical cache…')).toBeDefined();
      expect(onRestorationComplete).not.toHaveBeenCalled();

      await vi.advanceTimersByTimeAsync(1_000);
      await waitFor(() => expect(onRestorationComplete).toHaveBeenCalledWith(11));
      expect(await screen.findByRole('dialog', { name: 'View deep.pdf' })).toBeDefined();
      expect(initialCalls).toBe(2);
    } finally {
      rendered.unmount();
      vi.useRealTimers();
    }
  });

  it('cancels cache-readiness polling when the Files workspace is destroyed', async () => {
    vi.useFakeTimers();
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      error: 'analytical_cache_unavailable', message: 'The analytical cache is being prepared',
      readiness: 'building', recovery_action: ''
    }, { status: 503 }));
    const rendered = render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' }
    });
    await Promise.resolve();
    await Promise.resolve();
    expect(fetchFn).toHaveBeenCalledOnce();

    rendered.unmount();
    await vi.advanceTimersByTimeAsync(1_000);
    expect(fetchFn).toHaveBeenCalledOnce();
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it('requests a bounded canonical page and commits stable sortable headers', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return Response.json(response());
    });
    const onSortChange = vi.fn();
    render(FilesWorkspace, {
      client: createAPIClient(fetchFn),
      predicate: {
        query: 'quarterly', search_mode: 'full_text',
        filters: [{ dimension: 'participant', values: ['42'] }], presentation: 'table'
      },
      sort: { field: 'occurred_at', direction: 'desc' },
      filenameQuery: 'invoice',
      mimeFamilies: ['pdf'],
      onSortChange
    });

    expect(await screen.findByText('fixture.pdf')).toBeDefined();
    await expect(requests[0]!.clone().json()).resolves.toMatchObject({
      predicate: { query: 'quarterly', search_mode: 'full_text', filters: [{ dimension: 'participant', values: ['42'] }] },
      sort: { field: 'occurred_at', direction: 'desc' }, limit: 500,
      filename_query: 'invoice', mime_families: ['pdf']
    });
    await fireEvent.click(screen.getByRole('button', { name: 'Sort by date' }));
    expect(onSortChange).toHaveBeenCalledWith({ field: 'occurred_at', direction: 'asc' });
    await fireEvent.click(screen.getByRole('button', { name: 'Sort by filename' }));
    expect(onSortChange).toHaveBeenCalledWith({ field: 'filename', direction: 'asc' });
    await fireEvent.click(screen.getByRole('button', { name: 'Sort by size' }));
    expect(onSortChange).toHaveBeenCalledWith({ field: 'size', direction: 'asc' });
  });

  it('defaults a person Files view to incoming non-visual files and renders exact provenance', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return Response.json(personResponse());
    });
    render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      identityScope: { kind: 'person', id: 1 },
      sort: { field: 'occurred_at', direction: 'desc' }
    });

    expect(await screen.findByText('fixture.pdf')).toBeDefined();
    expect(new URL(requests[0]!.url).pathname).toBe('/api/v1/participants/1/files/search');
    await expect(requests[0]!.clone().json()).resolves.toMatchObject({
      directions: ['from_person'],
      mime_families: ['pdf', 'audio', 'text', 'document', 'archive', 'other']
    });
    expect(screen.getByRole('radio', { name: 'Files' }).getAttribute('aria-checked')).toBe('true');
    expect(screen.getByText('From them · Group conversation')).toBeDefined();
  });

  it('keeps durable-person file searches separate from analytical participant searches', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return Response.json(personResponse());
    });
    render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      // The production union gains this explicit durable-person scope. The
      // cast lets this regression prove the current fallback is unsafe.
      identityScope: { kind: 'durable-person', id: 7 } as never,
      sort: { field: 'occurred_at', direction: 'desc' }
    });

    await screen.findByText('fixture.pdf');
    expect(new URL(requests[0]!.url).pathname).toBe('/api/v1/people/7/files/search');
    expect(requests.some((request) => new URL(request.url).pathname === '/api/v1/participants/7/files/search')).toBe(false);
    expect(requests.some((request) => new URL(request.url).pathname === '/api/v1/files/search')).toBe(false);
  });

  it('updates the person direction union without exposing controls outside person scope', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return Response.json(personResponse());
    });
    const view = render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      identityScope: { kind: 'person', id: 1 },
      sort: { field: 'occurred_at', direction: 'desc' }
    });
    await screen.findByText('fixture.pdf');

    await fireEvent.click(screen.getByRole('checkbox', { name: 'To them' }));
    await waitFor(() => expect(requests).toHaveLength(2));
    await expect(requests[1]!.clone().json()).resolves.toMatchObject({
      directions: ['from_person', 'to_person']
    });
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Group conversations' }));
    await waitFor(() => expect(requests).toHaveLength(3));
    await expect(requests[2]!.clone().json()).resolves.toMatchObject({
      directions: ['from_person', 'to_person', 'group']
    });

    await view.rerender({ identityScope: { kind: 'domain', domain: 'example.com' } });
    await waitFor(() => expect(screen.queryByRole('checkbox', { name: 'To them' })).toBeNull());
    expect(screen.queryByRole('radio', { name: 'Files' })).toBeNull();
  });

  it('switches a person to bounded image/video media cards and reuses the file viewer', async () => {
    const searchRequests: Request[] = [];
    const mediaResult = personResponse();
    mediaResult.files[0] = {
      ...mediaResult.files[0]!, id: 8, key: 'file:8', filename: 'photo.png',
      mime_type: 'image/png', mime_family: 'image', content_state: 'metadata_only',
      content_available: false
    };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/files/8') return Response.json({
        id: 8, message_id: 11, conversation_id: 21, entry_key: 'message:11',
        filename: 'photo.png', mime_type: 'image/png', size_bytes: 8,
        content_state: 'metadata_only', content_available: false
      });
      searchRequests.push(request);
      const body = await request.clone().json() as { mime_families?: string[] };
      return Response.json(body.mime_families?.includes('image') ? mediaResult : personResponse());
    });
    render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      identityScope: { kind: 'person', id: 1 },
      sort: { field: 'occurred_at', direction: 'desc' }
    });
    await screen.findByText('fixture.pdf');

    await fireEvent.click(screen.getByRole('radio', { name: 'Media' }));
    expect(await screen.findByRole('button', { name: 'Open photo.png' })).toBeDefined();
    await expect(searchRequests[1]!.clone().json()).resolves.toMatchObject({
      directions: ['from_person'], mime_families: ['image', 'video']
    });
    expect(screen.getByText('archive@example.com')).toBeDefined();
    expect(screen.getByText('From them · Group conversation')).toBeDefined();

    await fireEvent.click(screen.getByRole('button', { name: 'Open photo.png' }));
    expect(await screen.findByRole('dialog', { name: 'View photo.png' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Open containing item' })).toBeDefined();
  });

  it('observes the grid and header when a Media view becomes Files', async () => {
    const records = installResizeObservers();
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(personResponse()));
    const view = render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      identityScope: { kind: 'person', id: 1 }, personPresentation: 'media',
      sort: { field: 'occurred_at', direction: 'desc' }
    });

    await screen.findByText('fixture.pdf');
    expect(records).toHaveLength(0);

    await view.rerender({ personPresentation: 'files' });

    const grid = await screen.findByRole('grid', { name: 'Files results' });
    const header = grid.querySelector('.table-header');
    expect(header).not.toBeNull();
    expect(records).toHaveLength(1);
    expect(records[0]!.observed).toEqual([grid, header]);
  });

  it('disconnects the old observer and observes replacement elements across a Files transition', async () => {
    const records = installResizeObservers();
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(personResponse()));
    const view = render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      identityScope: { kind: 'person', id: 1 }, personPresentation: 'files',
      sort: { field: 'occurred_at', direction: 'desc' }
    });

    const initialGrid = await screen.findByRole('grid', { name: 'Files results' });
    const initialHeader = initialGrid.querySelector('.table-header');
    expect(initialHeader).not.toBeNull();
    expect(records).toHaveLength(1);
    expect(records[0]!.observed).toEqual([initialGrid, initialHeader]);

    await view.rerender({ personPresentation: 'media' });
    await screen.findByRole('radio', { name: 'Media' });
    expect(screen.queryByRole('grid', { name: 'Files results' })).toBeNull();
    expect(records[0]!.disconnected).toBe(true);

    await view.rerender({ personPresentation: 'files' });

    const replacementGrid = await screen.findByRole('grid', { name: 'Files results' });
    const replacementHeader = replacementGrid.querySelector('.table-header');
    expect(replacementHeader).not.toBeNull();
    expect(replacementGrid).not.toBe(initialGrid);
    expect(replacementHeader).not.toBe(initialHeader);
    expect(records).toHaveLength(2);
    expect(records[1]!.observed).toEqual([replacementGrid, replacementHeader]);
  });

  it('restarts a media gallery after a terminal cursor failure', async () => {
    const first = personResponse();
    first.files[0] = {
      ...first.files[0]!, filename: 'first.png', mime_type: 'image/png', mime_family: 'image',
      content_state: 'metadata_only', content_available: false
    };
    Object.assign(first, { total_count: 2, next_cursor: 'dead-page' });
    const reloaded = personResponse();
    reloaded.files = [
      first.files[0]!,
      { ...first.files[0]!, id: 8, key: 'file:8', filename: 'recovered.png' }
    ];
    reloaded.total_count = 2;
    let initialCalls = 0;
    let cursorCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const body = await request.clone().json() as { cursor?: string };
      if (body.cursor) {
        cursorCalls += 1;
        return Response.json(
          { error: 'archive_revision_changed', message: 'Results changed under this cursor.' },
          { status: 409 }
        );
      }
      initialCalls += 1;
      return Response.json(initialCalls === 1 ? first : reloaded);
    });
    render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      identityScope: { kind: 'person', id: 1 }, personPresentation: 'media',
      sort: { field: 'occurred_at', direction: 'desc' }
    });

    expect(await screen.findByRole('button', { name: 'Open first.png' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Load more media' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Results changed under this cursor.');
    expect(screen.queryByRole('button', { name: 'Load more media' })).toBeNull();

    await fireEvent.click(screen.getByRole('button', { name: 'Reload media' }));
    expect(await screen.findByRole('button', { name: 'Open recovered.png' })).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(initialCalls).toBe(2);
    expect(cursorCalls).toBe(1);
  });

  it('does not open the active file when Enter activates a focused sort control', async () => {
    const onSortChange = vi.fn();
    const onSelectedKey = vi.fn();
    render(FilesWorkspace, {
      client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json(response()))),
      predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' },
      onSortChange,
      onSelectedKey
    });

    await screen.findByText('fixture.pdf');
    const sort = screen.getByRole('button', { name: 'Sort by date' });
    sort.focus();
    await fireEvent.keyDown(sort, { key: 'Enter' });

    expect(onSelectedKey).not.toHaveBeenCalled();
    expect(screen.queryByRole('dialog')).toBeNull();
    await fireEvent.click(sort);
    expect(onSortChange).toHaveBeenCalledOnce();
  });

  it('loads bounded cursor pages until a durable selected file is restored', async () => {
    document.documentElement.style.removeProperty('--row-height');
    const frames = new Map<number, FrameRequestCallback>();
    let nextFrame = 1;
    vi.stubGlobal('requestAnimationFrame', vi.fn((callback: FrameRequestCallback) => {
      const frame = nextFrame++;
      frames.set(frame, callback);
      return frame;
    }));
    vi.stubGlobal('cancelAnimationFrame', vi.fn((frame: number) => frames.delete(frame)));
    const requests: Request[] = [];
    const first = response();
    first.files[0]!.key = 'file:1';
    first.files[0]!.id = 1;
    first.files[0]!.filename = 'first.pdf';
    first.total_count = 50_000;
    Object.assign(first, { next_cursor: 'page-2' });
    const second = response();
    second.files[0]!.key = 'file:900';
    second.files[0]!.id = 900;
    second.files[0]!.filename = 'deep.pdf';
    second.total_count = 50_000;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url, document.baseURI).pathname;
      if (path === '/api/v1/files/900') return Response.json({
        id: 900, message_id: 11, conversation_id: 21, filename: 'deep.pdf', mime_type: 'application/pdf',
        size_bytes: 2048, content_state: 'missing_blob', content_available: false
      });
      const body = await request.clone().json() as { cursor?: string };
      return Response.json(body.cursor ? second : first);
    });
    const onRestorationComplete = vi.fn();

    const { container } = render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' }, selectedKey: 'file:900',
      activeKey: 'file:900', restorationEpoch: 7, onRestorationComplete
    });

    expect(await screen.findByRole('dialog', { name: 'View deep.pdf' })).toBeDefined();
    expect(requests.filter((request) => new URL(request.url).pathname === '/api/v1/files/search')).toHaveLength(2);
    const grid = screen.getByRole('grid', { name: 'Files results' });
    expect(await screen.findByText('Preparing files layout…')).toBeDefined();
    expect(onRestorationComplete).not.toHaveBeenCalled();
    expect(grid.getAttribute('aria-activedescendant')).toBeNull();

    document.documentElement.style.setProperty('--row-height', '36px');
    const [frame, callback] = [...frames.entries()][0]!;
    frames.delete(frame);
    callback(performance.now());

    await waitFor(() => expect(onRestorationComplete).toHaveBeenCalledWith(7));
    expect(grid.getAttribute('aria-activedescendant')).toBe('file-row-900');
    expect(container.querySelector('#file-row-900')).not.toBeNull();
    expect(screen.getAllByRole('row')).toHaveLength(3);
  });

  it('opens a file from keyboard focus and preserves containing navigation authority', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input), document.baseURI).pathname;
      if (path === '/api/v1/files/search') return Response.json(response());
      return Response.json({
        id: 7, message_id: 11, conversation_id: 21, filename: 'fixture.pdf', mime_type: 'application/pdf',
        size_bytes: 2048, content_state: 'missing_blob', content_available: false
      });
    });
    const onOpenItem = vi.fn();
    const onOpenConversation = vi.fn();
    const onSelectedKey = vi.fn();
    render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' }, onOpenItem, onOpenConversation, onSelectedKey
    });

    const grid = await screen.findByRole('grid', { name: 'Files results' });
    await screen.findByRole('row', { name: /fixture.pdf/ });
    grid.focus();
    await fireEvent.keyDown(grid, { key: 'Enter' });
    expect(onSelectedKey).toHaveBeenCalledWith('file:7');
    expect(await screen.findByRole('dialog', { name: 'View fixture.pdf' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Open containing item' }));
    expect(onOpenItem).toHaveBeenCalledWith('message:11');
    await fireEvent.click(screen.getByRole('button', { name: 'Open containing conversation' }));
    expect(onOpenConversation).toHaveBeenCalledWith('message:11', 11, 21);
    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(2));
  });

  it('closes the viewer when the controlled selection is cleared by history navigation', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input), document.baseURI).pathname;
      if (path === '/api/v1/files/search') return Response.json(response());
      return Response.json({
        id: 7, message_id: 11, conversation_id: 21, filename: 'fixture.pdf', mime_type: 'application/pdf',
        size_bytes: 2048, content_state: 'missing_blob', content_available: false
      });
    });
    const view = render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' }, selectedKey: 'file:7'
    });
    expect(await screen.findByRole('dialog', { name: 'View fixture.pdf' })).toBeDefined();

    await view.rerender({ selectedKey: null });

    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  });

  it('clears a stale viewer and shows a pending state when history selects an unloaded file', async () => {
    const first = response();
    Object.assign(first, { total_count: 2, next_cursor: 'page-2' });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input), document.baseURI).pathname;
      if (path === '/api/v1/files/7') return Response.json({
        id: 7, message_id: 11, conversation_id: 21, filename: 'fixture.pdf', mime_type: 'application/pdf',
        size_bytes: 2048, content_state: 'missing_blob', content_available: false
      });
      return Response.json(first);
    });
    const view = render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' }, selectedKey: 'file:7'
    });
    expect(await screen.findByRole('dialog', { name: 'View fixture.pdf' })).toBeDefined();

    await view.rerender({ selectedKey: 'file:900' });

    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(screen.getByText('Opening file…')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Open containing item' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Download fixture.pdf' })).toBeNull();
  });

  it('resolves the pending viewer when the requested file arrives on a later page', async () => {
    const first = response();
    Object.assign(first, { total_count: 2, next_cursor: 'page-2' });
    const second = response();
    second.files[0] = { ...second.files[0]!, id: 900, key: 'file:900', filename: 'deep.pdf' };
    second.total_count = 2;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url, document.baseURI).pathname;
      if (path === '/api/v1/files/7' || path === '/api/v1/files/900') return Response.json({
        id: path.endsWith('900') ? 900 : 7, message_id: 11, conversation_id: 21,
        filename: path.endsWith('900') ? 'deep.pdf' : 'fixture.pdf', mime_type: 'application/pdf',
        size_bytes: 2048, content_state: 'missing_blob', content_available: false
      });
      const body = await request.clone().json() as { cursor?: string };
      return Response.json(body.cursor ? second : first);
    });
    const view = render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' }, selectedKey: 'file:7'
    });
    expect(await screen.findByRole('dialog', { name: 'View fixture.pdf' })).toBeDefined();

    await view.rerender({ selectedKey: 'file:900' });
    await screen.findByText('Opening file…');

    await fireEvent.scroll(screen.getByRole('grid', { name: 'Files results' }));

    expect(await screen.findByRole('dialog', { name: 'View deep.pdf' })).toBeDefined();
    expect(screen.queryByText('Opening file…')).toBeNull();
    expect(screen.queryByText('The selected file is not in the current results.')).toBeNull();
  });

  it('closes a locally opened viewer when the predicate changes and the refreshed listing excludes the file', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url, document.baseURI).pathname;
      if (path === '/api/v1/files/7') return Response.json({
        id: 7, message_id: 11, conversation_id: 21, filename: 'fixture.pdf', mime_type: 'application/pdf',
        size_bytes: 2048, content_state: 'missing_blob', content_available: false
      });
      const body = await request.clone().json() as { predicate?: { query?: string } };
      return Response.json(body.predicate?.query === 'other'
        ? { files: [], total_count: 0, cache_revision: 'cache-other', search_provenance: {} }
        : response());
    });
    const view = render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' }
    });
    const grid = await screen.findByRole('grid', { name: 'Files results' });
    await screen.findByRole('row', { name: /fixture.pdf/ });
    grid.focus();
    await fireEvent.keyDown(grid, { key: 'Enter' });
    expect(await screen.findByRole('dialog', { name: 'View fixture.pdf' })).toBeDefined();

    await view.rerender({
      predicate: { query: 'other', search_mode: 'full_text', filters: [], presentation: 'table' }
    });

    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  });

  it('re-resolves an unchanged controlled selection against a changed predicate instead of keeping the stale viewer', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url, document.baseURI).pathname;
      if (path === '/api/v1/files/7') return Response.json({
        id: 7, message_id: 11, conversation_id: 21, filename: 'fixture.pdf', mime_type: 'application/pdf',
        size_bytes: 2048, content_state: 'missing_blob', content_available: false
      });
      const body = await request.clone().json() as { predicate?: { query?: string } };
      return Response.json(body.predicate?.query === 'other'
        ? { files: [], total_count: 0, cache_revision: 'cache-other', search_provenance: {} }
        : response());
    });
    const view = render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' }, selectedKey: 'file:7'
    });
    expect(await screen.findByRole('dialog', { name: 'View fixture.pdf' })).toBeDefined();

    // selectedKey stays 'file:7' while the context underneath it changes;
    // the refreshed listing excludes the file, so the viewer must close and
    // the selection settle as missing rather than keep the stale row.
    await view.rerender({
      predicate: { query: 'other', search_mode: 'full_text', filters: [], presentation: 'table' }
    });

    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('The selected file is not in the current results.');
  });

  it('shows a missing state when the listing settles without the selected file', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input), document.baseURI).pathname;
      if (path === '/api/v1/files/7') return Response.json({
        id: 7, message_id: 11, conversation_id: 21, filename: 'fixture.pdf', mime_type: 'application/pdf',
        size_bytes: 2048, content_state: 'missing_blob', content_available: false
      });
      return Response.json(response());
    });
    const view = render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' }, selectedKey: 'file:7'
    });
    expect(await screen.findByRole('dialog', { name: 'View fixture.pdf' })).toBeDefined();

    await view.rerender({ selectedKey: 'file:900' });

    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('The selected file is not in the current results.');
    expect(screen.queryByText('Opening file…')).toBeNull();
  });

  it('restores ascending date sort and toggles it back to descending', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(response()));
    const onSortChange = vi.fn();
    render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'asc' }, onSortChange
    });
    await screen.findByText('fixture.pdf');

    await fireEvent.click(screen.getByRole('button', { name: 'Sort by date' }));

    expect(onSortChange).toHaveBeenCalledWith({ field: 'occurred_at', direction: 'desc' });
    await expect((fetchFn.mock.calls[0]![0] as Request).clone().json()).resolves.toMatchObject({
      sort: { field: 'occurred_at', direction: 'asc' }
    });
  });

  it('retries a cursor after a transient network failure', async () => {
    const first = response();
    Object.assign(first, { total_count: 2, next_cursor: 'page-2' });
    const second = response();
    second.files[0] = { ...second.files[0]!, id: 8, key: 'file:8', filename: 'recovered.pdf' };
    second.total_count = 2;
    let cursorCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const body = await request.clone().json() as { cursor?: string };
      if (!body.cursor) return Response.json(first);
      cursorCalls += 1;
      if (cursorCalls === 1) throw new TypeError('temporary network failure');
      return Response.json(second);
    });
    render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' }
    });
    const grid = await screen.findByRole('grid', { name: 'Files results' });
    await screen.findByRole('row', { name: /fixture.pdf/ });
    await fireEvent.scroll(grid);
    expect((await screen.findByRole('alert')).textContent).toContain('temporary network failure');
    expect(screen.getByRole('row', { name: /fixture.pdf/ })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry loading more files' }));
    expect(await screen.findByText('recovered.pdf')).toBeDefined();
    expect(cursorCalls).toBe(2);
  });

  it('keeps loaded rows and offers retry when a cursor page returns 503', async () => {
    const first = response();
    Object.assign(first, { total_count: 2, next_cursor: 'page-2' });
    const second = response();
    second.files[0] = { ...second.files[0]!, id: 8, key: 'file:8', filename: 'recovered.pdf' };
    second.total_count = 2;
    let cursorCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const body = await request.clone().json() as { cursor?: string };
      if (!body.cursor) return Response.json(first);
      cursorCalls += 1;
      if (cursorCalls === 1) return Response.json({
        error: 'analytical_cache_unavailable', message: 'Synthetic cursor cache outage.',
        readiness: 'absent', recovery_action: 'msgvault build-cache'
      }, { status: 503 });
      return Response.json(second);
    });
    render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' }
    });
    const grid = await screen.findByRole('grid', { name: 'Files results' });
    await screen.findByRole('row', { name: /fixture.pdf/ });
    await fireEvent.scroll(grid);

    expect((await screen.findByRole('alert')).textContent).toContain('Synthetic cursor cache outage.');
    expect(screen.getByRole('row', { name: /fixture.pdf/ })).toBeDefined();
    expect(screen.queryByText('Analytical cache unavailable')).toBeNull();

    await fireEvent.click(screen.getByRole('button', { name: 'Retry loading more files' }));
    expect(await screen.findByText('recovered.pdf')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(cursorCalls).toBe(2);
  });

  it.each(['archive_revision_changed', 'search_revision_changed'])(
    'clears the cursor and offers reload when a cursor page returns 409 %s',
    async (code) => {
      const first = response();
      Object.assign(first, { total_count: 2, next_cursor: 'page-2' });
      const reloaded = response();
      reloaded.files = [
        first.files[0]!,
        { ...first.files[0]!, id: 8, key: 'file:8', filename: 'recovered.pdf' }
      ];
      reloaded.total_count = 2;
      let initialCalls = 0;
      let cursorCalls = 0;
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        const request = input instanceof Request ? input : new Request(input);
        const body = await request.clone().json() as { cursor?: string };
        if (body.cursor) {
          cursorCalls += 1;
          return Response.json(
            { error: code, message: 'Results changed under this cursor.' }, { status: 409 }
          );
        }
        initialCalls += 1;
        return Response.json(initialCalls === 1 ? first : reloaded);
      });
      render(FilesWorkspace, {
        client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
        sort: { field: 'occurred_at', direction: 'desc' }
      });
      const grid = await screen.findByRole('grid', { name: 'Files results' });
      await screen.findByRole('row', { name: /fixture.pdf/ });
      await fireEvent.scroll(grid);

      const alert = await screen.findByRole('alert');
      expect(alert.textContent).toContain('Results changed under this cursor.');
      expect(screen.getByRole('row', { name: /fixture.pdf/ })).toBeDefined();
      expect(screen.queryByRole('button', { name: 'Retry loading more files' })).toBeNull();

      // The cursor is dead: the scroll sentinel must not re-attempt it.
      await fireEvent.scroll(grid);
      expect(cursorCalls).toBe(1);

      await fireEvent.click(screen.getByRole('button', { name: 'Reload files' }));
      expect(await screen.findByText('recovered.pdf')).toBeDefined();
      expect(screen.queryByRole('alert')).toBeNull();
      expect(initialCalls).toBe(2);
      expect(cursorCalls).toBe(1);
    }
  );

  it('resumes deep-state restoration after retrying a transient cursor failure', async () => {
    const first = response();
    first.files[0] = { ...first.files[0]!, id: 1, key: 'file:1', filename: 'first.pdf' };
    Object.assign(first, { total_count: 2, next_cursor: 'page-2' });
    const second = response();
    second.files[0] = { ...second.files[0]!, id: 900, key: 'file:900', filename: 'deep.pdf' };
    second.total_count = 2;
    let cursorCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url, document.baseURI).pathname;
      if (path === '/api/v1/files/900') return Response.json({
        id: 900, message_id: 11, conversation_id: 21, filename: 'deep.pdf', mime_type: 'application/pdf',
        size_bytes: 2048, content_state: 'missing_blob', content_available: false
      });
      const body = await request.clone().json() as { cursor?: string };
      if (!body.cursor) return Response.json(first);
      cursorCalls += 1;
      if (cursorCalls === 1) return Response.json({
        error: 'analytical_cache_unavailable', message: 'Synthetic cursor cache outage.',
        readiness: 'absent', recovery_action: 'msgvault build-cache'
      }, { status: 503 });
      return Response.json(second);
    });
    const onRestorationComplete = vi.fn();
    const { container } = render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' }, selectedKey: 'file:900',
      activeKey: 'file:900', restorationEpoch: 7, onRestorationComplete
    });

    expect((await screen.findByRole('alert')).textContent).toContain('Synthetic cursor cache outage.');
    expect(onRestorationComplete).not.toHaveBeenCalled();

    await fireEvent.click(screen.getByRole('button', { name: 'Retry loading more files' }));

    await waitFor(() => expect(onRestorationComplete).toHaveBeenCalledWith(7));
    const grid = screen.getByRole('grid', { name: 'Files results' });
    expect(grid.getAttribute('aria-activedescendant')).toBe('file-row-900');
    expect(container.querySelector('#file-row-900')).not.toBeNull();
    expect(await screen.findByRole('dialog', { name: 'View deep.pdf' })).toBeDefined();
    expect(cursorCalls).toBe(2);
  });

  it('restarts deep-state restoration against the fresh listing after a terminal cursor reload', async () => {
    const first = response();
    first.files[0] = { ...first.files[0]!, id: 1, key: 'file:1', filename: 'first.pdf' };
    Object.assign(first, { total_count: 2, next_cursor: 'page-2' });
    const reloadedFirst = response();
    reloadedFirst.files[0] = { ...reloadedFirst.files[0]!, id: 1, key: 'file:1', filename: 'first.pdf' };
    Object.assign(reloadedFirst, { total_count: 2, next_cursor: 'page-2b' });
    const reloadedSecond = response();
    reloadedSecond.files[0] = { ...reloadedSecond.files[0]!, id: 900, key: 'file:900', filename: 'deep.pdf' };
    reloadedSecond.total_count = 2;
    let initialCalls = 0;
    let deadCursorCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url, document.baseURI).pathname;
      if (path === '/api/v1/files/900') return Response.json({
        id: 900, message_id: 11, conversation_id: 21, filename: 'deep.pdf', mime_type: 'application/pdf',
        size_bytes: 2048, content_state: 'missing_blob', content_available: false
      });
      const body = await request.clone().json() as { cursor?: string };
      if (body.cursor === 'page-2') {
        deadCursorCalls += 1;
        return Response.json(
          { error: 'archive_revision_changed', message: 'Results changed under this cursor.' },
          { status: 409 }
        );
      }
      if (body.cursor === 'page-2b') return Response.json(reloadedSecond);
      initialCalls += 1;
      return Response.json(initialCalls === 1 ? first : reloadedFirst);
    });
    const onRestorationComplete = vi.fn();
    const { container } = render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' }, selectedKey: 'file:900',
      activeKey: 'file:900', restorationEpoch: 9, onRestorationComplete
    });

    expect((await screen.findByRole('alert')).textContent).toContain('Results changed under this cursor.');
    expect(onRestorationComplete).not.toHaveBeenCalled();
    expect(screen.queryByRole('button', { name: 'Retry loading more files' })).toBeNull();

    await fireEvent.click(screen.getByRole('button', { name: 'Reload files' }));

    await waitFor(() => expect(onRestorationComplete).toHaveBeenCalledWith(9));
    const grid = screen.getByRole('grid', { name: 'Files results' });
    expect(grid.getAttribute('aria-activedescendant')).toBe('file-row-900');
    expect(container.querySelector('#file-row-900')).not.toBeNull();
    expect(await screen.findByRole('dialog', { name: 'View deep.pdf' })).toBeDefined();
    expect(initialCalls).toBe(2);
    expect(deadCursorCalls).toBe(1);
  });

  it('shows a consistency failure beside loaded rows and recovers via reload', async () => {
    const first = response();
    Object.assign(first, { total_count: 2, next_cursor: 'page-2' });
    const drifted = response();
    drifted.files[0] = { ...drifted.files[0]!, id: 8, key: 'file:8', filename: 'drifted.pdf' };
    drifted.cache_revision = 'cache-files-b';
    const reloaded = response();
    reloaded.files = [
      first.files[0]!,
      { ...first.files[0]!, id: 8, key: 'file:8', filename: 'recovered.pdf' }
    ];
    reloaded.total_count = 2;
    reloaded.cache_revision = 'cache-files-b';
    let initialCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const body = await request.clone().json() as { cursor?: string };
      if (body.cursor) return Response.json(drifted);
      initialCalls += 1;
      return Response.json(initialCalls === 1 ? first : reloaded);
    });
    render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' }
    });
    const grid = await screen.findByRole('grid', { name: 'Files results' });
    await screen.findByRole('row', { name: /fixture.pdf/ });
    await fireEvent.scroll(grid);

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Results changed while loading another page. Reload this view.');
    expect(screen.getByRole('row', { name: /fixture.pdf/ })).toBeDefined();

    await fireEvent.click(screen.getByRole('button', { name: 'Reload files' }));
    expect(await screen.findByText('recovered.pdf')).toBeDefined();
    expect(screen.getByText('fixture.pdf')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(initialCalls).toBe(2);
  });

  it('ignores a cursor failure from a superseded request', async () => {
    const first = response();
    first.files[0] = { ...first.files[0]!, filename: 'first.pdf' };
    Object.assign(first, { total_count: 2, next_cursor: 'page-2' });
    const fresh = response();
    fresh.files[0] = { ...fresh.files[0]!, id: 9, key: 'file:9', filename: 'fresh.pdf' };
    let rejectCursor: ((cause: unknown) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const body = await request.clone().json() as { cursor?: string; filename_query?: string };
      if (body.cursor) return new Promise<Response>((_, reject) => { rejectCursor = reject; });
      return Response.json(body.filename_query ? fresh : first);
    });
    const view = render(FilesWorkspace, {
      client: createAPIClient(fetchFn), predicate: { filters: [], presentation: 'table' },
      sort: { field: 'occurred_at', direction: 'desc' }
    });
    const grid = await screen.findByRole('grid', { name: 'Files results' });
    await screen.findByRole('row', { name: /first.pdf/ });
    await fireEvent.scroll(grid);
    await waitFor(() => expect(rejectCursor).toBeDefined());

    await view.rerender({ filenameQuery: 'fresh' });
    await screen.findByRole('row', { name: /fresh.pdf/ });
    rejectCursor!(new TypeError('stale cursor failure'));
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByText(/stale cursor failure/)).toBeNull();
    expect(screen.getByRole('row', { name: /fresh.pdf/ })).toBeDefined();
  });
});
