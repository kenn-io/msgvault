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
  it('shows counts and a Stage button with no confirmation modal on top', async () => {
    render(DeletionReview, { client: api(), selection, onclose: vi.fn() });
    expect(await screen.findByText('3 messages · 3 KB')).toBeDefined();
    expect(screen.getByText("1 can't be deleted from Gmail and stay.")).toBeDefined();
    expect(screen.getAllByRole('dialog')).toHaveLength(1);
    expect(screen.getByRole('button', { name: 'Stage 2 messages' })).toBeDefined();
    expect(screen.queryByText(/manifest|execute|Microsoft Graph/i)).toBeNull();
  });

  it('stages the reviewed selection and closes after daemon acknowledgement', async () => {
    const seen: Seen[] = [];
    const onStaged = vi.fn(), onclose = vi.fn();
    render(DeletionReview, { client: api(preflight(), seen), selection, onclose, onStaged });
    await fireEvent.click(await screen.findByRole('button', { name: 'Stage 2 messages' }));
    await waitFor(() => expect(onStaged).toHaveBeenCalledWith('batch-9'));
    expect(onclose).toHaveBeenCalledOnce();
    expect(seen.find(entry => entry.path === '/api/v1/deletions')?.body).toMatchObject({ selection, operation_token: 'token-1', dry_run: false });
  });

  it('runs a dry run from the menu without staging anything', async () => {
    const seen: Seen[] = [];
    render(DeletionReview, { client: api(preflight(), seen), selection, onclose: vi.fn() });
    await screen.findByText('3 messages · 3 KB');
    await fireEvent.click(screen.getByRole('button', { name: 'Dry run' }));
    expect(await screen.findByText('Dry run: 2 messages would be staged.')).toBeDefined();
    expect(seen.filter((entry) => entry.method === 'POST' && entry.body?.dry_run === false)).toEqual([]);
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
