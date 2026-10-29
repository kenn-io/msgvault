import { render } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import UndoToast from './UndoToast.svelte';

it('flushes pending work on pagehide and removes the listener on unmount', () => {
  const onExpire = vi.fn();
  const rendered = render(UndoToast, { message: 'View deleted', onUndo: vi.fn(), onExpire });
  window.dispatchEvent(new Event('pagehide'));
  expect(onExpire).toHaveBeenCalledTimes(1);
  rendered.unmount();
  window.dispatchEvent(new Event('pagehide'));
  expect(onExpire).toHaveBeenCalledTimes(1);
});
