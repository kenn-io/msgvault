import { fireEvent, render, screen } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryReviewController } from '../../directory/review-controller.svelte';
import PossibleDuplicates from './PossibleDuplicates.svelte';
import { pair } from '../shell/fixtures.test-support';

it('renders endpoint summaries and wires Retry and both decisions', async () => {
  const row = { ...pair, left_kind: 'person', basis: 'stable_provider_id', review_token: 'displayed-token' };
  let unavailable = true;
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    if (new URL(request.url).pathname.endsWith('/identity/match-candidates')) {
      if (unavailable) { unavailable = false; return Response.json({ message: 'Queue unavailable' }, { status: 503 }); }
      return Response.json({ candidates: [row], limit: 10, offset: 0 });
    }
    throw new Error(`Unexpected ${request.url}`);
  }));
  await controller.loadIdentityPage();
  controller.summaries.set('person:1', { id: 1, name: 'Saved example', address: 'Saved person', lastAt: null });
  controller.summaries.set('participant:2', { id: 2, name: 'Absorbed person', address: 'absorbed@example.com', count: 3, lastAt: null });
  const onDecide = vi.fn();
  const view = render(PossibleDuplicates, { controller, onDecide });
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByRole('button', { name: 'Merge' });
    await screen.findByText('Saved example');
    await screen.findByText('absorbed@example.com');
    await screen.findByText('Same provider identity');
    const merge = screen.getByRole('button', { name: 'Merge' });
    await fireEvent.click(merge);
    expect(onDecide).toHaveBeenCalledWith(expect.objectContaining({ id: 17, review_token: 'displayed-token' }), 'accept');
    await fireEvent.click(screen.getByRole('button', { name: 'Keep separate' }));
    expect(onDecide).toHaveBeenCalledWith(expect.objectContaining({ id: 17, review_token: 'displayed-token' }), 'reject');
  } finally { view.unmount(); controller.destroy(); }
});
