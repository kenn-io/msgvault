import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import ContextBar from './ContextBar.svelte';

describe('ContextBar presentation control', () => {
  it('exposes Table, Timeline, and Files as one keyboard-operable Show-as control', async () => {
    const onPresentationChange = vi.fn();
    render(ContextBar, {
      query: 'pasta', searchMode: 'hybrid', filters: [], groupingChain: [],
      presentation: 'table', onPresentationChange,
      onAddGroup: vi.fn(), onRemoveGroup: vi.fn(), onClearFilters: vi.fn()
    });

    const control = screen.getByRole('combobox', { name: 'Show as' });
    expect([...control.querySelectorAll('option')].map((option) => option.textContent))
      .toEqual(['Table', 'Timeline', 'Files']);

    await fireEvent.change(control, { target: { value: 'timeline' } });
    expect(onPresentationChange).toHaveBeenCalledWith('timeline');
  });
});
