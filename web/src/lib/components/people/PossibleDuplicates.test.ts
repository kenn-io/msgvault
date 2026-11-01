import { mergeResult, person } from '../../directory/entity-fixtures.test-support';
import { fireEvent, render, screen } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryReviewController } from '../../directory/review-controller.svelte';
import PossibleDuplicates from './PossibleDuplicates.svelte';
import { pair } from '../shell/fixtures.test-support';

it('renders endpoint summaries and wires Retry and Merge', async () => {
  const row = { ...pair, left_kind: 'person', basis: 'stable_provider_id', review_token: 'after-profile-merge' };
  let unavailable = true;
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    if (new URL(request.url).pathname.endsWith('/identity/match-candidates')) {
      if (unavailable) { unavailable = false; return Response.json({ message: 'Queue unavailable' }, { status: 503 }); }
      return Response.json({ candidates: [row], limit: 10, offset: 0 });
    }
    if (new URL(request.url).pathname === '/api/v1/people/1') return Response.json(person(1));
    if (new URL(request.url).pathname === '/api/v1/participants/2') return Response.json({ id: 2, display_label: 'Absorbed person', activity_count: 3, identifiers: [{ participant_id: 2, participant_display_name: 'Absorbed person', type: 'email', value: 'absorbed@example.com', is_primary: true }] });
    throw new Error(`Unexpected ${request.url}`);
  }));
  await controller.loadIdentityPage();
  const onDecide = vi.fn();
  const view = render(PossibleDuplicates, { controller, onDecide });
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByRole('button', { name: 'Merge' });
    await screen.findByText('3 messages');
    await screen.findByText('absorbed@example.com');
    expect(screen.queryByText('0 messages')).toBeNull();
    await screen.findByText('Same provider identity');
    const merge = screen.getByRole('button', { name: 'Merge' });
    await fireEvent.click(merge);
    expect(onDecide).toHaveBeenCalledWith(expect.objectContaining({ id: 17, review_token: 'after-profile-merge' }), 'accept');
  } finally { view.unmount(); controller.destroy(); }
});

it('disables Merge while the post-merge page reloads', async () => {
  let finish!: (response: Response) => void;
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    if (new URL(request.url).pathname.endsWith('/match-candidates')) return new Promise<Response>(resolve => { finish = resolve; });
    return Response.json({ id: 1, display_label: 'Alpha Example', identifiers: [] });
  }));
  controller.rows = [pair];
  const view = render(PossibleDuplicates, { controller, onDecide: (candidate, decision) => void controller.queueDecision(candidate, decision) });
  const refresh = controller.completePersonMerge(17, { survivor: person(7), result: mergeResult(person(7)), responseETag: '"person-7-r4"' });
  try {
    await vi.waitFor(() => expect(screen.getByRole('button', { name: 'Merge' })).toHaveProperty('disabled', true));
  } finally { finish(Response.json({ candidates: [{ ...pair, review_token: 'refreshed-17' }], offset: 0, limit: 11 })); await refresh; view.unmount(); controller.destroy(); }
});
