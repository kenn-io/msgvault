import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryReviewController } from '../../directory/review-controller.svelte';
import PossibleDuplicates from './PossibleDuplicates.svelte';

it('asks to confirm a merged pair with its refreshed token and retained note, then shows History', async () => {
  const requests: Request[] = [];
  const row = { id: 17, left_id: 1, left_kind: 'person', right_id: 2, right_kind: 'participant', basis: 'display_name', state: 'candidate', source: 'user', actionable: true, application_pending: false, review_token: 'after-profile-merge', evidence: [], created_at: '', updated_at: '' };
  let accepted = false, unavailable = true;
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    requests.push(request.clone());
    if (request.method === 'POST') {
      accepted = true;
      return Response.json({ candidate: { ...row, state: 'accepted' }, cache_state: 'ready' });
    }
    if (new URL(request.url).pathname.endsWith('/identity/match-candidates')) {
      if (unavailable) { unavailable = false; return Response.json({ message: 'Queue unavailable' }, { status: 503 }); }
      return Response.json({ candidates: accepted && !new URL(request.url).searchParams.get('state')?.includes('accepted') ? [] : [{ ...row, state: accepted ? 'accepted' : 'candidate' }], limit: 10, offset: 0 });
    }
    return Response.json({ id: 1, display_name: 'Synthetic person', email_address: 'person@example.com', ...(new URL(request.url).pathname.includes('/participants/') ? { activity_count: 3 } : {}) });
  }));
  await controller.loadIdentityPage();
  controller.setDecisionDraft(17, 'Same person');
  const view = render(PossibleDuplicates, { controller, onDecide: (candidate, decision) => controller.queueDecision(candidate, decision) });
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByRole('button', { name: 'Merge' });
    await screen.findByText('3 messages');
    expect(screen.queryByText('0 messages')).toBeNull();
    await fireEvent.click(screen.getByText('Evidence and notes'));
    expect(screen.queryByText('Decision pending…')).toBeNull();
    await controller.completePersonMerge(17, controller.reviewContextSnapshot(), { result: { cache_state: 'ready' }, survivor: { id: 7, display_name: 'Synthetic survivor' } } as never);
    await screen.findByText('Profiles merged into Synthetic survivor. Choose Merge to record this pair.');
    const merge = screen.getByRole('button', { name: 'Merge' });
    await waitFor(() => expect(document.activeElement).toBe(merge));
    expect(requests.filter(request => request.method === 'POST')).toHaveLength(0);
    await fireEvent.click(merge);
    await controller.settleDecision();
    expect(await requests.find(request => request.method === 'POST')!.json()).toEqual({ review_token: 'after-profile-merge', notes: 'Same person' });
    await fireEvent.click(screen.getByRole('radio', { name: 'History' }));
    await screen.findByText(/Merged ·/);
  } finally { view.unmount(); controller.destroy(); }
});
