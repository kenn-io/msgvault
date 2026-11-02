import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryReviewController } from '../../directory/review-controller.svelte';
import PossibleDuplicates from './PossibleDuplicates.svelte';
import { pair } from '../shell/fixtures.test-support';

it('renders endpoint summaries and wires Retry and both decisions', async () => {
  const row = { ...pair, basis: 'stable_provider_id', review_token: 'displayed-token' };
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
  controller.summaries.set('participant:1', { id: 1, name: 'Observed example', address: 'observed@example.com', count: 1, lastAt: null });
  controller.summaries.set('participant:2', { id: 2, name: 'Absorbed person', address: 'absorbed@example.com', count: 3, lastAt: null });
  const onDecide = vi.fn();
  const view = render(PossibleDuplicates, { controller, onDecide });
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByRole('button', { name: 'Merge' });
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

it('Next shows the unseen pair after settling a decision that removes the first row', async () => {
  const remaining = Array.from({ length: 12 }, (_, index) => ({ ...pair, id: index + 1, left_id: (index + 1) * 10, right_id: (index + 1) * 10 + 1, review_token: `pair-${index + 1}` }));
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const url = new URL(request.url);
    if (request.method === 'POST') {
      const id = Number(url.pathname.split('/').at(-3));
      const removed = remaining.splice(remaining.findIndex(row => row.id === id), 1)[0];
      return Response.json({ candidate: { ...removed, state: 'accepted', actionable: false }, cache_state: 'ready' });
    }
    if (url.pathname.endsWith('/identity/match-candidates')) {
      const offset = Number(url.searchParams.get('offset')), limit = Number(url.searchParams.get('limit'));
      return Response.json({ candidates: remaining.slice(offset, offset + limit), offset, limit });
    }
    return Response.json({ id: Number(url.pathname.split('/').at(-1)), display_label: 'Synthetic participant', identifiers: [], activity_count: 1 });
  }));
  await controller.loadIdentityPage();
  const view = render(PossibleDuplicates, { controller, onDecide: (row, decision) => void controller.queueDecision(row, decision) });
  try {
    expect([...document.querySelectorAll('li[id^="duplicate-"]')].map(row => row.id)).toEqual(Array.from({ length: 10 }, (_, index) => `duplicate-${index + 1}`));
    await fireEvent.click(within(document.getElementById('duplicate-1')!).getByRole('button', { name: 'Merge' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    await waitFor(() => expect([...document.querySelectorAll('li[id^="duplicate-"]')].map(row => row.id)).toEqual(['duplicate-11', 'duplicate-12']));
    expect.soft(screen.getByRole('navigation', { name: 'Duplicate pages' }).textContent).toContain('Page 2');
    await fireEvent.click(screen.getByRole('button', { name: 'Previous' }));
    await waitFor(() => expect([...document.querySelectorAll('li[id^="duplicate-"]')].map(row => row.id)).toEqual(Array.from({ length: 10 }, (_, index) => `duplicate-${index + 2}`)));
    expect(screen.getByText('Page 1', { exact: true })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    await waitFor(() => expect([...document.querySelectorAll('li[id^="duplicate-"]')].map(row => row.id)).toEqual(['duplicate-12']));
    expect(screen.getByText('Page 2', { exact: true })).toBeDefined();
  } finally { view.unmount(); controller.destroy(); }
});
