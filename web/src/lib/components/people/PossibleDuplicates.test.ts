import { fireEvent, render, screen } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryReviewController } from '../../directory/review-controller.svelte';
import PossibleDuplicates from './PossibleDuplicates.svelte';
import { pair } from '../shell/fixtures.test-support';

it.each([
  { state: 'candidate', leftKind: 'participant', pending: false, actionable: true, blocker: undefined, mergeDisabled: false },
  { state: 'candidate', leftKind: 'participant', pending: true, actionable: true, blocker: undefined, mergeDisabled: true },
  { state: 'candidate', leftKind: 'person', pending: false, actionable: false, blocker: 'endpoint_unsupported', mergeDisabled: true },
  { state: 'conflict', leftKind: 'participant', pending: false, actionable: false, blocker: 'person_merge_required', mergeDisabled: false },
])('renders summaries and decisions for $state, pending=$pending, blocker=$blocker', async ({ state, leftKind, pending, actionable, blocker, mergeDisabled }) => {
  const row = { ...pair, state, left_kind: leftKind, actionable, blocker, basis: 'stable_provider_id', review_token: 'displayed-token' };
  const controller = new DirectoryReviewController(createAPIClient(async () => { throw new Error('Unexpected request'); }));
  controller.rows = [row];
  if (pending) controller.pendingDecisions.add(row.id);
  controller.summaries.set(`${leftKind}:1`, { id: 1, name: 'Observed example', address: 'observed@example.com', count: 1, lastAt: null });
  controller.summaries.set('participant:2', { id: 2, name: 'Absorbed person', address: 'absorbed@example.com', count: 3, lastAt: null });
  const onDecide = vi.fn();
  const view = render(PossibleDuplicates, { controller, onDecide });
  try {
    await screen.findByText('Observed example');
    await screen.findByText('absorbed@example.com');
    await screen.findByText(/Same provider identity/);
    const merge = screen.getByRole('button', { name: 'Merge' });
    const separate = screen.getByRole('button', { name: 'Keep separate' });
    expect((merge as HTMLButtonElement).disabled).toBe(mergeDisabled);
    expect((separate as HTMLButtonElement).disabled).toBe(pending);
    if (blocker) expect(screen.getByText(blocker === 'person_merge_required' ? /Both are saved as different people/ : /Endpoint unsupported/)).toBeDefined();
    if (pending) expect(screen.getByText('Decision pending…')).toBeDefined();
    if (!mergeDisabled) {
      await fireEvent.click(merge);
      expect(onDecide).toHaveBeenCalledWith(expect.objectContaining({ id: 17, review_token: 'displayed-token' }), 'accept');
    }
    if (!pending) {
      await fireEvent.click(separate);
      expect(onDecide).toHaveBeenCalledWith(expect.objectContaining({ id: 17, review_token: 'displayed-token' }), 'reject');
    }
  } finally { view.unmount(); controller.destroy(); }
});

it('wires Retry for an unavailable empty queue', async () => {
  const controller = new DirectoryReviewController(createAPIClient(async () => { throw new Error('Unexpected request'); }));
  controller.error = 'Queue unavailable';
  const retry = vi.spyOn(controller, 'retryPage').mockResolvedValue();
  const view = render(PossibleDuplicates, { controller, onDecide: vi.fn() });
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(retry).toHaveBeenCalledOnce();
  } finally { view.unmount(); controller.destroy(); }
});

it.each([
  { pending: false, label: /^Merged ·/, error: 'Queue refresh failed' },
  { pending: true, label: /^Merge pending ·/, error: null },
])('keeps an accepted pair read-only with pending=$pending', ({ pending, label, error }) => {
  const controller = new DirectoryReviewController(createAPIClient(async () => { throw new Error('Unexpected request'); }));
  controller.identityState = 'accepted';
  controller.rows = [{ ...pair, state: 'accepted', application_pending: pending, actionable: false }];
  controller.pageError = error;
  const view = render(PossibleDuplicates, { controller, onDecide: vi.fn() });
  try {
    if (error) expect(screen.getByRole('alert').textContent).toContain(error);
    expect(screen.queryByRole('button', { name: 'Keep separate' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Merge' })).toBeNull();
    expect(screen.getByText(label)).toBeDefined();
  } finally { view.unmount(); controller.destroy(); }
});
