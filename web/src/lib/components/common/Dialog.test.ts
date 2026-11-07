import { fireEvent, render, screen } from '@testing-library/svelte';
import { createRawSnippet, mount, unmount } from 'svelte';
import { SelectDropdown } from '@kenn-io/kit-ui';
import { expect, it, vi } from 'vitest';
import Dialog from './Dialog.svelte';

it('leaves Escape to a child picker, then closes the dialog', async () => {
  const onclose = vi.fn();
  render(Dialog, { title: 'Export meeting', onclose, children: createRawSnippet(() => ({ render: () => '<div></div>', setup: (target) => {
    const picker = mount(SelectDropdown, { target, props: { title: 'Status', value: 'all', options: [{ value: 'all', label: 'All actions' }], onchange: vi.fn() } });
    return () => { void unmount(picker); };
  } })) });
  const picker = screen.getByRole('combobox', { name: /^Status:/ });
  await fireEvent.click(picker);
  await screen.findByRole('listbox');
  await fireEvent.keyDown(picker, { key: 'Escape' });
  expect(onclose).not.toHaveBeenCalled();
  expect(screen.queryByRole('listbox')).toBeNull();
  await fireEvent.keyDown(picker, { key: 'Escape' });
  expect(onclose).toHaveBeenCalledOnce();
});

it('puts Cancel before the primary action and closes with it', async () => {
  const onclose = vi.fn();
  const onprimary = vi.fn();
  render(Dialog, { title: 'Describe view', onclose, onprimary, primaryLabel: 'Save', children: createRawSnippet(() => ({ render: () => '<p>Body</p>' })) });
  const cancel = screen.getByRole('button', { name: 'Cancel' });
  const save = screen.getByRole('button', { name: 'Save' });
  expect(cancel.compareDocumentPosition(save) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  await fireEvent.click(cancel);
  expect(onclose).toHaveBeenCalledOnce();
  expect(onprimary).not.toHaveBeenCalled();
});

it('shows the primary alone when the dialog is not cancellable', () => {
  render(Dialog, { title: 'Review deletion', onclose: vi.fn(), primaryLabel: 'Done', cancellable: false, children: createRawSnippet(() => ({ render: () => '<p>Body</p>' })) });
  expect(screen.getByRole('button', { name: 'Done' })).toBeDefined();
  expect(screen.queryByRole('button', { name: 'Cancel' })).toBeNull();
});
