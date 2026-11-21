import { fireEvent, render, screen } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import { expect, it, vi } from 'vitest';
import DetailRow from './DetailRow.svelte';

it('shows how to close an expanded details panel', async () => {
  const content = createRawSnippet(() => ({ render: () => '<span>Content</span>' }));
  render(DetailRow, { children: content, details: content });
  await fireEvent.click(screen.getByRole('button', { name: 'Details' }));
  expect(screen.getByRole('button', { name: 'Hide details' }).getAttribute('aria-expanded')).toBe('true');
});

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

