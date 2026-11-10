import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import { afterEach, expect, it, vi } from 'vitest';
import DetailSection from './DetailSection.svelte';

afterEach(cleanup);
it('keeps Add in the header, history folded, and the subgroup after the rows', async () => {
  const onadd = vi.fn();
  const view = render(DetailSection, {
    title: 'Connections', addLabel: 'Add connection…', onadd,
    children: createRawSnippet(() => ({ render: () => '<p>Current connection</p>' })),
    historyLabel: '2 past connections', history: createRawSnippet(() => ({ render: () => '<p>Past connections</p>' })),
    subgroupTitle: 'From address books', subgroup: createRawSnippet(() => ({ render: () => '<p>Suggested (2)</p>' })),
  });
  const add = screen.getByRole('button', { name: 'Add connection…' });
  expect(add.closest('header')).not.toBeNull();
  await fireEvent.click(add);
  expect(onadd).toHaveBeenCalledOnce();
  const fold = screen.getByText('2 past connections').closest('details')!;
  expect(fold.open).toBe(false);
  await fireEvent.click(screen.getByText('2 past connections'));
  expect(fold.open).toBe(true);
  expect(screen.getByRole('heading', { name: 'From address books' })).toBeDefined();
  expect(view.container.textContent).toMatch(/Current connection.*2 past connections.*Past connections.*From address books.*Suggested/s);
});
