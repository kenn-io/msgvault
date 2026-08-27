import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { chooseSelectOption } from '../../../test/kit-ui';
import ContextBar from './ContextBar.svelte';

describe('ContextBar presentation control', () => {
  it('exposes Table, Timeline, and Files as one keyboard-operable Show-as control', async () => {
    const onPresentationChange = vi.fn();
    render(ContextBar, {
      client: createAPIClient(vi.fn<typeof fetch>()),
      query: 'pasta', searchMode: 'hybrid', filters: [], groupingChain: [],
      presentation: 'table', onPresentationChange,
      onAddGroup: vi.fn(), onRemoveGroup: vi.fn(), onClearFilters: vi.fn(),
      onFiltersChange: vi.fn()
    });

    const control = screen.getByRole('combobox', { name: 'Show as: Table' });
    await fireEvent.click(control);
    expect(screen.getAllByRole('option').map((option) => option.textContent?.trim()))
      .toEqual(['Table', 'Timeline', 'Files']);
    await fireEvent.click(screen.getByRole('option', { name: 'Timeline' }));
    expect(onPresentationChange).toHaveBeenCalledWith('timeline');
  });
});
