import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import ContextBar from './ContextBar.svelte';

const sort = { options: [{ value: 'newest', label: 'Newest first' }], value: 'newest' };

describe('ContextBar presentation control', () => {
  it.each(['table', 'timeline'] as const)('changes presentation from %s', async (presentation) => {
    const onPresentationChange = vi.fn();
    render(ContextBar, {
      client: createAPIClient(vi.fn<typeof fetch>()),
      query: 'pasta', searchMode: 'hybrid', filters: [], groupingChain: [],
      messages: presentation === 'timeline', presentation, onPresentationChange, countLabel: '0 items', sort,
      onAddGroup: vi.fn(), onRemoveGroup: vi.fn(), onClearFilters: vi.fn(),
      onFiltersChange: vi.fn()
    });

    const control = screen.getByRole('combobox', { name: `Show as: ${presentation === 'table' ? 'Table' : 'Timeline'}` });
    expect(control.textContent?.trim()).toBe(`Show as: ${presentation === 'table' ? 'Table' : 'Timeline'}`);
    expect(screen.getByRole('combobox', { name: 'Sort: Newest first' }).textContent?.trim())
      .toBe('Sort: Newest first');
    await fireEvent.click(control);
    expect(screen.getAllByRole('option').map((option) => option.textContent?.trim()))
      .toEqual(['Table', 'Timeline', 'Files']);
    await fireEvent.click(screen.getByRole('option', { name: presentation === 'table' ? 'Timeline' : 'Table' }));
    expect(onPresentationChange).toHaveBeenCalledWith(presentation === 'table' ? 'timeline' : 'table');
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

it('forwards Messages filter changes', async () => {
  const onFiltersChange = vi.fn();
  render(ContextBar, {
    messages: true, client: createAPIClient(async (input) => {
      if (new URL((input as Request).url).pathname.endsWith('/groups')) return Response.json({ rows: [{ key: 'whatsapp' }, { key: 'discord' }, { key: 'email' }] });
      return Response.json({ accounts: [], identities: [] });
    }), query: '', searchMode: 'full_text', filters: [{ dimension: 'message_type', values: ['whatsapp', 'restored-kind'] }], groupingChain: [], countLabel: '', sort,
    onAddGroup: vi.fn(), onRemoveGroup: vi.fn(), onClearFilters: vi.fn(), onFiltersChange,
  });
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  await fireEvent.click(await screen.findByLabelText('Email'));
  expect(onFiltersChange).toHaveBeenLastCalledWith([{ dimension: 'message_type', values: ['whatsapp', 'restored-kind', 'email'] }]);
});
