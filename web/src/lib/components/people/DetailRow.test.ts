import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import { afterEach, expect, it, vi } from 'vitest';
import DetailRow from './DetailRow.svelte';

afterEach(cleanup);
it('names its menu for the row, disables unavailable actions, and calls the selected action', async () => {
  const edit = vi.fn(), remove = vi.fn(), primary = vi.fn();
  render(DetailRow, {
    actionLabel: 'Engineer at Example Org', children: createRawSnippet(() => ({ render: () => '<span>Engineer at Example Org</span>' })),
    menu: [{ label: 'Edit…', onselect: edit }, { label: 'Make primary', onselect: primary, disabled: true }, { label: 'Delete…', onselect: remove, danger: true }],
  });
  await fireEvent.click(screen.getByRole('button', { name: 'Actions for Engineer at Example Org' }));
  const disabled = screen.getByRole('menuitem', { name: 'Make primary' });
  expect(disabled.getAttribute('aria-disabled')).toBe('true');
  await fireEvent.click(disabled);
  expect(primary).not.toHaveBeenCalled();
  const deletion = screen.getByRole('menuitem', { name: 'Delete…' });
  expect(deletion.className).toContain('danger');
  await fireEvent.click(deletion);
  expect(remove).toHaveBeenCalledOnce();
  expect(edit).not.toHaveBeenCalled();
});

it('shows a lone action as its own button', async () => {
  const remove = vi.fn();
  render(DetailRow, { children: createRawSnippet(() => ({ render: () => '<span>Mira Vale</span>' })), menu: [{ label: 'Delete job…', onselect: remove }] });
  await fireEvent.click(screen.getByRole('button', { name: 'Delete job…' }));
  expect(remove).toHaveBeenCalledOnce();
});
