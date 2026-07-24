import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import SearchBar from './SearchBar.svelte';

describe('SearchBar', () => {
  it('shows every explicit search mode and submits the selected mode', async () => {
    const onSubmit = vi.fn();
    render(SearchBar, {
      props: { initialQuery: '', initialMode: 'fts', onSubmit }
    });

    expect(screen.getByRole('radio', { name: 'Full text' }).getAttribute('aria-checked')).toBe('true');
    expect(screen.getByRole('radio', { name: 'Semantic' })).toBeDefined();
    expect(screen.getByRole('radio', { name: 'Hybrid' })).toBeDefined();

    await fireEvent.click(screen.getByRole('radio', { name: 'Semantic' }));
    await fireEvent.input(screen.getByRole('searchbox'), {
      target: { value: '  quarterly plan  ' }
    });
    await fireEvent.submit(screen.getByRole('search', { name: 'Search archive' }));

    expect(onSubmit).toHaveBeenCalledWith('quarterly plan', 'vector');
  });

  it('submits an empty query so the owner can clear URL state', async () => {
    const onSubmit = vi.fn();
    render(SearchBar, {
      props: { initialQuery: 'old query', initialMode: 'hybrid', onSubmit }
    });

    await fireEvent.input(screen.getByRole('searchbox'), { target: { value: '   ' } });
    await fireEvent.submit(screen.getByRole('search', { name: 'Search archive' }));

    expect(onSubmit).toHaveBeenCalledWith('', 'hybrid');
  });

  it('restores query and mode when URL-owned props change', async () => {
    const { rerender } = render(SearchBar, {
      props: { initialQuery: 'first', initialMode: 'fts', onSubmit: vi.fn() }
    });

    await rerender({
      initialQuery: 'restored',
      initialMode: 'hybrid',
      onSubmit: vi.fn()
    });

    expect((screen.getByRole('searchbox') as HTMLInputElement).value).toBe('restored');
    expect(screen.getByRole('radio', { name: 'Hybrid' }).getAttribute('aria-checked')).toBe('true');
  });
});
