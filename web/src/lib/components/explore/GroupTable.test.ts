import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import GroupTable from './GroupTable.svelte';

describe('GroupTable', () => {
  const rows = [
    { key: '7', label: 'Example source', count: 12, estimated_bytes: 2048, latest_at: '2026-07-18T12:00:00Z' },
    { key: '8', label: 'Second source', count: 5, estimated_bytes: 512, latest_at: '2026-07-17T12:00:00Z' }
  ];

  it('owns headers, virtual rows, and named states in one focusable grid', async () => {
    const rendered = render(GroupTable, { rows, dimension: 'source', onDrill: vi.fn() });
    const grid = screen.getByRole('grid', { name: 'Everything grouped by source' });
    expect(screen.getAllByRole('grid')).toHaveLength(1);
    expect(grid.contains(screen.getByRole('columnheader', { name: 'Group' }))).toBe(true);
    expect(grid.contains(screen.getByRole('row', { name: /Example source/ }))).toBe(true);

    await rendered.rerender({ rows: [], dimension: 'source', onDrill: vi.fn(), error: 'Synthetic failure' });
    const alert = screen.getByRole('alert');
    expect(alert.closest('[role="gridcell"]')?.closest('[role="row"]')).not.toBeNull();
    expect(grid.getAttribute('aria-rowcount')).toBeNull();

    await rendered.rerender({ rows: [], dimension: 'source', onDrill: vi.fn(), loading: true, error: '' });
    expect(screen.getByRole('status').textContent).toContain('Loading grouped results');
    expect(grid.getAttribute('aria-rowcount')).toBeNull();

    await rendered.rerender({ rows: [], dimension: 'source', onDrill: vi.fn(), loading: false, error: '' });
    expect(screen.getByText('No groups match this view.')).toBeDefined();
    expect(grid.getAttribute('aria-rowcount')).toBe('2');
  });

  it('leaves aggregate row height to CSS and drills with keyboard focus retained', async () => {
    const onDrill = vi.fn();
    render(GroupTable, { rows, dimension: 'source', onDrill });
    const grid = screen.getByRole('grid', { name: 'Everything grouped by source' });
    const first = screen.getByRole('row', { name: /Example source/ });

    expect((first as HTMLElement).style.height).toBe('');
    expect(getComputedStyle(document.documentElement).getPropertyValue('--row-height').trim()).toBe('36px');
    grid.focus();
    await fireEvent.keyDown(grid, { key: 'j' });
    await fireEvent.keyDown(grid, { key: 'Enter' });

    expect(document.activeElement).toBe(grid);
    expect(onDrill).toHaveBeenCalledWith(expect.objectContaining({ key: '8' }));
    expect(first.getAttribute('aria-rowindex')).toBe('2');
    expect(screen.getByRole('button', { name: 'Drill into Example source' }).textContent?.trim()).toBe('Drill');
  });

  it('does not let a focused drill control bubble Enter into grid navigation', async () => {
    const onDrill = vi.fn();
    render(GroupTable, { rows, dimension: 'source', onDrill });
    const drill = screen.getByRole('button', { name: 'Drill into Example source' });

    drill.focus();
    await fireEvent.keyDown(drill, { key: 'Enter' });
    expect(onDrill).not.toHaveBeenCalled();
    await fireEvent.click(drill);
    expect(onDrill).toHaveBeenCalledOnce();
  });

  it('renders cache and request failures exclusively with retry', async () => {
    const onRetry = vi.fn();
    const rendered = render(GroupTable, {
      rows: [],
      dimension: 'source',
      onDrill: vi.fn(),
      unavailable: {
        error: 'cache_unavailable',
        message: 'The analytical cache has not been built.',
        readiness: 'absent',
        recovery_action: 'msgvault build-cache'
      },
      onRetry
    });

    expect(screen.getByRole('alert').textContent).toContain('msgvault build-cache');
    expect(screen.getByRole('grid', { name: 'Everything grouped by source' }).getAttribute('aria-rowcount')).toBeNull();
    expect(screen.queryByText('No groups match this view.')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry cache check' }));
    expect(onRetry).toHaveBeenCalledOnce();

    await rendered.rerender({
      rows: [], dimension: 'source', onDrill: vi.fn(), error: 'Grouped query failed.',
      unavailable: undefined, onRetry
    });
    expect(screen.getByRole('alert').textContent).toContain('Grouped query failed.');
    expect(screen.queryByText('No groups match this view.')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry request' }));
    expect(onRetry).toHaveBeenCalledTimes(2);
  });

  it('uses the observed viewport, durable group key and one-based virtual row indices', async () => {
    let resizeCallback: ResizeObserverCallback | undefined;
    const observe = vi.fn();
    const disconnect = vi.fn();
    vi.stubGlobal('ResizeObserver', class {
      constructor(callback: ResizeObserverCallback) { resizeCallback = callback; }
      observe = observe;
      disconnect = disconnect;
    });
    const onActiveKey = vi.fn();
    const onScrollAnchor = vi.fn();
    const manyRows = Array.from({ length: 200 }, (_, index) => ({
      key: String(index + 1), label: `Source ${index + 1}`, count: index + 1,
      estimated_bytes: 10, latest_at: '2026-07-18T12:00:00Z'
    }));
    render(GroupTable, {
      rows: manyRows,
      dimension: 'source',
      onDrill: vi.fn(),
      focusedKey: 'group:source:150',
      scrollAnchor: { key: 'group:source:150', offset: 5 },
      onActiveKey,
      onScrollAnchor
    });
    const grid = screen.getByRole('grid', { name: 'Everything grouped by source' }) as HTMLDivElement;
    Object.defineProperty(grid, 'clientHeight', { configurable: true, value: 720 });
    resizeCallback?.([], {} as ResizeObserver);

    await waitFor(() => expect(grid.scrollTop).toBe(149 * 36 + 5));
    expect(observe).toHaveBeenCalledWith(grid);
    expect(grid.querySelectorAll('[role="row"]').length).toBeLessThan(40);
    expect(grid.getAttribute('aria-activedescendant')).toContain('150');
    expect(onActiveKey).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it('reapplies an exact anchor when a retained same-count result gets a new generation', async () => {
    const onActiveKey = vi.fn();
    const onScrollAnchor = vi.fn();
    const manyRows = Array.from({ length: 200 }, (_, index) => ({
      key: String(index + 1), label: `Source ${index + 1}`, count: index + 1,
      estimated_bytes: 10, latest_at: '2026-07-18T12:00:00Z'
    }));
    const rendered = render(GroupTable, {
      rows: manyRows,
      dimension: 'source',
      generation: 1,
      restoring: true,
      focusedKey: 'group:source:150',
      scrollAnchor: { key: 'group:source:140', offset: 5 },
      onDrill: vi.fn(),
      onActiveKey,
      onScrollAnchor
    });
    const grid = screen.getByRole('grid', { name: 'Everything grouped by source' }) as HTMLDivElement;
    await waitFor(() => expect(grid.scrollTop).toBe(139 * 36 + 5));
    await fireEvent.scroll(grid);
    expect(onActiveKey).not.toHaveBeenCalled();
    expect(onScrollAnchor).not.toHaveBeenCalled();

    grid.scrollTop = 0;
    await rendered.rerender({
      rows: manyRows.map((row) => ({ ...row, label: `Rebuilt ${row.label}` })),
      dimension: 'source',
      generation: 2,
      restoring: true,
      focusedKey: 'group:source:150',
      scrollAnchor: { key: 'group:source:140', offset: 5 },
      onDrill: vi.fn(),
      onActiveKey,
      onScrollAnchor
    });

    await waitFor(() => expect(grid.scrollTop).toBe(139 * 36 + 5));
    await fireEvent.scroll(grid);
    expect(grid.getAttribute('aria-activedescendant')).toContain('source-3A150');
    expect(onActiveKey).not.toHaveBeenCalled();
    expect(onScrollAnchor).not.toHaveBeenCalled();
  });

  it('keeps a multi-dimension anchor authoritative when restoration completes', async () => {
    const onActiveKey = vi.fn();
    const onScrollAnchor = vi.fn();
    const manyRows = Array.from({ length: 200 }, (_, index) => ({
      key: String(index + 1), label: `Group ${index + 1}`, count: index + 1,
      estimated_bytes: 10, latest_at: '2026-07-18T12:00:00Z'
    }));
    const rendered = render(GroupTable, {
      rows: manyRows,
      dimension: 'source',
      generation: 1,
      restoring: true,
      focusedKey: 'group:source:150',
      scrollAnchor: { key: 'group:source:140', offset: 5 },
      onDrill: vi.fn(),
      onActiveKey,
      onScrollAnchor
    });
    const grid = screen.getByRole('grid', { name: 'Everything grouped by source' }) as HTMLDivElement;
    await waitFor(() => expect(grid.scrollTop).toBe(139 * 36 + 5));

    await rendered.rerender({
      rows: manyRows,
      dimension: 'month',
      generation: 2,
      restoring: true,
      focusedKey: 'group:month:150',
      scrollAnchor: { key: 'group:month:140', offset: 5 },
      onDrill: vi.fn(),
      onActiveKey,
      onScrollAnchor
    });
    await waitFor(() => expect(grid.scrollTop).toBe(139 * 36 + 5));
    await fireEvent.scroll(grid);
    expect(onActiveKey).not.toHaveBeenCalled();
    expect(onScrollAnchor).not.toHaveBeenCalled();

    await rendered.rerender({
      rows: manyRows,
      dimension: 'month',
      generation: 2,
      restoring: false,
      focusedKey: 'group:month:150',
      scrollAnchor: { key: 'group:month:140', offset: 5 },
      onDrill: vi.fn(),
      onActiveKey,
      onScrollAnchor
    });

    await waitFor(() => expect(grid.scrollTop).toBe(139 * 36 + 5));
    expect(grid.getAttribute('aria-activedescendant')).toContain('month-3A150');
  });

  it('removes the drill affordance for a non-filterable group dimension', async () => {
    const onDrill = vi.fn();
    render(GroupTable, { rows, dimension: 'kind', drillable: false, onDrill });
    const grid = screen.getByRole('grid', { name: 'Everything grouped by kind' });
    grid.focus();
    await fireEvent.keyDown(grid, { key: 'Enter' });

    expect(screen.queryByRole('button', { name: /Drill into/ })).toBeNull();
    expect(onDrill).not.toHaveBeenCalled();
  });

  it('opens aggregate inspection from a focused pointer row without drilling', async () => {
    const onInspect = vi.fn();
    const onDrill = vi.fn();
    render(GroupTable, { rows, dimension: 'source', onDrill, onInspect });

    await fireEvent.pointerDown(screen.getByRole('row', { name: /Example source/ }));

    expect(onInspect).toHaveBeenCalledWith(rows[0]);
    expect(onDrill).not.toHaveBeenCalled();
  });

  it('marks the inspected aggregate independently from keyboard focus', () => {
    render(GroupTable, {
      rows,
      dimension: 'source',
      focusedKey: 'group:source:7',
      inspectedKey: 'group:source:8',
      onDrill: vi.fn()
    });

    expect(screen.getByRole('row', { name: /Example source/ }).getAttribute('aria-current')).toBeNull();
    expect(screen.getByRole('row', { name: /Second source/ }).getAttribute('aria-current')).toBe('true');
  });
});
