import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { ExploreSelection } from '../../api/generated/models';
import DeletionReview from './DeletionReview.svelte';

const selection: ExploreSelection = {
  mode: 'explicit', predicate: { presentation: 'table' }, row_keys: ['source:1:message:m1'],
  cache_revision: 'cache-1', search_provenance: {}
};

function preflight(overrides: Record<string, unknown> = {}) {
  return {
    count: 3, deletable_count: 2, estimated_bytes: 3 * 1024, cache_revision: 'cache-1', search_provenance: {},
    unavailable_actions: [], operation_token: 'token-1', expires_at: '2026-07-19T10:05:00Z', ...overrides
  };
}

function api(review = preflight()) {
  return createAPIClient(vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path.endsWith('/explore/preflight')) return Response.json(review);
    return Response.json({}, { status: 404 });
  }));
}

afterEach(() => {
  document.body.replaceChildren();
});

it('disables staging and says why when nothing qualifies', async () => {
  const review = preflight({
    deletable_count: 0,
    unavailable_actions: [{ action: 'stage_deletion', reason: 'selection_contains_items_that_cannot_be_deleted_from_source' }]
  });
  render(DeletionReview, { client: api(review), selection, onclose: vi.fn() });
  expect(await screen.findByText('None of the selected items can be deleted from their source.')).toBeDefined();
  const stage = await screen.findByRole('button', { name: 'Stage 0 messages' });
  expect((stage as HTMLButtonElement).disabled).toBe(true);
});

it('keeps all-matching exclusions and its token through failed review load and failed staging', async () => {
  const matching: ExploreSelection = { ...selection, mode: 'all_matching', row_keys: undefined, exclusions: ['message:2'], predicate: { query: 'budget', search_mode: 'semantic', presentation: 'table' }, candidate_snapshot_id: 'snapshot-1' };
  const posts: unknown[] = [];
  let failStage = true, reads = 0;
  const client = createAPIClient(async input => {
    const request = input as Request, body = await request.clone().json();
    if (request.url.endsWith('/preflight')) return ++reads === 1 ? Response.json({ message: 'Unavailable' }, { status: 500 }) : Response.json(preflight({ search_deletion_scope: 'active', operation_token: `token-${reads}` }));
    posts.push(body);
    if (body.dry_run) return Response.json({ dry_run: true, message_count: 2, skipped_count: 1, account: 'outlook@example.com' });
    if (failStage) { failStage = false; return Response.json({ message: 'Review expired' }, { status: 409 }); }
    return Response.json({ id: 'm365-batch', dry_run: false, message_count: 2, skipped_count: 1 }, { status: 201 });
  });
  const onStaged = vi.fn();
  render(DeletionReview, { client, selection: matching, onclose: vi.fn(), onStaged });
  await fireEvent.click(await screen.findByRole('button', { name: 'Try again' }));
  await screen.findByText('2 eligible · 1 skipped');
  await fireEvent.click(screen.getByRole('button', { name: 'Dry run' }));
  expect(await screen.findByText('Dry run: Matched: 3 · Staged: 2 · Skipped: 1 in outlook@example.com')).toBeDefined();
  await waitFor(() => expect((screen.getByRole('button', { name: 'Stage 2 messages' }) as HTMLButtonElement).disabled).toBe(false));
  await fireEvent.click(screen.getByRole('button', { name: 'Stage 2 messages' }));
  await screen.findByText('Review expired');
  expect(screen.queryByText(/^Dry run:/)).toBeNull();
  expect(onStaged).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
  await screen.findByText('2 eligible · 1 skipped');
  await fireEvent.click(screen.getByRole('button', { name: 'Stage 2 messages' }));
  await screen.findByText('m365-batch');
  expect(posts).toEqual([
    { selection: matching, operation_token: 'token-2', dry_run: true },
    { selection: matching, operation_token: 'token-2', dry_run: false, description: 'reviewed matching selection' },
    { selection: matching, operation_token: 'token-3', dry_run: false, description: 'reviewed matching selection' },
  ]);
});

it('handles a pending stage when the review stays mounted, with Cancel until it is done', async () => {
  let finish!: (response: Response) => void;
  const onclose = vi.fn(), onStaged = vi.fn();
  const client = createAPIClient(async input => (input as Request).url.endsWith('/preflight')
    ? Response.json(preflight())
    : new Promise<Response>(resolve => { finish = resolve; }));
  render(DeletionReview, { client, selection, onclose, onStaged });
  const stage = await screen.findByRole('button', { name: 'Stage 2 messages' });
  const cancel = screen.getByRole('button', { name: 'Cancel' });
  expect(cancel.compareDocumentPosition(stage) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  await fireEvent.click(stage);
  await waitFor(() => expect(finish).toBeTypeOf('function'));
  await fireEvent.keyDown(document, { key: 'Escape' });
  await fireEvent.click(cancel);
  expect(onclose).not.toHaveBeenCalled();
  finish(Response.json({ id: 'batch-pending', message_count: 2, matched_count: 3, dry_run: false }, { status: 201 }));
  await screen.findByText('batch-pending');
  expect(onStaged).toHaveBeenCalledExactlyOnceWith('batch-pending');
  expect(screen.getByText('Staged: Matched: 3 · Staged: 2 · Skipped: 1')).toBeDefined();
  expect(screen.queryByRole('button', { name: 'Cancel' })).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Done' }));
  expect(onclose).toHaveBeenCalledOnce();
});

it('aborts a pending stage after the review unmounts', async () => {
  let staging: Promise<Response> | undefined;
  const onStaged = vi.fn();
  const client = createAPIClient(async input => {
    const request = input as Request;
    if (request.url.endsWith('/preflight')) return Response.json(preflight());
    staging = new Promise<Response>((_resolve, reject) => {
      request.signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true });
    });
    return staging;
  });
  const view = render(DeletionReview, { client, selection, onclose: vi.fn(), onStaged });
  await fireEvent.click(await screen.findByRole('button', { name: 'Stage 2 messages' }));
  await waitFor(() => expect(staging).toBeDefined());
  const aborted = expect(staging).rejects.toMatchObject({ name: 'AbortError' });
  view.unmount();
  await aborted;
  expect(onStaged).not.toHaveBeenCalled();
});
