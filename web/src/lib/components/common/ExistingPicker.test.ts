import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { describe, expect, it, vi } from 'vitest';
import ExistingPicker from './ExistingPicker.svelte';

const item = { id: 'one', label: 'Send the budget' };

describe('ExistingPicker', () => {
  it('loads immediately, filters, and ignores late replies', async () => {
    let older!: (items: { items: typeof item[]; truncated: boolean }) => void;
    let fail!: (error: Error) => void;
    const load = vi.fn((query: string, _signal: AbortSignal): Promise<{ items: typeof item[]; truncated: boolean }> => {
      if (query === 'old') return new Promise(resolve => { older = resolve; });
      if (query === 'broken') return new Promise((_resolve, reject) => { fail = reject; });
      return Promise.resolve({ items: query ? [] : [item], truncated: !query });
    });
    render(ExistingPicker, { load, noun: 'tasks', empty: 'No tasks yet.', moreNote: 'More tasks available.' });
    await fireEvent.click(await screen.findByRole('radio', { name: item.label }));
    expect(load.mock.calls[0][0]).toBe('');
    expect(screen.getByRole('radio', { name: item.label })).toHaveProperty('checked', true);
    expect(screen.getByText('More tasks available.')).toBeDefined();
    const filter = screen.getByRole('searchbox', { name: 'Filter tasks' });
    await fireEvent.input(filter, { target: { value: 'old' } });
    expect(screen.queryByRole('radio', { name: item.label })).toBeNull();
    expect(screen.queryByText('More tasks available.')).toBeNull();
    await waitFor(() => expect(older).toBeTypeOf('function'));
    expect(screen.getByRole('status').textContent).toBe('Loading…');
    await fireEvent.input(filter, { target: { value: 'missing' } });
    await screen.findByText('No tasks match "missing".');
    expect(load.mock.calls[1][1].aborted).toBe(true);
    older({ items: [item], truncated: true });
    await tick();
    expect(screen.getByText('No tasks match "missing".')).toBeDefined();
    expect(screen.queryByRole('radio')).toBeNull();
    await fireEvent.input(filter, { target: { value: '' } });
    expect(await screen.findByRole('radio', { name: item.label })).toHaveProperty('checked', false);
    await fireEvent.input(filter, { target: { value: 'broken' } });
    await waitFor(() => expect(fail).toBeTypeOf('function'));
    fail(new Error('Task service unavailable'));
    expect((await screen.findByRole('alert')).textContent).toBe('Task service unavailable');
  });

  it('shows an empty first list', async () => {
    render(ExistingPicker, { load: async () => ({ items: [], truncated: false }), noun: 'open issues', empty: 'No open issues yet.' });
    expect(await screen.findByText('No open issues yet.')).toBeDefined();
  });
});
