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

it('keeps an accepted pair read-only when refreshing the queue fails', async () => {
  let reads = 0;
  const writes: string[] = [];
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (request.method === 'POST') {
      writes.push(path);
      return Response.json({ candidate: { ...pair, state: 'accepted', actionable: false }, identity_revision: 2, cache_state: 'ready' });
    }
    if (path.endsWith('/identity/match-candidates')) return ++reads === 1
      ? Response.json({ candidates: [pair], limit: 10, offset: 0 })
      : Response.json({ message: 'Queue refresh failed' }, { status: 503 });
    if (/\/participants\/[12]$/.test(path)) return Response.json({ id: Number(path.split('/').at(-1)), display_label: 'Synthetic person', identifiers: [], activity_count: 1 });
    throw new Error(`Unexpected ${request.method} ${path}`);
  }));
  await controller.loadIdentityPage();
  const view = render(PossibleDuplicates, { controller, onDecide: (candidate, decision) => void controller.queueDecision(candidate, decision) });
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    await controller.settleDecision();
    await screen.findByText('Queue refresh failed');
    expect(controller.rows[0]?.state).toBe('accepted');
    expect(writes).toEqual(['/api/v1/identity/match-candidates/17/review/accept']);
    expect(screen.queryByRole('button', { name: 'Keep separate' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Merge' })).toBeNull();
    expect(screen.getByText(/Merged/)).toBeDefined();
  } finally { view.unmount(); controller.destroy(); }
});
