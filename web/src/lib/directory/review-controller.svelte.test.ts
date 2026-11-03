import { candidate, requestOf } from './entity-fixtures.test-support';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import { DirectoryReviewController, IDENTITY_REVIEW_PAGE_LIMIT } from './review-controller.svelte';
import { mergeResult, person } from './entity-fixtures.test-support';

function page(candidates: ReturnType<typeof candidate>[], offset = 0): Response {
  return Response.json({ candidates, limit: IDENTITY_REVIEW_PAGE_LIMIT, offset });
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((next) => { resolve = next; });
  return { promise, resolve };
}

async function settle(controller: DirectoryReviewController, id: number, decision: 'accept' | 'reject', notes?: string) {
  if (notes !== undefined) controller.setDecisionDraft(id, notes);
  await controller.queueDecision(controller.rows.find(row => row.id === id)!, decision);
  return controller.settleDecision();
}

describe('DirectoryReviewController', () => {
  it('uses the generated list contract, aborts a superseded page, and discards its stale response', async () => {
    const first = deferredResponse();
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (requests.length === 1) return first.promise;
      return page([candidate(2, 'conflict')]);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));

    const candidateLoad = controller.loadIdentityPage(0, 'candidate');
    await vi.waitFor(() => expect(requests).toHaveLength(1));
    const conflictLoad = controller.loadIdentityPage(0, 'conflict');
    await conflictLoad;

    expect(requests[0]!.signal.aborted).toBe(true);
    const query = new URL(requests[1]!.url).searchParams;
    expect(query.get('state')).toBe('candidate,conflict');
    expect(query.get('limit')).toBe('11');
    expect(query.get('offset')).toBe('0');
    expect(controller.rows).toEqual([candidate(2, 'conflict')]);

    first.resolve(page([candidate(1)]));
    await candidateLoad;
    expect(controller.rows).toEqual([candidate(2, 'conflict')]);
  });

  it.each(['next', 'previous'] as const)('retains the prior page after a %s-page failure', async direction => {
    const firstPage = Array.from({ length: IDENTITY_REVIEW_PAGE_LIMIT + 1 }, (_, index) => candidate(index + 1));
    const secondPage = [candidate(101)];
    const failedOffset = direction === 'next' ? '10' : '0';
    let failedAttempts = 0;
    const offsets: string[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const url = new URL(requestOf(input).url);
      if (url.pathname.includes('/participants/')) return Response.json({ id: Number(url.pathname.split('/').at(-1)), display_label: 'Synthetic person', identifiers: [], activity_count: 1 });
      const offset = url.searchParams.get('offset') ?? '';
      offsets.push(offset);
      if (offset === failedOffset && offsets.length > 1) {
        failedAttempts += 1;
        if (failedAttempts === 1) return Response.json({ error: 'unavailable', message: 'Queue unavailable' }, { status: 503 });
      }
      return offset === '0' ? page(firstPage) : page(secondPage, 10);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));

    await controller.loadIdentityPage();
    expect(controller.hasNextPage).toBe(true);
    if (direction === 'previous') await controller.loadNextPage();
    await (direction === 'next' ? controller.loadNextPage() : controller.loadPreviousPage());
    expect(controller.rows).toEqual(direction === 'next' ? firstPage.slice(0, 10) : secondPage);
    expect(controller.offset).toBe(direction === 'next' ? 0 : 10);
    expect(controller.pageError).toBe('Queue unavailable');
    expect(controller.error).toBeNull();
    if (direction === 'previous') expect(offsets).toEqual(['0', '10', '0']);

    await controller.retryPage();
    expect(offsets).toEqual(direction === 'next' ? ['0', '10', '10'] : ['0', '10', '0', '0']);
    expect(controller.rows).toEqual(direction === 'next' ? secondPage : firstPage.slice(0, 10));
    expect(controller.offset).toBe(direction === 'next' ? 10 : 0);
    expect(controller.hasPreviousPage).toBe(direction === 'next');
    expect(controller.hasNextPage).toBe(direction === 'previous');
  });

  it('keeps the first unseen candidate when a queued decision removes a row before a forward retry', async () => {
    let candidates = Array.from({ length: 21 }, (_, index) => candidate(index + 1));
    const offsets: number[] = [];
    let failNextPage = true;
    const controller = new DirectoryReviewController(createAPIClient(async input => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (url.pathname.includes('/participants/')) return Response.json({
        id: Number(url.pathname.split('/').at(-1)), display_label: 'Synthetic person', identifiers: [], activity_count: 1
      });
      if (request.method === 'POST') {
        expect(url.pathname).toBe('/api/v1/identity/match-candidates/1/review/reject');
        candidates = candidates.filter(row => row.id !== 1);
        return Response.json({ candidate: candidate(1, 'rejected'), cache_state: 'ready' });
      }
      const offset = Number(url.searchParams.get('offset'));
      offsets.push(offset);
      if (offset === 10 && failNextPage) {
        failNextPage = false;
        return Response.json({ message: 'Queue unavailable' }, { status: 503 });
      }
      const limit = Number(url.searchParams.get('limit'));
      return page(candidates.slice(offset, offset + limit), offset);
    }));
    try {
      await controller.loadIdentityPage();
      await controller.loadNextPage();
      expect(controller.pageError).toBe('Queue unavailable');
      await controller.queueDecision(controller.rows[0]!, 'reject');

      await controller.retryPage();

      expect(offsets).toEqual([0, 10, 0, 9]);
      expect(controller.rows.map(row => row.id)).toEqual([11, 12, 13, 14, 15, 16, 17, 18, 19, 20]);
      expect(controller.offset).toBe(9);
      expect(controller.pageError).toBeNull();
      expect(controller.hasNextPage).toBe(true);
      await controller.loadNextPage();
      expect(controller.rows.map(row => row.id)).toEqual([21]);
    } finally { controller.destroy(); }
  });

  it.each(['restoration', 'History'] as const)('resets the offset synchronously before a failed %s load', async entry => {
    const restored = deferredResponse();
    let restoredAttempts = 0;
    const offsets: string[] = [];
    const firstPage = Array.from({ length: IDENTITY_REVIEW_PAGE_LIMIT + 1 }, (_, index) => candidate(index + 1));
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const offset = new URL(request.url).searchParams.get('offset') ?? '';
      offsets.push(offset);
      if (offset === '10') return page([candidate(101)], 10);
      if (offsets.length === 1) return page(firstPage);
      restoredAttempts += 1;
      if (restoredAttempts === 1) return restored.promise;
      return page([candidate(1, entry === 'History' ? 'accepted' : 'candidate')]);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();
    await controller.loadNextPage();
    expect(controller.offset).toBe(10);
    expect(controller.rows).toEqual([candidate(101)]);

    if (entry === 'restoration') controller.applyURLState({ identityState: 'candidate' }, true);
    else void controller.setIdentityState('accepted');

    expect(controller.identityState).toBe(entry === 'restoration' ? 'candidate' : 'accepted');
    expect(new URL(requestOf(fetchFn.mock.lastCall![0]).url).searchParams.get('state')).toBe(entry === 'restoration' ? 'candidate,conflict' : 'accepted,rejected');
    expect(controller.offset).toBe(0);
    expect(controller.rows).toEqual([]);
    expect(controller.loading).toBe(true);
    restored.resolve(Response.json({ error: 'unavailable', message: 'Restoration failed' }, { status: 503 }));
    await vi.waitFor(() => expect(controller.error).toBe('Restoration failed'));
    expect(controller.pageError).toBeNull();
    expect(controller.offset).toBe(0);
    expect(controller.rows).toEqual([]);

    await controller.retryPage();
    expect(offsets).toEqual(['0', '10', '0', '0']);
    expect(controller.rows).toEqual([candidate(1, entry === 'History' ? 'accepted' : 'candidate')]);
  });

  it('keeps the reject note while the decision is pending without retrying it', async () => {
    const pending = deferredResponse();
    const requests: Request[] = [];
    const failure = () => Response.json({ error: 'unavailable', message: 'Decision unavailable' }, { status: 503 });
    const controller = new DirectoryReviewController(createAPIClient(async input => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'GET') return page([candidate(17)]);
      return pending.promise;
    }));
    try {
      await controller.loadIdentityPage();
      const decision = settle(controller, 17, 'reject', 'Original note');
      await vi.waitFor(() => expect(controller.isDecisionPending(17)).toBe(true));
      controller.setDecisionDraft(17, 'Overwrite attempt');
      expect(controller.getDecisionDraft(17)).toBe('Original note');
      pending.resolve(failure());
      await expect(decision).resolves.toEqual({ ok: false, kind: 'error', status: 503, message: 'Decision unavailable' });
      expect(controller.decisionError).toBe('Decision unavailable');
      expect(controller.getDecisionDraft(17)).toBe('Original note');
      expect(controller.rows).toEqual([candidate(17)]);
      expect(requests.filter(request => request.method === 'POST')).toHaveLength(1);
    } finally { pending.resolve(failure()); controller.destroy(); }
  });

  it('preserves a malformed merge-required conflict message without showing a merge offer', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      if (requestOf(input).method === 'GET') return page([candidate(17)]);
      return Response.json({
      error: 'person_merge_required',
      message: 'Malformed conflict',
      profiles: [
        { etag: 'W/"person-7-r4"', person: { id: 7, revision: 4 } },
        { etag: '"person-7-r4"', person: { id: 7, revision: 4 } }
      ]
      }, { status: 409 });
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
	await controller.loadIdentityPage();

    await expect(settle(controller, 17, 'accept')).resolves.toEqual({
      ok: false, kind: 'error', status: 409,
      message: 'Malformed conflict'
    });
    expect(controller.mergeRequired).toBeNull();
  });

  it.each(['survivor choice', 'merge refresh'])('leaves the second pair untouched during %s', async phase => {
    const reload = deferredResponse();
    const requests: Request[] = [];
    const controller = new DirectoryReviewController(createAPIClient(async input => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'POST') return Response.json({ error: 'person_merge_required', message: 'Choose a survivor', profiles: [7, 9].map(id => ({ person: person(id), etag: id === 7 ? '"person-7-r4"' : '"person-9-r2"' })) }, { status: 409 });
      return reload.promise;
    }));
    controller.rows = [candidate(17), candidate(18)];
    const success = { result: mergeResult(person(7), 'stale'), survivor: person(7), responseETag: '"person-7-r5"' };
    try {
      controller.setDecisionDraft(17, 'Review both profiles');
      await controller.queueDecision(controller.rows[0]!, 'accept');
      let completion: Promise<void> | undefined;
      if (phase === 'merge refresh') {
        await controller.settleDecision();
        completion = controller.completePersonMerge(17, success);
        expect(controller.submitting).toBe(true);
      }
      await controller.queueDecision(controller.rows[1]!, 'accept');
      expect(controller.pending).toBeUndefined();
      expect(requests.filter(request => request.method === 'POST')).toHaveLength(1);
      expect(controller.hidden.has(18)).toBe(false);
      expect(controller.isDecisionPending(18)).toBe(false);
      if (phase === 'survivor choice') expect(controller.mergeRequired?.candidateID).toBe(17);
      else {
        reload.resolve(Response.json({ message: 'Queue unavailable' }, { status: 503 }));
        await completion;
        expect(controller.submitting).toBe(false);
        expect(controller.lastMerge).toEqual({ candidateID: 17, ...success });
        expect(controller.rows.map(row => row.id)).toEqual([17, 18]);
        expect(controller.pageError).toBe('Queue unavailable');
        expect(controller.status).toContain('Review the refreshed pair to finish.');
      }
    } finally { reload.resolve(page([])); controller.destroy(); }
  });

  it.each(['newer To review', '409 recovery', '503 failure remains available after History navigation'] as const)('preserves the active review context after deferred settlement: %s', async scenario => {
    const decisionResponse = deferredResponse();
    const conflict = {
      error: 'person_merge_required', message: 'Choose a survivor', profiles: [
        { etag: '"person-7-r4"', person: person(7) },
        { etag: '"person-9-r2"', person: person(9) }
      ]
    };
    const requests: Request[] = [];
    let reads = 0;
    const historyRows = [candidate(88, 'accepted')];
    const controller = new DirectoryReviewController(createAPIClient(async input => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'POST') return decisionResponse.promise;
      reads += 1;
      const params = new URL(request.url).searchParams;
      return params.get('state') === 'accepted,rejected' ? page(historyRows, Number(params.get('offset'))) : page([candidate(reads === 1 ? 17 : reads === 2 ? 99 : 100)]);
    }));
    try {
      await controller.loadIdentityPage();
      controller.setDecisionDraft(17, 'Review both profiles');
      await controller.queueDecision(controller.rows[0]!, 'accept');
      const navigation = scenario === 'newer To review' ? controller.setIdentityState('accepted') : undefined;
      const decision = controller.settleDecision();
      await vi.waitFor(() => expect(controller.isDecisionPending(17)).toBe(true));
      if (scenario === 'newer To review') {
        expect(controller.identityState).toBe('candidate');
        controller.applyURLState({ identityState: 'candidate' }, true);
        await vi.waitFor(() => expect(controller.rows).toEqual([candidate(99)]));
        decisionResponse.resolve(Response.json({ candidate: candidate(17, 'accepted'), identity_revision: 4, cache_state: 'ready' }));
        await expect(decision).resolves.toEqual({ ok: true, candidate: candidate(17, 'accepted'), cacheState: 'ready' });
        await navigation;
        expect(controller.identityState).toBe('candidate');
        expect(controller.rows).toEqual([candidate(100)]);
        expect(controller.offset).toBe(0);
        expect(controller.pageError).toBeNull();
        expect(controller.status).toBeNull();
        expect(reads).toBe(3);
      } else {
        await controller.setIdentityState('accepted');
        await vi.waitFor(() => expect(controller.rows).toEqual(historyRows));
        if (scenario === '409 recovery') {
          decisionResponse.resolve(Response.json(conflict, { status: 409 }));
          await expect(decision).resolves.toMatchObject({ ok: false, kind: 'merge_required', conflict });
        }
        if (scenario === '503 failure remains available after History navigation') {
          decisionResponse.resolve(Response.json({ error: 'unavailable', message: 'Decision unavailable' }, { status: 503 }));
          await expect(decision).resolves.toMatchObject({ ok: false, kind: 'error', status: 503, message: 'Decision unavailable' });
          expect(controller.decisionError).toBe('Decision unavailable');
          expect(controller.mergeRequired).toBeNull();
        } else {
          expect(controller.mergeRequired?.candidateID).toBe(17);
          expect(controller.decisionError).toBeNull();
        }
        expect(controller.identityState).toBe('accepted');
        expect(controller.offset).toBe(0);
        expect(controller.rows).toEqual(historyRows);
      }
      expect(controller.isDecisionPending(17)).toBe(false);
      expect(requests.filter(request => request.method === 'POST')).toHaveLength(1);
    } finally { controller.destroy(); }
  });

  it('aborts the pending decision and clears its state when destroyed', async () => {
    const decisionRequest = deferredResponse();
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'GET') return page([candidate(17)]);
      return decisionRequest.promise;
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));

    await controller.loadIdentityPage();
    controller.setDecisionDraft(17, 'Ephemeral note');
    void settle(controller, 17, 'reject');
    await vi.waitFor(() => expect(requests).toHaveLength(2));
    controller.destroy();

    expect(requests[1]!.signal.aborted).toBe(true);
    expect(controller.pendingDecisions.size).toBe(0);
    expect(controller.getDecisionDraft(17)).toBe('');
    expect(controller.loading).toBe(false);
  });
});

it.each(['restoration', 'stale token'] as const)('refreshes evidence through %s', async reconciliation => {
  const writes: Request[] = [];
  let decided = false;
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const request = requestOf(input);
    if (request.method === 'POST') {
      writes.push(request);
      decided = true;
      return Response.json({ error: 'identity_match_review_stale', message: 'Changed' }, { status: 409 });
    }
    const path = new URL(request.url).pathname;
    if (path.includes('/participants/')) return Response.json({ id: Number(path.split('/').at(-1)), display_label: decided ? 'Updated Example' : 'Original Example', activity_count: decided ? 9 : 3, identifiers: [{ participant_id: Number(path.split('/').at(-1)), type: 'email', is_primary: true, value: decided ? 'updated@example.com' : 'original@example.com' }] });
    return page([{ ...candidate(1), review_token: decided ? 'fresh-token' : 'old-token' }]);
  }));
  try {
    await controller.loadIdentityPage();
    expect(controller.rows[0]?.review_token).toBe('old-token');
    controller.requestSummaries();
    await vi.waitFor(() => expect(controller.summaries.get('participant:10')).toMatchObject({ name: 'Original Example', address: 'original@example.com', count: 3 }));
    if (reconciliation === 'restoration') {
      decided = true;
      controller.applyURLState({ identityState: 'candidate' }, true);
    } else {
      await expect(settle(controller, 1, 'accept')).resolves.toMatchObject({ ok: false, kind: 'stale', status: 409 });
      expect(writes).toHaveLength(1);
      expect(controller.decisionError).toContain('Review the refreshed evidence');
      expect(controller.pageError).toBeNull();
    }
    await vi.waitFor(() => expect(controller.rows[0]?.review_token).toBe('fresh-token'));
    controller.requestSummaries();
    await vi.waitFor(() => expect(controller.summaries.get('participant:10')).toMatchObject({ name: 'Updated Example', address: 'updated@example.com', count: 9 }));
  } finally { controller.destroy(); }
});

it.each([
  { decision: 'accept', reconciliation: 'ready' },
  { decision: 'reject', reconciliation: 'ready' },
  { decision: 'accept', reconciliation: 'pending' },
  { decision: 'accept', reconciliation: 'failed' }
] as const)('settles $decision through $reconciliation', async ({ decision, reconciliation }) => {
  const writes: Array<{ path: string; body: unknown; keepalive: boolean }> = [];
  const settled = vi.fn();
  const refresh = deferredResponse();
  const accepted = candidate(1, decision === 'accept' ? 'accepted' : 'rejected');
  const cacheState = 'ready';
  let decided = false;
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const request = requestOf(input);
    if (request.method === 'POST') {
      writes.push({ path: new URL(request.url).pathname, body: await request.json(), keepalive: request.keepalive });
      decided = true;
      return Response.json({ candidate: accepted, cache_state: cacheState });
    }
    const path = new URL(request.url).pathname;
    if (path.includes('/participants/')) return Response.json({ id: Number(path.split('/').at(-1)), display_label: decided ? 'Updated Example' : 'Original Example', activity_count: decided ? 9 : 3, identifiers: [{ participant_id: Number(path.split('/').at(-1)), type: 'email', is_primary: true, value: decided ? 'updated@example.com' : 'original@example.com' }] });
    if (decided && reconciliation === 'pending') return refresh.promise;
    if (decided && reconciliation === 'failed') return Response.json({ error: 'unavailable', message: 'Reload failed' }, { status: 503 });
    return page(decided ? [candidate(2)] : reconciliation === 'ready' ? [candidate(1), candidate(2)] : [candidate(1)]);
  }), undefined, settled);
  try {
    await controller.loadIdentityPage();
    const saved = { id: 7, name: 'Saved', address: 'Saved person', lastAt: null };
    controller.summaries.set('participant:999', saved);
    const completion = settle(controller, 1, decision, '  Different people  ');
    if (reconciliation === 'pending') {
      await vi.waitFor(() => expect(controller.rows).toEqual([accepted]));
      expect(controller.isDecisionPending(1)).toBe(true);
      refresh.resolve(page([candidate(2)]));
    }
    await expect(completion).resolves.toEqual({ ok: true, candidate: accepted, cacheState });
    await vi.waitFor(() => expect(settled).toHaveBeenCalledOnce());
    expect(controller.isDecisionPending(1)).toBe(false);
    expect(writes).toEqual([{ path: `/api/v1/identity/match-candidates/1/review/${decision}`, body: { review_token: 'token-1-candidate', notes: 'Different people' }, keepalive: true }]);
    expect(controller.rows).toEqual(reconciliation === 'failed' ? [accepted] : [candidate(2)]);
    expect(controller.getDecisionDraft(1)).toBe('');
    expect(controller.summaries.has('participant:999')).toBe(decision === 'reject');
    if (reconciliation === 'pending') expect(controller.status).toContain('accepted');
    if (reconciliation === 'failed') {
      expect(controller.pageError).toBe('Reload failed');
      expect(controller.decisionError).toBeNull();
    }
  } finally { controller.destroy(); }
});

it('returns to the first page when deciding the last later-page row empties that offset', async () => {
  let decided = false;
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const request = requestOf(input);
    if (request.method === 'POST') { decided = true; return Response.json({ candidate: candidate(11, 'rejected'), cache_state: 'ready' }); }
    const offset = Number(new URL(request.url).searchParams.get('offset'));
    return page(offset ? decided ? [] : [candidate(11)] : Array.from({ length: decided ? 10 : 11 }, (_, i) => candidate(i + 1)), offset);
  }));
  try {
    await controller.loadIdentityPage();
    await controller.loadNextPage();
    await settle(controller, 11, 'reject');
    expect(controller.offset).toBe(0);
    expect(controller.rows).toHaveLength(10);
    expect(controller.hasNextPage).toBe(false);
  } finally { controller.destroy(); }
});

it.each([
  { direction: 'next', failed: false },
  { direction: 'retry', failed: false },
  { direction: 'invalidate', failed: false },
  { direction: 'next', failed: true },
] as const)('settles its queued decision before $direction navigation, failed=$failed', async ({ direction, failed }) => {
  const decision = deferredResponse(), destination = deferredResponse();
  const target = direction === 'invalidate' ? 0 : failed ? 10 : 9;
  const order: string[] = [];
  let reads = 0;
  let moving = false, decided = false, posted = false, destinationRead = false;
  const rows = Array.from({ length: 11 }, (_, index) => candidate(index + 1));
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const request = requestOf(input);
    order.push(request.method);
    if (request.method === 'POST') { posted = true; const response = await decision.promise; decided = !failed; order.push('POST resolved'); return response; }
    if (direction === 'invalidate') return page([candidate(++reads)]);
    const offset = Number(new URL(request.url).searchParams.get('offset'));
    if (!moving && offset === 10 && direction === 'retry') return Response.json({ message: 'Queue unavailable' }, { status: 503 });
    if (moving && offset === target) { destinationRead = true; return destination.promise; }
    return page(decided ? rows.slice(1) : rows, offset);
  }));
  try {
    await controller.loadIdentityPage(0);
    if (direction === 'retry') await controller.loadNextPage();
    await controller.queueDecision(controller.rows[0]!, 'reject');
    moving = true;
    const navigation = direction === 'next' ? controller.loadNextPage() : direction === 'invalidate' ? controller.invalidate(true) : controller.retryPage();
    await vi.waitFor(() => expect(posted).toBe(true));
    if (direction === 'invalidate') expect(order).toEqual(['GET', 'POST']);
    decision.resolve(failed ? Response.json({ message: 'Decision unavailable' }, { status: 503 }) : Response.json({ candidate: candidate(1, 'rejected'), cache_state: 'ready' }));
    await vi.waitFor(() => expect(controller.pendingDecisions.size).toBe(0));
    if (direction !== 'invalidate') {
      await vi.waitFor(() => expect(destinationRead).toBe(true));
      destination.resolve(page([candidate(failed ? 11 : 101)], target));
    }
    await navigation;
    expect(controller.offset).toBe(target);
    expect(controller.rows.map(row => row.id)).toEqual([direction === 'invalidate' ? 3 : failed ? 11 : 101]);
    if (direction === 'invalidate') {
      expect(order).toEqual(['GET', 'POST', 'POST resolved', 'GET', 'GET']);
      expect(controller.hidden.has(1)).toBe(false);
    }
    if (failed) {
      expect(controller.hidden.size).toBe(0);
      expect(controller.decisionError).toBe('Decision unavailable');
    }
    if (direction === 'next' && !failed) {
      await controller.loadPreviousPage();
      expect(controller.offset).toBe(0);
      expect(controller.rows.map(row => row.id)).toEqual([2, 3, 4, 5, 6, 7, 8, 9, 10, 11]);
    }
  } finally { controller.destroy(); }
});
