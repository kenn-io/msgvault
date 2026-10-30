import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { chooseSelectOption } from '../../../test/kit-ui';
import SearchBar from './SearchBar.svelte';

function setup(live: boolean) {
  const onDraft = vi.fn();
  const onSubmit = vi.fn();
  render(SearchBar, { workspace: 'sources', query: 'budget', mode: 'full_text', live, compact: false, onDraft, onSubmit });
  return { onDraft, onSubmit, input: screen.getByRole('searchbox', { name: 'Search everything' }) };
}

describe('SearchBar', () => {
  it('reports each keystroke when live', async () => {
    const { onDraft, input } = setup(true);
    await fireEvent.input(input, { target: { value: 'budget q3' } });
    expect(onDraft).toHaveBeenLastCalledWith('budget q3', 'full_text');
  });

  it('keeps typing local when not live and submits the draft', async () => {
    const { onDraft, onSubmit, input } = setup(false);
    await fireEvent.input(input, { target: { value: '  merger  ' } });
    await fireEvent.click(screen.getByRole('radio', { name: 'Hybrid' }));
    expect((input as HTMLInputElement).value).toBe('  merger  ');
    await fireEvent.submit(screen.getByRole('search', { name: 'Search Everything' }));
    expect(onDraft).not.toHaveBeenCalled();
    expect(onSubmit).toHaveBeenCalledWith('merger', 'hybrid');
  });

  it('shows the committed query again when it changes', async () => {
    const onSubmit = vi.fn();
    const { rerender } = render(SearchBar, {
      workspace: 'sources',
      query: 'budget',
      mode: 'full_text',
      live: false,
      compact: false,
      onDraft: vi.fn(),
      onSubmit
    });
    const input = screen.getByRole('searchbox', { name: 'Search everything' }) as HTMLInputElement;
    expect(input.value).toBe('budget');

    await rerender({ query: 'restored', mode: 'hybrid' });

    expect(input.value).toBe('restored');
    expect(screen.getByRole('radio', { name: 'Hybrid' }).getAttribute('aria-checked')).toBe('true');
  });

  it('offers the search mode as a select when compact', () => {
    render(SearchBar, {
      workspace: 'sources',
      query: '',
      mode: 'semantic',
      live: true,
      compact: true,
      onDraft: vi.fn(),
      onSubmit: vi.fn()
    });
    expect(screen.getByRole('combobox', { name: /^Search mode:/ })).toBeTruthy();
    expect(screen.queryByRole('radiogroup', { name: 'Search mode' })).toBeNull();
  });

  it.each([
    [true, 1],
    [false, 0]
  ])('reports a compact mode choice only when live (live=%s)', async (live, calls) => {
    const onDraft = vi.fn();
    render(SearchBar, { workspace: 'sources', query: 'budget', mode: 'full_text', live, compact: true, onDraft, onSubmit: vi.fn() });

    await chooseSelectOption(screen.getByRole('combobox', { name: /^Search mode:/ }), 'Hybrid');

    expect(onDraft).toHaveBeenCalledTimes(calls);
    if (calls) expect(onDraft).toHaveBeenLastCalledWith('budget', 'hybrid');
  });

  it('keeps a non-live draft when only the committed mode changes', async () => {
    const { rerender } = render(SearchBar, {
      workspace: 'sources',
      query: 'budget',
      mode: 'full_text',
      live: false,
      compact: false,
      onDraft: vi.fn(),
      onSubmit: vi.fn()
    });
    const input = screen.getByRole('searchbox', { name: 'Search everything' }) as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'pipeline' } });

    await rerender({ mode: 'semantic' });

    expect(input.value).toBe('pipeline');
    expect(screen.getByRole('radio', { name: 'Semantic' }).getAttribute('aria-checked')).toBe('true');
  });
});
