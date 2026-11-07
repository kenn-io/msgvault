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

it.each([
  { primaryDisabled: false, cancelDisabled: false },
  { primaryDisabled: true, cancelDisabled: false },
  { primaryDisabled: true, cancelDisabled: true }
])('keeps cancellation independent of Save: %j', async ({ primaryDisabled, cancelDisabled }) => {
  const onclose = vi.fn();
  const onprimary = vi.fn();
  render(Dialog, {
    title: 'Describe Archive search', onclose, onprimary, primaryLabel: 'Save', primaryDisabled, cancelDisabled,
    children: createRawSnippet(() => ({ render: () => '<p>Describe this saved view.</p>' }))
  });
  const save = screen.getByRole('button', { name: 'Save' }) as HTMLButtonElement;
  expect(save.disabled).toBe(primaryDisabled);
  const cancel = screen.getByRole('button', { name: 'Cancel' }) as HTMLButtonElement;
  expect(cancel.disabled).toBe(cancelDisabled);
  expect(cancel.compareDocumentPosition(save) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  cancel.click();
  expect(onclose).toHaveBeenCalledTimes(cancelDisabled ? 0 : 1);
  expect(onprimary).not.toHaveBeenCalled();
});
