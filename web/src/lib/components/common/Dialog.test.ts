import { fireEvent, render, screen } from '@testing-library/svelte';
import { createRawSnippet, mount, unmount } from 'svelte';
import { SelectDropdown } from '@kenn-io/kit-ui';
import { expect, it, vi } from 'vitest';
import Dialog from './Dialog.svelte';

it('calls the owner when Close is clicked', async () => {
  const onclose = vi.fn();
  render(Dialog, { title: 'Archive choices', onclose, children: createRawSnippet(() => ({ render: () => '<p>Choices</p>' })) });
  await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
  expect(onclose).toHaveBeenCalledOnce();
});

it('preserves the caller footer actions', () => {
  render(Dialog, {
    title: 'Cancel batch', onclose: vi.fn(),
    children: createRawSnippet(() => ({ render: () => '<div><p>The staged batch</p><button>Keep staged</button></div>' })),
    primaryLabel: 'Cancel batch', onprimary: vi.fn()
  });
  expect(screen.getByRole('button', { name: 'Keep staged' })).toBeDefined();
  expect(screen.getByRole('button', { name: 'Cancel batch' })).toBeDefined();
  expect(screen.queryByRole('button', { name: 'Cancel' })).toBeNull();
});

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
