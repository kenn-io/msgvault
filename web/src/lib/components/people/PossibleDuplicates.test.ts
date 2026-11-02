import { fireEvent, render, screen } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryReviewController } from '../../directory/review-controller.svelte';
import PossibleDuplicates from './PossibleDuplicates.svelte';
import { pair } from '../shell/fixtures.test-support';

it('renders endpoint summaries and wires Retry and both decisions', async () => {
  const row = { ...pair, basis: 'stable_provider_id', review_token: 'displayed-token' };
  const controller = new DirectoryReviewController(createAPIClient(async () => { throw new Error('Unexpected request'); }));
  controller.rows = [row];
  controller.error = 'Queue unavailable';
  const retry = vi.spyOn(controller, 'retryPage').mockImplementation(async () => { controller.error = null; });
  controller.summaries.set('participant:1', { id: 1, name: 'Observed example', address: 'observed@example.com', count: 1, lastAt: null });
  controller.summaries.set('participant:2', { id: 2, name: 'Absorbed person', address: 'absorbed@example.com', count: 3, lastAt: null });
  const onDecide = vi.fn();
  const view = render(PossibleDuplicates, { controller, onDecide });
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(retry).toHaveBeenCalledOnce();
    await screen.findByText('Observed example');
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
  const controller = new DirectoryReviewController(createAPIClient(async () => { throw new Error('Unexpected request'); }));
  controller.rows = [{ ...pair, state: 'accepted', actionable: false }];
  controller.pageError = 'Queue refresh failed';
  const view = render(PossibleDuplicates, { controller, onDecide: vi.fn() });
  try {
    expect(screen.getByRole('alert').textContent).toContain('Queue refresh failed');
    expect(screen.queryByRole('button', { name: 'Keep separate' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Merge' })).toBeNull();
    expect(screen.getByText(/Merged/)).toBeDefined();
  } finally { view.unmount(); controller.destroy(); }
});
