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
