import { appShortcuts, initShortcuts } from '@kenn-io/kit-ui';
import { tick } from 'svelte';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';

import { view } from './fixtures.test-support';
import SavedViewRow from './SavedViewRow.svelte';

function renderRow(saved = view(1, 'Budget threads'), overrides: Record<string, unknown> = {}) {
  return render(SavedViewRow, { view: saved, onOpen: vi.fn(), onRename: vi.fn(async () => undefined), onDelete: vi.fn(), ...overrides });
}

it('shows what a saved view holds in its tooltip', () => {
  const saved = view(1, 'Budget threads');
  saved.description = 'Every thread about the Q3 budget';
  saved.canonical_state = { ...(saved.canonical_state as object), query: 'budget' };
  renderRow(saved);
  expect(screen.getByRole('button', { name: 'Budget threads' }).getAttribute('title')?.split('\n')).toEqual([
    expect.stringMatching(/\S/), 'Every thread about the Q3 budget',
  ]);
});

it('keeps a view with invalid state disabled and deletable', async () => {
  const incompatible = { schema_version: 2, incompatibility_reason: 'invalid saved view canonical state: semantic and hybrid exploration require free text', canonical_state: { presentation: 'files', search_mode: 'semantic', filters: [] } };
  const onOpenView = vi.fn();
  const onDeleteView = vi.fn();
  renderRow({ ...view(1, 'Old view'), ...incompatible }, { onOpen: onOpenView, onDelete: onDeleteView });
  const saved = screen.getByRole('button', { name: 'Old view' });
  expect(saved.getAttribute('aria-disabled')).toBe('true');
  await fireEvent.click(saved);
  expect(onOpenView).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole('button', { name: 'Old view actions' }));
  expect(screen.queryByRole('menuitem', { name: 'Rename' })).toBeNull();
  await fireEvent.click(screen.getByRole('menuitem', { name: 'Delete' }));
  expect(onDeleteView).toHaveBeenCalledOnce();
});

it.each(['save', 'move'])('locks a pending rename and preserves focus through %s', async ending => {
  let finish!: () => void;
  const pending = new Promise<undefined>(resolve => { finish = () => resolve(undefined); });
  const onRenameView = vi.fn(() => pending);
  const saved = view(1, 'Budget threads');
  const { container } = renderRow(saved, { onRename: onRenameView });

  await fireEvent.click(screen.getByRole('button', { name: 'Budget threads actions' }));
  await fireEvent.click(screen.getByRole('menuitem', { name: 'Rename' }));
  const input = (await screen.findByRole('textbox', { name: 'Rename Budget threads' })) as HTMLInputElement;
  await fireEvent.input(input, { target: { value: 'Q3 budget' } });
  await fireEvent.keyDown(input, { key: 'Enter' });
  await waitFor(() => expect(onRenameView).toHaveBeenCalledWith(saved, 'Q3 budget'));
  expect(input.readOnly).toBe(true);
  await fireEvent.keyDown(input, { key: 'Enter' });
  expect(onRenameView).toHaveBeenCalledOnce();
  const files = document.createElement('button');
  files.textContent = 'Files';
  container.appendChild(files);
  if (ending === 'move') files.focus();
  finish();
  await waitFor(() => expect(screen.queryByRole('textbox', { name: /Rename/ })).toBeNull());
  await tick();
  expect(document.activeElement).toBe(ending === 'move' ? files : screen.getByRole('button', { name: 'Budget threads' }));
});

it.each(['name', 'description'] as const)('keeps the original %s revision when a refresh updates an open editor', async field => {
  const saved = { ...view(1, 'Budget threads'), description: 'Original description' };
  const onRenameView = vi.fn(async () => 'Review the latest view.');
  const onDescribeView = vi.fn(async () => 'Review the latest view.');
  const { rerender } = renderRow(saved, { onRename: onRenameView, onDescribe: onDescribeView });
  await fireEvent.click(screen.getByRole('button', { name: 'Budget threads actions' }));
  await fireEvent.click(screen.getByRole('menuitem', { name: field === 'name' ? 'Rename' : 'Edit description…' }));
  const editor = await screen.findByRole('textbox', { name: field === 'name' ? 'Rename Budget threads' : 'Description' });
  await fireEvent.input(editor, { target: { value: 'My stale edit' } });
  await rerender({ view: { ...saved, [field]: 'Other browser edit', revision: 2 } });
  if (field === 'name') await fireEvent.keyDown(editor, { key: 'Enter' });
  else await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  expect((await screen.findByRole('alert')).textContent).toContain('Review the latest view.');
  if (field === 'name') expect(document.activeElement).toBe(editor);
  const handler = field === 'name' ? onRenameView : onDescribeView;
  expect(handler).toHaveBeenCalledWith(saved, 'My stale edit');
});

it('Saved view actions typeahead keeps page shortcuts suspended', async () => {
  const detachShortcuts = initShortcuts();
  const pageShortcut = vi.fn();
  const unregister = appShortcuts.register('d', pageShortcut);
  const rendered = renderRow(view(1, 'Budget'));
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'Budget actions' }));
    const first = (await screen.findAllByRole('menuitem'))[0]!;
    first.focus();
    await fireEvent.keyDown(first, { key: 'd' });
    expect(pageShortcut).not.toHaveBeenCalled();
    expect(screen.getByRole('menu').contains(document.activeElement)).toBe(true);
  } finally {
    rendered.unmount();
    unregister();
    detachShortcuts();
  }
});
