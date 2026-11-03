import { fireEvent, render, screen } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import PersonHeader from './PersonHeader.svelte';

it('offers one keyboard stop to open details from the avatar and name', async () => {
  const onOpenDetails = vi.fn();
  const view = render(PersonHeader, { name: 'Example Person', onOpenDetails });
  try {
    expect(screen.getAllByRole('button').filter(button => button.tabIndex >= 0)).toHaveLength(1);
    await fireEvent.click(screen.getByRole('button', { name: 'Example Person' }));
    expect(onOpenDetails).toHaveBeenCalledOnce();
    await fireEvent.click(screen.getByRole('button', { name: 'Open details' }));
    expect(onOpenDetails).toHaveBeenCalledTimes(2);
  } finally { view.unmount(); }
});

it('shows the email once when it is also the person heading', async () => {
  const onOpenDetails = vi.fn();
  const view = render(PersonHeader, { props: { name: 'alice@example.com', email: 'alice@example.com', onOpenDetails } });
  expect(screen.getAllByText('alice@example.com')).toHaveLength(1);
  await fireEvent.click(screen.getByRole('button', { name: 'alice@example.com' }));
  expect(onOpenDetails).toHaveBeenCalledOnce();

  await view.rerender({ name: 'Alice Example' });
  expect(screen.getByRole('heading', { name: 'Alice Example' })).toBeDefined();
  expect(screen.getByText('alice@example.com')).toBeDefined();

  await view.rerender({ email: '' });
  expect(screen.getByRole('heading', { name: 'Alice Example' })).toBeDefined();
  expect(screen.queryByText('alice@example.com')).toBeNull();
});
