import { fireEvent, render, screen } from '@testing-library/svelte';
import { createRawSnippet, mount, unmount } from 'svelte';
import { SelectDropdown } from '@kenn-io/kit-ui';
import { expect, it, vi } from 'vitest';
import Dialog from './Dialog.svelte';

it('leaves Escape to a child picker, then closes before the underlying reader', async () => {
  const onclose = vi.fn();
  const readerEscape = vi.fn((event: KeyboardEvent) => { if (!event.defaultPrevented) readerClosed(); });
  const readerClosed = vi.fn();
  window.addEventListener('keydown', readerEscape);
  const view = render(Dialog, { title: 'Export meeting', onclose, children: createRawSnippet(() => ({ render: () => '<div></div>', setup: (target) => {
    const picker = mount(SelectDropdown, { target, props: { title: 'Status', value: 'all', options: [{ value: 'all', label: 'All actions' }], onchange: vi.fn() } });
    return () => { void unmount(picker); };
  } })) });
  try {
    const picker = screen.getByRole('combobox', { name: /^Status:/ });
    await fireEvent.click(picker);
    await screen.findByRole('listbox');
    await fireEvent.keyDown(picker, { key: 'Escape' });
    expect(onclose).not.toHaveBeenCalled();
    expect(screen.queryByRole('listbox')).toBeNull();
    readerClosed.mockClear();
    await fireEvent.keyDown(picker, { key: 'Escape' });
    expect(onclose).toHaveBeenCalledOnce();
    expect(readerClosed).not.toHaveBeenCalled();
  } finally { view.unmount(); window.removeEventListener('keydown', readerEscape); }
});
