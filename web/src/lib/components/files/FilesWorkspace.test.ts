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

  afterEach(() => vi.unstubAllGlobals());

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
