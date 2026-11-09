import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { createRawSnippet, mount, unmount } from 'svelte';
import { appShortcuts, SelectDropdown } from '@kenn-io/kit-ui';
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

it('gives the dialog its own shortcut scope', async () => {
  const rootShortcut = vi.fn();
  const unregister = appShortcuts.register('x', rootShortcut);
  const onclose = vi.fn();
  const view = render(Dialog, { title: 'Delete job?', onclose, children: createRawSnippet(() => ({ render: () => '<p>Delete this job?</p>' })) });
  try {
    expect(appShortcuts.activeScope()).toMatch(/^shell-dialog:/);
    appShortcuts.handleKeydown(new KeyboardEvent('keydown', { key: 'x', cancelable: true }));
    expect(rootShortcut).not.toHaveBeenCalled();
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));
    await waitFor(() => expect(onclose).toHaveBeenCalledOnce());
    view.unmount();
    expect(appShortcuts.activeScope()).toBe('root');
  } finally {
    view.unmount();
    unregister();
  }
});

it.each([
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
});

it('disables dismissal and Back during a write, then restores both', async () => {
  const onclose = vi.fn(), onback = vi.fn();
  const view = render(Dialog, {
    title: 'Split Example Person', onclose, onback, busy: true,
    primaryLabel: 'Split into new person', onprimary: vi.fn(),
    children: createRawSnippet(() => ({ render: () => '<p>Selected addresses</p>' })),
  });
  for (const name of ['Back', 'Close', 'Cancel']) {
    const button = screen.getByRole('button', { name }) as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    button.click();
  }
  await fireEvent.keyDown(document, { key: 'Escape' });
  await fireEvent.click(view.container.querySelector('.kit-modal-overlay')!);
  expect(onclose).not.toHaveBeenCalled();
  expect(onback).not.toHaveBeenCalled();
  await view.rerender({ busy: false });
  await fireEvent.click(screen.getByRole('button', { name: 'Back' }));
  expect(onback).toHaveBeenCalledOnce();
  await fireEvent.keyDown(document, { key: 'Escape' });
  expect(onclose).toHaveBeenCalledOnce();
  view.unmount();
});

it('shows a notice before the footer and leaves a receipt with Done alone', () => {
  render(Dialog, {
    title: 'Split complete', onclose: vi.fn(), width: 'small',
    primaryLabel: 'Done', onprimary: vi.fn(), cancellable: false,
    children: createRawSnippet(() => ({ render: () => '<p>New person created</p>' })),
    notice: createRawSnippet(() => ({ render: () => '<p>Some fields stayed with the original person.</p>' })),
  });
  const done = screen.getByRole('button', { name: 'Done' });
  expect(screen.getByRole('dialog', { name: 'Split complete' })).toBeDefined();
  expect(screen.queryByRole('button', { name: 'Cancel' })).toBeNull();
  expect(screen.getByText('Some fields stayed with the original person.').compareDocumentPosition(done) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
});

it('keeps focus inside the dialog when a step removes its focused control', async () => {
  render(Dialog, {
    title: 'History', onclose: vi.fn(),
    children: createRawSnippet(() => ({ render: () => '<button>Open merge</button>', setup: target => {
      const button = target as HTMLButtonElement;
      const advance = () => button.remove();
      button.addEventListener('click', advance);
      return () => button.removeEventListener('click', advance);
    } })),
  });
  const row = screen.getByRole('button', { name: 'Open merge' });
  row.focus();
  await fireEvent.click(row);
  await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('heading', { name: 'History' })));
});
