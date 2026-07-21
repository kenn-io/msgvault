import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { ExploreSelectionState } from '../../explore/state.svelte';
import type { EntryRow } from '../../explore/models';
import EverythingTable from './EverythingTable.svelte';

function row(index: number, overrides: Partial<EntryRow> = {}): EntryRow {
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
    source_type: 'gmail',
    participant_labels: ['Alice Example'],
    participant_ids: [1],
    attachment_count: 0,
    attachment_size: 0,
    has_attachments: false,
    deleted_from_source: false,
    message_count: 1,
    match: {},
    ...overrides
  };
}

describe('EverythingTable', () => {
  it('owns headers, virtual rows, and named states in one focusable grid', () => {
    const { rerender } = render(EverythingTable, {
      rows: [row(1)], selection: new ExploreSelectionState()
    });
    const grid = screen.getByRole('grid', { name: 'Everything results' });
    expect(screen.getAllByRole('grid')).toHaveLength(1);
    expect(grid.contains(screen.getByRole('columnheader', { name: 'Kind' }))).toBe(true);
    expect(grid.contains(screen.getByRole('row', { name: /Synthetic subject 1/ }))).toBe(true);

    void rerender({ rows: [], selection: new ExploreSelectionState(), error: 'Synthetic failure' });
    const alert = screen.getByRole('alert');
    expect(alert.closest('[role="gridcell"]')?.closest('[role="row"]')).not.toBeNull();
  });

  it('waits for computed row geometry before virtualizing and becomes ready reactively', async () => {
    document.documentElement.style.removeProperty('--row-height');
    document.documentElement.removeAttribute('data-density');
    const rendered = render(EverythingTable, {
      rows: [row(1)],
      selection: new ExploreSelectionState()
    });

    expect(screen.getByText('Preparing table layout…')).toBeDefined();
    expect(rendered.container.querySelector('.virtual-spacer')).toBeNull();

    document.documentElement.style.setProperty('--row-height', '41px');
    document.documentElement.dataset.density = 'comfortable';

    await waitFor(() => {
      const spacer = rendered.container.querySelector<HTMLElement>('.virtual-spacer');
      expect(spacer?.style.height).toBe('41px');
    });
    expect(screen.getByRole('row', { name: /Synthetic subject 1/ })).toBeDefined();
  });

  it('marks the inspected row independently from bulk selection', () => {
    render(EverythingTable, {
      rows: [row(1), row(2)],
      selection: new ExploreSelectionState(),
      inspectedKey: 'message:2'
    });

    expect(screen.getByRole('row', { name: /Synthetic subject 1/ }).getAttribute('aria-current')).toBeNull();
    expect(screen.getByRole('row', { name: /Synthetic subject 2/ }).getAttribute('aria-current')).toBe('true');
  });

  it('reports rendered conversation rows for lazy exact counts and displays the result', async () => {
    const onVisibleRows = vi.fn();
    render(EverythingTable, {
      rows: [row(1, { kind: 'conversation', message_type: 'sms', match: { lexical_match_count: 3 } })],
      selection: new ExploreSelectionState(),
      onVisibleRows
    });

    await waitFor(() => expect(onVisibleRows).toHaveBeenCalledWith(['message:1']));
    expect(screen.getByText('3 lexical matches')).toBeDefined();
  });

  it('leaves row height to the computed CSS token with initial columns and textual modality', () => {
    render(EverythingTable, { rows: [row(1)], selection: new ExploreSelectionState() });

    const rendered = screen.getByRole('row', { name: /Email item/ });
    expect((rendered as HTMLElement).style.height).toBe('');
    expect(getComputedStyle(document.documentElement).getPropertyValue('--row-height').trim()).toBe('36px');
    expect(screen.getByRole('columnheader', { name: 'Kind' })).toBeDefined();
    expect(screen.getByRole('columnheader', { name: 'People / source' })).toBeDefined();
    expect(screen.getByRole('columnheader', { name: 'Attachments' }).textContent).toBe('⌕');
    expect(screen.getByLabelText('Email item')).toBeDefined();
  });

  it('exposes size through the column picker without showing it initially', async () => {
    const onColumnsChange = vi.fn();
    render(EverythingTable, {
      rows: [row(1, { attachment_size: 2048 })],
      selection: new ExploreSelectionState(),
      onColumnsChange
    });

    expect(screen.queryByRole('columnheader', { name: 'Size' })).toBeNull();
    await fireEvent.click(screen.getByText('Columns'));
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Size' }));

    expect(screen.getByRole('columnheader', { name: 'Size' })).toBeDefined();
    expect(onColumnsChange).toHaveBeenCalled();
  });

  it('keeps keyboard focus on the grid while j/k move a stable keyed cursor', async () => {
    render(EverythingTable, {
      rows: [row(1), row(2), row(3)],
      selection: new ExploreSelectionState()
    });
    const grid = screen.getByRole('grid', { name: 'Everything results' });
    grid.focus();

    await fireEvent.keyDown(grid, { key: 'j' });
    await fireEvent.keyDown(grid, { key: 'j' });
    await fireEvent.keyDown(grid, { key: 'k' });

    expect(document.activeElement).toBe(grid);
    expect(grid.getAttribute('aria-activedescendant')).toContain('message-3a-2');
  });

  it('requests another cursor page when navigation or scrolling reaches the loaded boundary', async () => {
    const onLoadMore = vi.fn().mockResolvedValue(undefined);
    render(EverythingTable, {
      rows: [row(1), row(2)],
      selection: new ExploreSelectionState(),
      hasMore: true,
      onLoadMore
    });
    const grid = screen.getByRole('grid', { name: 'Everything results' }) as HTMLDivElement;
    grid.focus();

    await fireEvent.keyDown(grid, { key: 'j' });
    await fireEvent.keyDown(grid, { key: 'j' });
    expect(onLoadMore).toHaveBeenCalled();

    onLoadMore.mockClear();
    Object.defineProperty(grid, 'scrollTop', { configurable: true, value: 36, writable: true });
    await fireEvent.scroll(grid);
    expect(onLoadMore).toHaveBeenCalled();
  });

  it('uses lowercase a to select only non-overscan viewport rows', async () => {
    const selection = new ExploreSelectionState();
    render(EverythingTable, {
      rows: Array.from({ length: 20 }, (_, index) => row(index + 1)),
      selection
    });
    const grid = screen.getByRole('grid', { name: 'Everything results' });
    grid.focus();

    await fireEvent.keyDown(grid, { key: 'a' });

    expect(selection.count).toBe(10);
    expect(selection.selectedKeys(['message:1', 'message:10', 'message:11'])).toEqual([
      'message:1',
      'message:10'
    ]);
  });

  it('keeps the active stable key across reorder and reconciles manual scrolling to a rendered row', async () => {
    const selection = new ExploreSelectionState();
    const rows = Array.from({ length: 100 }, (_, index) => row(index + 1));
    const rendered = render(EverythingTable, { rows, selection });
    const grid = screen.getByRole('grid', { name: 'Everything results' }) as HTMLDivElement;
    grid.focus();
    await fireEvent.keyDown(grid, { key: 'j' });
    expect(grid.getAttribute('aria-activedescendant')).toContain('message-3a-2');

    await rendered.rerender({ rows: [row(100), ...rows.slice(0, 99)], selection });
    expect(grid.getAttribute('aria-activedescendant')).toContain('message-3a-2');

    Object.defineProperty(grid, 'scrollTop', { configurable: true, value: 50 * 36, writable: true });
    await fireEvent.scroll(grid);
    await waitFor(() => {
      const active = grid.getAttribute('aria-activedescendant');
      expect(active).toBeTruthy();
      expect(document.getElementById(active!)).toBeDefined();
      expect(document.getElementById(active!)).not.toBeNull();
    });
    expect(document.activeElement).toBe(grid);
  });

  it('reports a replacement stable key when search results remove the active row', async () => {
    const onActiveKey = vi.fn();
    const selection = new ExploreSelectionState();
    const rendered = render(EverythingTable, {
      rows: [row(1), row(2)],
      selection,
      focusedKey: 'message:2',
      onActiveKey
    });

    await rendered.rerender({ rows: [row(10), row(11)], selection, focusedKey: 'message:2', onActiveKey });

    await waitFor(() => expect(onActiveKey).toHaveBeenCalledWith('message:10'));
    expect(screen.getByRole('grid', { name: 'Everything results' }).getAttribute('aria-activedescendant'))
      .toContain('message-3a-10');
  });

  it('preserves a deep restoration key while its cursor pages are still loading', async () => {
    const onActiveKey = vi.fn();
    const selection = new ExploreSelectionState();
    const rendered = render(EverythingTable, {
      rows: [row(1), row(2)],
      selection,
      focusedKey: 'message:5000',
      restoring: true,
      onActiveKey
    });

    expect(onActiveKey).not.toHaveBeenCalled();
    expect(screen.getByRole('grid', { name: 'Everything results' }).getAttribute('aria-activedescendant')).toBeNull();

    await rendered.rerender({
      rows: [row(1), row(2), row(5000)],
      selection,
      focusedKey: 'message:5000',
      restoring: false,
      onActiveKey
    });
    await waitFor(() => expect(screen.getByRole('grid', { name: 'Everything results' })
      .getAttribute('aria-activedescendant')).toContain('message-3a-5000'));
    expect(onActiveKey).not.toHaveBeenCalled();
  });

  it('restores the keyed focus and scroll anchor supplied by URL state', async () => {
    render(EverythingTable, {
      rows: [row(1), row(2), row(3)],
      selection: new ExploreSelectionState(),
      focusedKey: 'message:3',
      scrollAnchor: { key: 'message:2', offset: 7 }
    });
    const grid = screen.getByRole('grid', { name: 'Everything results' }) as HTMLDivElement;
    await Promise.resolve();

    expect(grid.getAttribute('aria-activedescendant')).toContain('message-3a-3');
    expect(grid.scrollTop).toBe(43);
  });

  it('reapplies a retained anchor across same-count result generations', async () => {
    const rows = Array.from({ length: 200 }, (_, index) => row(index + 1));
    const selection = new ExploreSelectionState();
    const rendered = render(EverythingTable, {
      rows,
      selection,
      generation: 1,
      restoring: true,
      focusedKey: 'message:150',
      scrollAnchor: { key: 'message:140', offset: 5 }
    });
    const grid = screen.getByRole('grid', { name: 'Everything results' }) as HTMLDivElement;
    await waitFor(() => expect(grid.scrollTop).toBe(139 * 36 + 5));

    grid.scrollTop = 0;
    await rendered.rerender({
      rows: rows.map((entry) => ({ ...entry, title: `Rebuilt ${entry.title}` })),
      selection,
      generation: 2,
      restoring: true,
      focusedKey: 'message:150',
      scrollAnchor: { key: 'message:140', offset: 5 }
    });

    await waitFor(() => expect(grid.scrollTop).toBe(139 * 36 + 5));
  });

  it('does not discard the first real user scroll when a restored anchor was already at zero', async () => {
    const onScrollAnchor = vi.fn();
    render(EverythingTable, {
      rows: [row(1), row(2)],
      selection: new ExploreSelectionState(),
      scrollAnchor: { key: 'message:1', offset: 0 },
      onScrollAnchor
    });
    const grid = screen.getByRole('grid', { name: 'Everything results' }) as HTMLDivElement;
    await Promise.resolve();
    Object.defineProperty(grid, 'scrollTop', { configurable: true, value: 36, writable: true });
    await fireEvent.scroll(grid);

    expect(onScrollAnchor).toHaveBeenCalledWith('message:2', 0);
  });

  it('supports Space, Shift range, A visible, x clear, and Enter open', async () => {
    const selection = new ExploreSelectionState();
    const onOpen = vi.fn();
    render(EverythingTable, { rows: [row(1), row(2), row(3)], selection, onOpen });
    const grid = screen.getByRole('grid', { name: 'Everything results' });
    grid.focus();

    await fireEvent.keyDown(grid, { key: ' ' });
    await fireEvent.keyDown(grid, { key: 'j' });
    await fireEvent.keyDown(grid, { key: 'j' });
    await fireEvent.keyDown(grid, { key: ' ', shiftKey: true });
    expect(selection.selectedKeys(['message:1', 'message:2', 'message:3'])).toHaveLength(3);

    await fireEvent.keyDown(grid, { key: 'x' });
    expect(selection.count).toBe(0);
    await fireEvent.keyDown(grid, { key: 'A', shiftKey: true });
    expect(selection.count).toBe(3);
    await fireEvent.keyDown(grid, { key: 'Enter' });
    expect(onOpen).toHaveBeenCalledWith(expect.objectContaining({ key: 'message:3' }));
  });

  it('distinguishes loading, empty, and persistent cache-unavailable states', async () => {
    const selection = new ExploreSelectionState();
    const onRetry = vi.fn();
    const { rerender } = render(EverythingTable, { rows: [], selection, loading: true, onRetry });
    const skeleton = screen.getAllByTestId('everything-skeleton')[0]!;
    expect(skeleton.style.height).toBe('');
    expect(getComputedStyle(document.documentElement).getPropertyValue('--row-height').trim()).toBe('36px');
    const grid = screen.getByRole('grid', { name: 'Everything results' });
    expect(grid.getAttribute('aria-busy')).toBe('true');
    expect(grid.getAttribute('aria-rowcount')).toBeNull();
    expect(skeleton.style.gridTemplateColumns).toBe(
      screen.getByRole('row', { name: /Kind/ }).style.gridTemplateColumns
    );
    expect(skeleton.children).toHaveLength(6);

    void rerender({ rows: [], selection, loading: false });
    expect(screen.getByText('No items match this view')).toBeDefined();
    expect(grid.getAttribute('aria-rowcount')).toBe('2');

    void rerender({
      rows: [],
      selection,
      loading: false,
      unavailable: {
        error: 'cache_unavailable',
        message: 'The analytical cache has not been built.',
        readiness: 'absent',
        recovery_action: 'msgvault build-cache'
      },
      onRetry
    });
    expect(screen.getByRole('alert').textContent).toContain('msgvault build-cache');
    expect(grid.getAttribute('aria-rowcount')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry cache check' }));
    expect(onRetry).toHaveBeenCalledOnce();
    expect(screen.queryByText('No items match this view')).toBeNull();
  });

  it('renders request errors as an exclusive state rather than an empty result', () => {
    render(EverythingTable, {
      rows: [],
      selection: new ExploreSelectionState(),
      error: 'The query could not be completed.'
    });

    expect(screen.getByRole('alert').textContent).toContain('The query could not be completed.');
    expect(screen.queryByText('No items match this view')).toBeNull();
    expect(screen.getByRole('grid', { name: 'Everything results' }).getAttribute('aria-rowcount')).toBeNull();
  });
});
