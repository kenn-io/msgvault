import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { MEDIA } from '@kenn-io/kit-ui';
import { createRawSnippet } from 'svelte';

import { createAPIClient } from '../../api/client';
import ContextBar from './ContextBar.svelte';

const sort = { options: [{ value: 'newest', label: 'Newest first' }], value: 'newest' };

function mockCompact(compact: boolean): void {
  const matchMedia = window.matchMedia.bind(window);
  vi.spyOn(window, 'matchMedia').mockImplementation((query) => {
    const list = matchMedia(query);
    return {
      media: list.media, matches: compact && query === MEDIA.compact, onchange: list.onchange,
      addListener: list.addListener.bind(list), removeListener: list.removeListener.bind(list),
      addEventListener: list.addEventListener.bind(list), removeEventListener: list.removeEventListener.bind(list),
      dispatchEvent: list.dispatchEvent.bind(list)
    };
  });
}

describe('ContextBar extra controls', () => {
  afterEach(() => vi.restoreAllMocks());

  it.each([true, false])('places extra controls in the Filter dialog only when compact is %s', async (compact) => {
    mockCompact(compact);
    const { container } = render(ContextBar, {
      client: createAPIClient(vi.fn(async () => Response.json({ rows: [] }))),
      messages: true, query: '', searchMode: 'full_text', filters: [], groupingChain: [], countLabel: '20 files', sort,
      filterExtra: createRawSnippet(() => ({ render: () => '<span>Filename filter</span>' })),
      extraControls: createRawSnippet(() => ({ render: () => '<button>Visual search</button>' })),
      onAddGroup: vi.fn(), onRemoveGroup: vi.fn(), onClearFilters: vi.fn(), onFiltersChange: vi.fn()
    });
    expect(container.querySelector('.context-controls')?.textContent?.includes('Visual search')).toBe(!compact);
    await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
    const dialog = screen.getByRole('dialog', { name: 'Filter' });
    expect(dialog.textContent).toContain('Filename filter');
    expect(within(dialog).queryByRole('button', { name: 'Visual search' }) !== null).toBe(compact);
  });
});

describe('ContextBar compact View menu', () => {
  beforeEach(() => mockCompact(true));
  afterEach(() => vi.restoreAllMocks());

  it('offers presentation, grouping, and sort in View', async () => {
    const onPresentationChange = vi.fn();
    const onAddGroup = vi.fn();
    const onchange = vi.fn();
    render(ContextBar, {
      client: createAPIClient(vi.fn()), query: '', searchMode: 'full_text', filters: [], groupingChain: [], countLabel: '20 items',
      sort: { options: [...sort.options, { value: 'oldest', label: 'Oldest first' }], value: 'newest', note: 'Archive order', onchange },
      onPresentationChange, onAddGroup, onRemoveGroup: vi.fn(), onClearFilters: vi.fn(), onFiltersChange: vi.fn()
    });
    expect(screen.queryByRole('combobox', { name: 'Show as: Table' })).toBeNull();
    const view = screen.getByRole('button', { name: 'View' });
    await fireEvent.click(view);
    expect(screen.getByRole('menuitemradio', { name: 'Table' }).getAttribute('aria-checked')).toBe('true');
    expect(screen.getByRole('menuitemradio', { name: 'Labels (not available yet)' }).getAttribute('aria-disabled')).toBe('true');
    expect(screen.getByRole('menuitemradio', { name: 'Archive order' }).getAttribute('aria-disabled')).toBe('true');
    await fireEvent.click(screen.getByRole('menuitemradio', { name: 'Timeline' }));
    expect(onPresentationChange).toHaveBeenCalledWith('timeline');
    expect(screen.queryByRole('menu')).toBeNull();
    await fireEvent.click(view);
    await fireEvent.click(screen.getByRole('menuitemradio', { name: 'Year' }));
    expect(onAddGroup).toHaveBeenCalledWith('year');
    await fireEvent.click(view);
    await fireEvent.click(screen.getByRole('menuitemradio', { name: 'Oldest first' }));
    expect(onchange).toHaveBeenCalledWith('oldest');
  });

  it.each([
    { label: 'with sort', sort, groupingChain: [] as const },
    { label: 'without sort', sort: undefined, groupingChain: ['year'] as const }
  ])('handles a locked person scope $label', async ({ sort: scopeSort, groupingChain }) => {
    render(ContextBar, {
      client: createAPIClient(vi.fn()), query: '', searchMode: 'full_text', filters: [], groupingChain: [...groupingChain], countLabel: '', sort: scopeSort,
      lockedFilter: { dimension: 'participant', values: ['1'] },
      onAddGroup: vi.fn(), onRemoveGroup: vi.fn(), onClearFilters: vi.fn(), onFiltersChange: vi.fn()
    });
    if (!scopeSort) {
      expect(screen.queryByRole('button', { name: 'View' })).toBeNull();
      return;
    }
    await fireEvent.click(screen.getByRole('button', { name: 'View' }));
    expect(screen.getAllByRole('menuitemradio').map((item) => item.textContent?.trim())).toEqual(['Newest first']);
    expect(screen.queryByRole('menuitem', { name: 'Year' })).toBeNull();
  });

  it('keeps grouping choices when grouped without sort', async () => {
    render(ContextBar, {
      client: createAPIClient(vi.fn()), query: '', searchMode: 'full_text', filters: [], groupingChain: ['year'], countLabel: '2 groups',
      onAddGroup: vi.fn(), onRemoveGroup: vi.fn(), onClearFilters: vi.fn(), onFiltersChange: vi.fn()
    });
    await fireEvent.click(screen.getByRole('button', { name: 'View' }));
    expect(screen.getByRole('menuitemradio', { name: 'Month' })).toBeDefined();
    expect(screen.queryByRole('group', { name: 'Sort' })).toBeNull();
    expect(screen.getByRole('menuitemradio', { name: 'Year' }).getAttribute('aria-checked')).toBe('true');
  });
});

describe('ContextBar presentation control', () => {
  it('shows presentation and sort choices and changes Table to Timeline', async () => {
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
  it('hides presentation and grouping choices for a locked filter', () => {
    render(ContextBar, {
      client: createAPIClient(vi.fn()),
      query: '', searchMode: 'full_text', filters: [], groupingChain: [],
      lockedFilter: { dimension: 'participant', values: ['1'] },
      countLabel: '', sort,
      onAddGroup: vi.fn(), onRemoveGroup: vi.fn(), onClearFilters: vi.fn(), onFiltersChange: vi.fn()
    });
    expect(screen.queryByRole('combobox', { name: /^Show as:/ })).toBeNull();
    expect(screen.queryByRole('combobox', { name: 'Group by' })).toBeNull();
  });

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
