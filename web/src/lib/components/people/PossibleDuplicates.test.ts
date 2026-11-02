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
  if (pending) {
    controller.rows.push({ ...pair, id: 18 });
    controller.pendingDecisions.add(18);
    controller.hidden.add(18);
  }
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

it.each([
  { failure: 'queue', message: 'Queue unavailable', action: 'Retry' },
  { failure: 'page', message: 'Queue refresh failed', action: 'Retry page' },
])('displays the $failure error and wires $action', async ({ failure, message, action }) => {
  const controller = new DirectoryReviewController(createAPIClient(async () => { throw new Error('Unexpected request'); }));
  if (failure === 'queue') controller.error = message;
  else { controller.rows = [pair]; controller.pageError = message; }
  const retry = vi.spyOn(controller, 'retryPage').mockResolvedValue();
  const view = render(PossibleDuplicates, { controller, onDecide: vi.fn() });
  try {
    if (failure === 'page') expect(screen.getByRole('alert').textContent).toContain(message);
    else expect(screen.getByText(message)).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: action }));
    expect(retry).toHaveBeenCalledOnce();
  } finally { view.unmount(); controller.destroy(); }
});

it.each([
  { state: 'rejected', pending: false, label: /^Kept separate ·/ },
  { state: 'accepted', pending: false, label: /^Merged ·/ },
  { state: 'accepted', pending: true, label: /^Merge pending ·/ },
] as const)('keeps $state read-only with pending=$pending', ({ state, pending, label }) => {
  const controller = new DirectoryReviewController(createAPIClient(async () => { throw new Error('Unexpected request'); }));
  controller.rows = [{ ...pair, state, application_pending: pending, actionable: false }];
  const view = render(PossibleDuplicates, { controller, onDecide: vi.fn() });
  try {
    expect(screen.queryByRole('button', { name: 'Keep separate' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Merge' })).toBeNull();
    expect(screen.getByText(label)).toBeDefined();
  } finally { view.unmount(); controller.destroy(); }
});
