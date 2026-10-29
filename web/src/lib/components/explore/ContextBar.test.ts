import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import ContextBar from './ContextBar.svelte';

const sort = { options: [{ value: 'newest', label: 'Newest first' }], value: 'newest' };

describe('ContextBar presentation control', () => {
  it('exposes Table, Timeline, and Files as one keyboard-operable Show-as control', async () => {
    const onPresentationChange = vi.fn();
    render(ContextBar, {
      client: createAPIClient(vi.fn<typeof fetch>()),
      query: 'pasta', searchMode: 'hybrid', filters: [], groupingChain: [],
      presentation: 'table', onPresentationChange, countLabel: '0 items', sort,
      onAddGroup: vi.fn(), onRemoveGroup: vi.fn(), onClearFilters: vi.fn(),
      onFiltersChange: vi.fn()
    });

    const control = screen.getByRole('combobox', { name: 'Show as: Table' });
    expect(control.textContent?.trim()).toBe('Show as: Table');
    expect(screen.getByRole('combobox', { name: 'Sort: Newest first' }).textContent?.trim())
      .toBe('Sort: Newest first');
    await fireEvent.click(control);
    expect(screen.getAllByRole('option').map((option) => option.textContent?.trim()))
      .toEqual(['Table', 'Timeline', 'Files']);
    await fireEvent.click(screen.getByRole('option', { name: 'Timeline' }));
    expect(onPresentationChange).toHaveBeenCalledWith('timeline');
  });
});

describe('ContextBar chips', () => {
  it('shows readable, removable chips for the query, filters, and groupings', async () => {
    const onRemoveQuery = vi.fn();
    const onRemoveFilter = vi.fn();
    const onRemoveGroup = vi.fn();
    render(ContextBar, {
      client: createAPIClient(vi.fn()),
      query: 'network', searchMode: 'full_text',
      filters: [{ dimension: 'source', values: ['7'] }],
      groupingChain: ['year'],
      countLabel: '20 items',
      sort,
      onAddGroup: vi.fn(), onRemoveGroup, onClearFilters: vi.fn(), onFiltersChange: vi.fn(),
      onRemoveQuery, onRemoveFilter
    });
    expect(screen.getByText('Full text: “network”')).toBeTruthy();
    expect(screen.getByTitle('Full text: “network”')).toBeTruthy();
    expect(screen.getByText('Source: 7')).toBeTruthy();
    expect(screen.queryByText(/full_text/)).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Remove search' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Remove Source filter' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Remove Year grouping' }));
    expect(onRemoveQuery).toHaveBeenCalledOnce();
    expect(onRemoveFilter).toHaveBeenCalledWith(0);
    expect(onRemoveGroup).toHaveBeenCalledWith(0);
    expect(screen.getAllByText('20 items')).toHaveLength(1);
  });

  it('names a grouping chip the way saved-view summaries do', () => {
    render(ContextBar, {
      client: createAPIClient(vi.fn()),
      query: '', searchMode: 'full_text', filters: [], groupingChain: ['participant'],
      countLabel: '', sort,
      onAddGroup: vi.fn(), onRemoveGroup: vi.fn(), onClearFilters: vi.fn(), onFiltersChange: vi.fn()
    });
    expect(screen.getByText('Grouped by Person')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Remove Person grouping' })).toBeDefined();
  });

  it('says plainly when no filters are applied', async () => {
    render(ContextBar, {
      client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json({ rows: [] }))),
      query: '', searchMode: 'full_text', filters: [], groupingChain: [],
      countLabel: '', sort,
      onAddGroup: vi.fn(), onRemoveGroup: vi.fn(), onClearFilters: vi.fn(), onFiltersChange: vi.fn()
    });
    await fireEvent.click(screen.getByRole('button', { name: 'Filters' }));
    expect(screen.getByText('No filters applied.')).toBeDefined();
  });

  it('hides the chip line when nothing is active', () => {
    render(ContextBar, {
      client: createAPIClient(vi.fn()),
      query: '', searchMode: 'full_text', filters: [], groupingChain: [],
      countLabel: '20 items',
      sort,
      onAddGroup: vi.fn(), onRemoveGroup: vi.fn(), onClearFilters: vi.fn(), onFiltersChange: vi.fn()
    });
    expect(screen.queryByText('All archive entries')).toBeNull();
    expect(screen.queryByRole('button', { name: /^Remove / })).toBeNull();
  });
});

describe('ContextBar grouping picker', () => {
  it.each([
    [[], 'Group by'],
    [['participant'], 'Add grouping']
  ] as const)('names the picker by what it does when the chain is %j', (groupingChain, name) => {
    render(ContextBar, {
      client: createAPIClient(vi.fn()),
      query: '', searchMode: 'full_text', filters: [], groupingChain: [...groupingChain],
      countLabel: '', sort,
      onAddGroup: vi.fn(), onRemoveGroup: vi.fn(), onClearFilters: vi.fn(), onFiltersChange: vi.fn()
    });
    expect(screen.getByRole('combobox', { name }).textContent?.trim()).toBe(name);
  });

  it('adds a grouping from the picker', async () => {
    const onAddGroup = vi.fn();
    render(ContextBar, {
      client: createAPIClient(vi.fn()),
      query: '', searchMode: 'full_text', filters: [], groupingChain: [],
      countLabel: '', sort,
      onAddGroup, onRemoveGroup: vi.fn(), onClearFilters: vi.fn(), onFiltersChange: vi.fn()
    });
    await fireEvent.click(screen.getByRole('combobox', { name: 'Group by' }));
    await fireEvent.click(screen.getByRole('option', { name: 'Year' }));
    expect(onAddGroup).toHaveBeenCalledWith('year');
  });

  it('lists unavailable groupings with a short, disabled label', async () => {
    render(ContextBar, {
      client: createAPIClient(vi.fn()),
      query: '', searchMode: 'full_text', filters: [], groupingChain: [],
      countLabel: '', sort,
      onAddGroup: vi.fn(), onRemoveGroup: vi.fn(), onClearFilters: vi.fn(), onFiltersChange: vi.fn()
    });
    await fireEvent.click(screen.getByRole('combobox', { name: 'Group by' }));
    const labels = screen.getByRole('option', { name: 'Labels (not available yet)' });
    expect((labels as HTMLButtonElement).disabled).toBe(true);
    expect(screen.queryByRole('option', { name: /analytical API/ })).toBeNull();
  });
});

it('keeps Messages grouping chains with fixed presentation and order', async () => {
  const onFiltersChange = vi.fn();
  const onAddGroup = vi.fn();
  render(ContextBar, {
    messages: true, client: createAPIClient(async (input) => {
      const path = new URL((input as Request).url).pathname;
      if (path.endsWith('/groups')) return Response.json({ rows: [{ key: 'email', count: 2 }], cache_revision: 'cache' });
      return Response.json({ accounts: [], identities: [] });
    }), query: '', searchMode: 'full_text', filters: [], groupingChain: ['source'], countLabel: '2 items', sort,
    onAddGroup, onRemoveGroup: vi.fn(), onClearFilters: vi.fn(), onFiltersChange,
  });
  expect(screen.queryByRole('combobox', { name: /Show as/ })).toBeNull();
  expect(screen.queryByRole('combobox', { name: /Sort:/ })).toBeNull();
  expect(screen.getByRole('combobox', { name: 'Add grouping' })).toBeDefined();
});

it('keeps Kind lookup failures visible and retries when Filter reopens', async () => {
  let failed = true;
  render(ContextBar, {
    messages: true, client: createAPIClient(async (input) => {
      if (new URL((input as Request).url).pathname.endsWith('/groups')) {
        return failed ? Response.json({ message: 'Unavailable' }, { status: 500 }) : Response.json({ rows: [{ key: 'email', count: 1 }] });
      }
      return Response.json({ accounts: [], identities: [] });
    }), query: '', searchMode: 'full_text', filters: [], groupingChain: [], countLabel: '', sort,
    onAddGroup: vi.fn(), onRemoveGroup: vi.fn(), onClearFilters: vi.fn(), onFiltersChange: vi.fn(),
  });
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  expect(await screen.findByText('Could not load kinds')).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  failed = false;
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  expect(await screen.findByLabelText('Email')).toBeDefined();
  expect(screen.queryByText('Could not load kinds')).toBeNull();
});

it('keeps the selected chat network and restored kinds when Email is added', async () => {
  const onFiltersChange = vi.fn();
  const view = render(ContextBar, {
    messages: true, client: createAPIClient(async (input) => {
      if (new URL((input as Request).url).pathname.endsWith('/groups')) return Response.json({ rows: [{ key: 'whatsapp' }, { key: 'discord' }, { key: 'email' }] });
      return Response.json({ accounts: [], identities: [] });
    }), query: '', searchMode: 'full_text', filters: [{ dimension: 'message_type', values: ['whatsapp', 'restored-kind'] }], groupingChain: [], countLabel: '', sort,
    onAddGroup: vi.fn(), onRemoveGroup: vi.fn(), onClearFilters: vi.fn(), onFiltersChange,
  });
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  await fireEvent.click(await screen.findByLabelText('Email'));
  expect(onFiltersChange).toHaveBeenLastCalledWith([{ dimension: 'message_type', values: ['whatsapp', 'restored-kind', 'email'] }]);
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  await view.rerender({ filters: [] });
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  await fireEvent.click(await screen.findByLabelText('Email'));
  expect(onFiltersChange).toHaveBeenLastCalledWith([{ dimension: 'message_type', values: ['email'] }]);
});

it('retains committed dates when a range is reversed, accepts the same day, and clears a bound', async () => {
  const onFiltersChange = vi.fn();
  const after = '2026-07-18T00:00:00.000Z';
  const before = '2026-07-21T00:00:00.000Z';
  render(ContextBar, {
    messages: true, client: createAPIClient(async () => Response.json({ rows: [], accounts: [], identities: [] })),
    query: '', searchMode: 'full_text', filters: [{ dimension: 'after', values: [after] }, { dimension: 'before', values: [before] }], groupingChain: [], countLabel: '', sort,
    onAddGroup: vi.fn(), onRemoveGroup: vi.fn(), onClearFilters: vi.fn(), onFiltersChange,
  });
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  const from = screen.getByLabelText('From') as HTMLInputElement;
  const to = screen.getByLabelText('To') as HTMLInputElement;
  await fireEvent.change(from, { target: { value: '2026-07-21' } });
  expect(screen.getByRole('alert').textContent).toBe('From must be on or before To.');
  expect(from.value).toBe('2026-07-18');
  await fireEvent.change(to, { target: { value: '2026-07-17' } });
  expect(to.value).toBe('2026-07-20');
  expect(onFiltersChange).not.toHaveBeenCalled();
  await fireEvent.change(from, { target: { value: '2026-07-20' } });
  expect(onFiltersChange).toHaveBeenLastCalledWith([{ dimension: 'before', values: [before] }, { dimension: 'after', values: ['2026-07-20T00:00:00.000Z'] }]);
  expect(screen.queryByRole('alert')).toBeNull();
  await fireEvent.change(from, { target: { value: '' } });
  expect(onFiltersChange).toHaveBeenLastCalledWith([{ dimension: 'before', values: [before] }]);
  await fireEvent.change(to, { target: { value: '2026-07-18' } });
  expect(onFiltersChange).toHaveBeenLastCalledWith([{ dimension: 'after', values: [after] }, { dimension: 'before', values: ['2026-07-19T00:00:00.000Z'] }]);
});

it('lets a restored Messages Timeline return to Table', async () => {
  const onPresentationChange = vi.fn();
  render(ContextBar, {
    messages: true, presentation: 'timeline', client: createAPIClient(vi.fn()), query: '', searchMode: 'full_text', filters: [], groupingChain: [], countLabel: '', sort,
    onAddGroup: vi.fn(), onRemoveGroup: vi.fn(), onClearFilters: vi.fn(), onFiltersChange: vi.fn(), onPresentationChange,
  });
  await fireEvent.click(screen.getByRole('combobox', { name: 'Show as: Timeline' }));
  await fireEvent.click(screen.getByRole('option', { name: 'Table' }));
  expect(onPresentationChange).toHaveBeenCalledWith('table');
});
