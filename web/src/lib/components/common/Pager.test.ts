import { fireEvent, render, screen } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import { expect, it, vi } from 'vitest';
import Pager from './Pager.svelte';

it('pages through visible ranges and disappears when everything fits', async () => {
  const onprevious = vi.fn(), onnext = vi.fn();
  const view = render(Pager, { offset: 25, count: 25, hasNext: true, onprevious, onnext });
  expect(screen.getByText('26–50')).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Previous page' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Next page' }));
  expect(onprevious).toHaveBeenCalledOnce();
  expect(onnext).toHaveBeenCalledOnce();
  await view.rerender({ offset: 50, count: 3, hasNext: false });
  expect(screen.getByText('51–53')).toBeDefined();
  expect(screen.getByRole('button', { name: 'Next page' })).toHaveProperty('disabled', true);
  await view.rerender({ offset: 100, count: 0, hasNext: false });
  expect(screen.queryByText('0–0')).toBeNull();
  expect(screen.getByRole('navigation', { name: 'Pages' })).toBeDefined();
  expect(screen.getByRole('button', { name: 'Previous page' })).toHaveProperty('disabled', false);
  await view.rerender({ offset: 0, count: 12, hasNext: false });
  expect(screen.queryByRole('navigation')).toBeNull();
});
