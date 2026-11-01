import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryReviewController } from '../../directory/review-controller.svelte';
import PossibleDuplicates from './PossibleDuplicates.svelte';

it('renders endpoint summaries and wires Retry, Merge, and History', async () => {
  const row = { id: 17, left_id: 1, left_kind: 'person', right_id: 2, right_kind: 'participant', basis: 'stable_provider_id', state: 'candidate', source: 'user', actionable: true, application_pending: false, review_token: 'after-profile-merge', evidence: [], created_at: '2026-07-19T10:00:00Z', updated_at: '2026-07-19T10:00:00Z' };
  let accepted = false, unavailable = true;
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    if (new URL(request.url).pathname.endsWith('/identity/match-candidates')) {
      if (unavailable) { unavailable = false; return Response.json({ message: 'Queue unavailable' }, { status: 503 }); }
      return Response.json({ candidates: accepted && !new URL(request.url).searchParams.get('state')?.includes('accepted') ? [] : [{ ...row, state: accepted ? 'accepted' : 'candidate' }], limit: 10, offset: 0 });
    }
    return Response.json({ id: 1, display_name: 'Synthetic person', email_address: 'person@example.com', ...(new URL(request.url).pathname.includes('/participants/') ? { activity_count: 3, identifiers: [{ participant_id: 2, participant_display_name: 'Absorbed person', type: 'email', value: 'absorbed@example.com', is_primary: true }] } : {}) });
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
    accepted = true;
    await fireEvent.click(screen.getByRole('radio', { name: 'History' }));
    await screen.findByText(/Merged ·/);
  } finally { view.unmount(); controller.destroy(); }
});

const pair = { id: 17, left_id: 1, left_kind: 'participant', right_id: 2, right_kind: 'participant', basis: 'email', source: 'user', state: 'candidate', actionable: true, application_pending: false, review_token: 'review-17', evidence: [], created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z' };

it('waits for refreshed evidence before enabling the post-merge decision', async () => {
  let finish!: (response: Response) => void;
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    if (new URL(request.url).pathname.endsWith('/match-candidates')) return new Promise<Response>(resolve => { finish = resolve; });
    return Response.json({ id: 1, display_label: 'Alpha Example', identifiers: [] });
  }));
  controller.rows = [pair];
  const view = render(PossibleDuplicates, { controller, onDecide: (candidate, decision) => void controller.queueDecision(candidate, decision) });
  const refresh = controller.completePersonMerge(17, controller.reviewContextSnapshot(), { survivor: { id: 7, display_name: 'Alpha Example' }, result: { cache_state: 'ready' } } as never);
  try {
    await vi.waitFor(() => expect(screen.getByRole('button', { name: 'Merge' })).toHaveProperty('disabled', true));
    await controller.queueDecision(pair, 'accept');
    expect(controller.pending).toBeUndefined();
    await expect(controller.acceptIdentity(17)).resolves.toMatchObject({ ok: false });
  } finally { finish(Response.json({ candidates: [{ ...pair, review_token: 'refreshed-17' }], offset: 0, limit: 11 })); await refresh; view.unmount(); controller.destroy(); }
});

it('refreshes another endpoint in the changed cluster after accepting a pair', async () => {
  let accepted = false;
  const aliasPair = { ...pair, id: 18, left_id: 3, right_id: 4, review_token: 'review-18' };
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (request.method === 'POST') { accepted = true; return Response.json({ candidate: { ...pair, state: 'accepted' }, cache_state: 'ready', identity_revision: 2 }); }
    if (path.endsWith('/match-candidates')) return Response.json({ candidates: accepted ? [aliasPair] : [pair, aliasPair], offset: 0, limit: 11 });
    const id = Number(path.split('/').at(-1));
    return Response.json({ id, display_label: id === 3 ? 'Alias Example' : `Participant ${id}`, identifiers: [{ participant_id: id, type: 'email', value: `person-${id}@example.com`, is_primary: true }], activity_count: accepted && id === 3 ? 5 : 1, cluster: { canonical_id: id === 3 ? 2 : id, member_ids: id === 3 ? [2, 3] : [id] } });
  }));
  await controller.loadIdentityPage();
  const view = render(PossibleDuplicates, { controller, onDecide: vi.fn() });
  try {
    await screen.findByText('Participant 4');
    await controller.acceptIdentity(17);
    const alias = (await screen.findByText('Alias Example')).closest('li')!;
    expect(await within(alias).findByText('5 messages')).toBeDefined();
  } finally { view.unmount(); controller.destroy(); }
});
