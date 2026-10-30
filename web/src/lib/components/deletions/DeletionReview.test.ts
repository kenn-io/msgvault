import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

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

interface Seen { method: string; path: string; body?: Record<string, unknown> }

function api(review = preflight(), seen: Seen[] = []) {
  return createAPIClient(vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    const body = request.method === 'POST' ? await request.clone().json() : undefined;
    seen.push({ method: request.method, path, body });
    if (path.endsWith('/explore/preflight')) return Response.json(review);
    if (path === '/api/v1/deletions' && request.method === 'POST') {
      return body?.dry_run
        ? Response.json({ dry_run: true, message_count: 2, matched_count: 3 })
        : Response.json({ dry_run: false, id: 'batch-9', message_count: 2, matched_count: 3 }, { status: 201 });
    }
    if (request.method === 'DELETE') return Response.json({ id: 'batch-9', status: 'cancelled' });
    return Response.json({}, { status: 404 });
  }));
}

afterEach(() => {
  vi.useRealTimers();
  document.body.replaceChildren();
});

describe('DeletionReview', () => {
  it('keeps a staged summary until Done after daemon acknowledgement', async () => {
    const seen: Seen[] = [];
    const onStaged = vi.fn(), onclose = vi.fn();
    render(DeletionReview, { client: api(preflight(), seen), selection, onclose, onStaged });
    await fireEvent.click(await screen.findByRole('button', { name: 'Stage 2 messages' }));
    await waitFor(() => expect(onStaged).toHaveBeenCalledWith('batch-9'));
    expect(onclose).not.toHaveBeenCalled();
    expect(screen.getByText('Staged: Matched: 3 · Staged: 2 · Skipped: 1')).toBeDefined();
    expect(screen.getByText('batch-9')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Done' }));
    expect(onclose).toHaveBeenCalledOnce();
    expect(seen.find(entry => entry.path === '/api/v1/deletions')?.body).toMatchObject({ selection, operation_token: 'token-1', dry_run: false });
  });

  it('disables staging and says why when nothing qualifies', async () => {
    const review = preflight({
      deletable_count: 0,
      unavailable_actions: [{ action: 'stage_deletion', reason: 'no_deletable_messages' }]
    });
    render(DeletionReview, { client: api(review), selection, onclose: vi.fn() });
    const stage = await screen.findByRole('button', { name: 'Stage 0 messages' });
    expect((stage as HTMLButtonElement).disabled).toBe(true);
  });
});

it('keeps all-matching exclusions and its token, clears failed previews, and retains the staged result', async () => {
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
  expect(screen.getByText('After 1 exclusion.')).toBeDefined();
  expect(screen.getByText('Semantic search covers active messages only.')).toBeDefined();
  expect(document.querySelector('time')?.dateTime).toBe('2026-07-19T10:05:00Z');
  await fireEvent.click(screen.getByRole('button', { name: 'Dry run' }));
  await screen.findByText('Dry run: Matched: 3 · Staged: 2 · Skipped: 1 in outlook@example.com');
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

it('ignores a staging response after the review closes', async () => {
  let finish!: (response: Response) => void;
  const onStaged = vi.fn();
  const client = createAPIClient(async input => (input as Request).url.endsWith('/preflight') ? Response.json(preflight()) : new Promise(resolve => { finish = resolve; }));
  const view = render(DeletionReview, { client, selection, onclose: vi.fn(), onStaged });
  await fireEvent.click(await screen.findByRole('button', { name: 'Stage 2 messages' }));
  view.unmount();
  finish(Response.json({ id: 'late', message_count: 2, dry_run: false }, { status: 201 }));
  await new Promise(resolve => setTimeout(resolve, 0));
  expect(onStaged).not.toHaveBeenCalled();
});

it('holds the review open through Escape while staging and handles the result once', async () => {
  let finish!: (response: Response) => void;
  const onclose = vi.fn(), onStaged = vi.fn();
  const client = createAPIClient(async input => (input as Request).url.endsWith('/preflight')
    ? Response.json(preflight())
    : new Promise<Response>(resolve => { finish = resolve; }));
  render(DeletionReview, { client, selection, onclose, onStaged });
  await fireEvent.click(await screen.findByRole('button', { name: 'Stage 2 messages' }));
  await waitFor(() => expect(finish).toBeTypeOf('function'));
  await fireEvent.keyDown(document, { key: 'Escape' });
  expect(onclose).not.toHaveBeenCalled();
  finish(Response.json({ id: 'batch-pending', message_count: 2, dry_run: false }, { status: 201 }));
  await screen.findByText('batch-pending');
  expect(onStaged).toHaveBeenCalledExactlyOnceWith('batch-pending');
  await fireEvent.keyDown(document, { key: 'Escape' });
  expect(onclose).toHaveBeenCalledOnce();
});
