import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryReviewController } from '../../directory/review-controller.svelte';
import PossibleDuplicates from './PossibleDuplicates.svelte';

it('asks to confirm a merged pair with its refreshed token and retained note, then shows History', async () => {
  const row = { id: 17, left_id: 1, left_kind: 'person', right_id: 2, right_kind: 'participant', basis: 'stable_provider_id', state: 'candidate', source: 'user', actionable: true, application_pending: false, review_token: 'after-profile-merge', evidence: [], created_at: '2026-07-19T10:00:00Z', updated_at: '2026-07-19T10:00:00Z' };
  let accepted = false, unavailable = true, merged = false;
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    if (new URL(request.url).pathname.endsWith('/identity/match-candidates')) {
      if (unavailable) { unavailable = false; return Response.json({ message: 'Queue unavailable' }, { status: 503 }); }
      return Response.json({ candidates: accepted && !new URL(request.url).searchParams.get('state')?.includes('accepted') ? [] : [{ ...row, review_token: merged ? 'after-profile-merge' : 'before-profile-merge', state: accepted ? 'accepted' : 'candidate' }], limit: 10, offset: 0 });
    }
    return Response.json({ id: 1, display_name: 'Synthetic person', email_address: 'person@example.com', ...(new URL(request.url).pathname.includes('/participants/') ? { activity_count: 3, identifiers: [{ participant_id: 2, participant_display_name: 'Absorbed person', type: 'email', value: 'absorbed@example.com', is_primary: true }] } : {}) });
  }));
  await controller.loadIdentityPage();
  controller.setDecisionDraft(17, 'Same person');
  const onDecide = vi.fn();
  const view = render(PossibleDuplicates, { controller, onDecide });
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByRole('button', { name: 'Merge' });
    await screen.findByText('3 messages');
    await screen.findByText('absorbed@example.com');
    expect(screen.queryByText('0 messages')).toBeNull();
    await fireEvent.click(screen.getByText('Evidence and notes'));
    await screen.findByText('Same provider identity');
    merged = true;
    await controller.completePersonMerge(17, controller.reviewContextSnapshot(), { result: { cache_state: 'ready' }, survivor: { id: 7, display_name: 'Synthetic survivor' } } as never);
    await screen.findByText('Profiles merged into Synthetic survivor. Choose Merge to record this pair.');
    const merge = screen.getByRole('button', { name: 'Merge' });
    await waitFor(() => expect(document.activeElement).toBe(merge));
    await screen.findByText('Absorbed person');
    await fireEvent.click(merge);
    expect(onDecide).toHaveBeenCalledWith(expect.objectContaining({ id: 17, review_token: 'after-profile-merge' }), 'accept', ['Synthetic person', 'Absorbed person']);
    accepted = true;
    await fireEvent.click(screen.getByRole('radio', { name: 'History' }));
    await screen.findByText(/Merged ·/);
  } finally { view.unmount(); controller.destroy(); }
});
