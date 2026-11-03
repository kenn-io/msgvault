import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import { mergeResult, person } from '../../directory/entity-fixtures.test-support';
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
  const controller = new DirectoryReviewController(createAPIClient(async () => Response.json({ candidates: Array.from({ length: 11 }, (_, index) => ({ ...row, id: 17 + index })), offset: 10, limit: 11 })));
  await controller.loadIdentityPage(10);
  controller.pageError = 'Queue refresh failed';
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
    for (const name of ['Previous', 'Next', 'Retry page']) expect(screen.getByRole('button', { name })).toHaveProperty('disabled', pending);
    for (const name of ['To review', 'History']) expect(screen.getByRole('radio', { name })).toHaveProperty('disabled', pending);
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

it.each(['observed@example.com', 'Observed example'])('shows an address once when the displayed name is %s', name => {
  const controller = new DirectoryReviewController(createAPIClient(async () => { throw new Error('Unexpected request'); }));
  controller.rows = [pair];
  controller.summaries.set('participant:1', { id: 1, name, address: 'observed@example.com', count: 1, lastAt: null });
  controller.summaries.set('participant:2', { id: 2, name: 'Other example', address: 'other@example.com', count: 1, lastAt: null });
  const view = render(PossibleDuplicates, { controller, onDecide: vi.fn() });
  try {
    expect(screen.getAllByText('observed@example.com')).toHaveLength(1);
    expect(screen.getByText(name)).toBeDefined();
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

it('returns focus to a merged pair once across same-page reloads', async () => {
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    if (path.endsWith('/identity/match-candidates')) return Response.json({ candidates: [pair, { ...pair, id: 18 }], offset: 0, limit: 11 });
    return Response.json({ id: 1, display_label: 'Synthetic person', identifiers: [], activity_count: 3 });
  }));
  controller.rows = [pair, { ...pair, id: 18 }];
  const view = render(PossibleDuplicates, { controller, onDecide: vi.fn() });
  try {
    controller.lastMerge = { candidateID: 17, result: mergeResult(person(7)), survivor: person(7), responseETag: '"person-7-r5"' };
    const firstMerge = within(document.getElementById('duplicate-17')!).getByRole('button', { name: 'Merge' });
    await waitFor(() => expect(document.activeElement).toBe(firstMerge));
    const secondChoice = within(document.getElementById('duplicate-18')!).getByRole('button', { name: 'Keep separate' });
    secondChoice.focus();
    await controller.loadIdentityPage();
    await tick();
    expect(document.activeElement).toBe(secondChoice);
  } finally { view.unmount(); controller.destroy(); }
});

it('keeps a queued decision failure visible after switching to History', async () => {
  let finishDecision!: (response: Response) => void;
  const decisionResponse = new Promise<Response>(resolve => { finishDecision = resolve; });
  const decisions: Request[] = [];
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const url = new URL(request.url);
    if (request.method === 'POST') { decisions.push(request); return decisionResponse; }
    if (url.pathname.endsWith('/identity/match-candidates')) return Response.json({ candidates: url.searchParams.get('state') === 'accepted,rejected' ? [] : [pair], offset: 0, limit: 11 });
    return Response.json({ id: Number(url.pathname.split('/').at(-1)), display_label: 'Synthetic person', identifiers: [], activity_count: 3 });
  }));
  await controller.loadIdentityPage();
  controller.setDecisionDraft(pair.id, 'Keep this note for retry');
  await controller.queueDecision(pair, 'reject');
  const view = render(PossibleDuplicates, { controller, onDecide: vi.fn() });
  try {
    await fireEvent.click(screen.getByRole('radio', { name: 'History' }));
    await waitFor(() => expect(decisions).toHaveLength(1));
    expect(new URL(decisions[0]!.url).pathname).toBe(`/api/v1/identity/match-candidates/${pair.id}/review/reject`);
    expect(await decisions[0]!.json()).toEqual({ review_token: pair.review_token, notes: 'Keep this note for retry' });
    finishDecision(Response.json({ error: 'unavailable', message: 'Decision unavailable. Try again.' }, { status: 503 }));
    await screen.findByText('Nothing decided yet');
    expect(screen.getByRole('alert').textContent).toBe('Decision unavailable. Try again.');
    await fireEvent.click(screen.getByRole('radio', { name: 'To review' }));
    await screen.findByRole('button', { name: 'Keep separate' });
    expect(screen.getByRole('textbox', { name: 'Decision notes', hidden: true })).toHaveProperty('value', 'Keep this note for retry');
    expect(decisions).toHaveLength(1);
  } finally { view.unmount(); controller.destroy(); }
});
