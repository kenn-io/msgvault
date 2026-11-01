import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import { DirectoryReviewController, IDENTITY_REVIEW_PAGE_LIMIT } from './review-controller.svelte';
import { mergeResult, person } from './entity-fixtures.test-support';

function requestOf(input: RequestInfo | URL): Request {
  return input instanceof Request ? input : new Request(input);
}

function candidate(id: number, state = 'candidate') {
  return {
    id,
    left_id: id * 10,
    left_kind: 'participant',
    right_id: id * 10 + 1,
    right_kind: 'participant',
    basis: 'stable_provider_id',
    source: 'synthetic',
    state,
    review_token: `token-${id}-${state}`,
    actionable: state === 'candidate',
    application_pending: false,
    evidence: [],
    created_at: '2026-08-01T00:00:00Z',
    updated_at: '2026-08-01T00:00:00Z'
  };
}

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
      return page([candidate(2, requests.length === 2 ? 'conflict' : 'accepted')]);
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

  it('retains the prior page after a later-page failure and retries the same offset', async () => {
    const firstPage = Array.from({ length: IDENTITY_REVIEW_PAGE_LIMIT + 1 }, (_, index) => candidate(index + 1));
    let secondAttempt = 0;
    const offsets: string[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const offset = new URL(request.url).searchParams.get('offset') ?? '';
      offsets.push(offset);
      if (offset === '0') return page(firstPage);
      secondAttempt += 1;
      if (secondAttempt === 1) return Response.json({ error: 'unavailable', message: 'Queue unavailable' }, { status: 503 });
      return page(Array.from({ length: 10 }, (_, index) => candidate(101 + index)), 10);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));

    await controller.loadIdentityPage();
    expect(controller.hasNextPage).toBe(true);
    await controller.loadNextPage();
    expect(controller.rows).toEqual(firstPage.slice(0, 10));
    expect(controller.offset).toBe(0);
    expect(controller.pageError).toBe('Queue unavailable');

    await controller.retryPage();
    expect(offsets).toEqual(['0', '10', '10']);
    expect(controller.rows).toHaveLength(10);
    expect(controller.offset).toBe(10);
    expect(controller.hasPreviousPage).toBe(true);
    expect(controller.hasNextPage).toBe(false);
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

  it('refreshes a restored copy of the same URL context after a late success', async () => {
    const decisionResponse = deferredResponse();
    const requests: Request[] = [];
    let reads = 0;
    const accepted = candidate(17, 'accepted');
    const restoredRows = [candidate(99)];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'POST') return decisionResponse.promise;
      reads += 1;
      if (reads === 1) return page([candidate(17)]);
      if (reads === 2) return page(restoredRows);
      return page([candidate(100)]);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();

    const decision = settle(controller, 17, 'accept', 'Confirmed');
    await vi.waitFor(() => expect(controller.isDecisionPending(17)).toBe(true));
    controller.applyURLState({ identityState: 'candidate' }, true);
    await vi.waitFor(() => expect(controller.rows).toEqual(restoredRows));
    decisionResponse.resolve(Response.json({ candidate: accepted, identity_revision: 4, cache_state: 'ready' }));

    await expect(decision).resolves.toEqual({ ok: true, candidate: accepted, cacheState: 'ready' });
    expect(controller.identityState).toBe('candidate');
    expect(controller.rows).toEqual([candidate(100)]);
    expect(controller.offset).toBe(0);
    expect(controller.pageError).toBeNull();
    expect(controller.status).toBeNull();
    expect(requests.filter((request) => request.method === 'GET')).toHaveLength(3);
  });

  it('retains the current same-state page when previous-page loading fails', async () => {
    const firstPage = Array.from({ length: IDENTITY_REVIEW_PAGE_LIMIT + 1 }, (_, index) => candidate(index + 1));
    let firstPageLoads = 0;
    const offsets: string[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const offset = new URL(requestOf(input).url).searchParams.get('offset') ?? '';
      offsets.push(offset);
      if (offset === '10') return page([candidate(101)], 10);
      firstPageLoads += 1;
      if (firstPageLoads === 1) return page(firstPage);
      return Response.json({ error: 'unavailable', message: 'Previous page failed' }, { status: 503 });
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();
    await controller.loadNextPage();

    await controller.loadPreviousPage();

    expect(offsets).toEqual(['0', '10', '0']);
    expect(controller.offset).toBe(10);
    expect(controller.rows).toEqual([candidate(101)]);
    expect(controller.pageError).toBe('Previous page failed');
    expect(controller.error).toBeNull();
  });

  it('applies a successful decision before best-effort reconciliation', async () => {
    const reconcile = deferredResponse();
    const requests: Request[] = [];
    const accepted = candidate(17, 'accepted');
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'POST') {
        return Response.json({ candidate: accepted, identity_revision: 4, cache_state: 'ready' });
      }
      if (requests.length === 1) return page([candidate(17)]);
      return reconcile.promise;
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();

    const decision = settle(controller, 17, 'accept');
    await vi.waitFor(() => expect(controller.rows).toEqual([accepted]));
    expect(controller.isDecisionPending(17)).toBe(true);

    reconcile.resolve(page([accepted]));
    await expect(decision).resolves.toEqual({ ok: true, candidate: accepted, cacheState: 'ready' });
    expect(controller.isDecisionPending(17)).toBe(false);
    expect(controller.status).toContain('accepted');
  });

  it.each([503, 409])('keeps a failed reject note and never automatically retries the decision: %s', async status => {
    const message = status === 503 ? 'Decision unavailable' : 'An accepted identity match cannot be rejected because its participant link is retained';
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'GET') return page([candidate(17)]);
      return Response.json({ error: status === 503 ? 'unavailable' : 'identity_match_already_accepted', message }, { status });
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();

    await expect(settle(controller, 17, 'reject', 'Different people')).resolves.toEqual({
      ok: false,
      kind: 'error',
      status,
      message
    });

    expect(controller.decisionError).toBe(message);
    expect(controller.getDecisionDraft(17)).toBe('Different people');
    expect(controller.rows).toEqual([candidate(17)]);
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
  });

  it('refreshes the queue after a stale review token without retrying the decision', async () => {
    const requests: Request[] = [];
    let reads = 0;
    let changed = false;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'POST') {
        changed = true;
        return Response.json({ error: 'identity_match_review_stale', message: 'Changed' }, { status: 409 });
      }
      const url = new URL(request.url);
      if (url.pathname.includes('/participants/')) {
        const id = Number(url.pathname.split('/').at(-1));
        return Response.json({ id, display_label: changed ? 'Updated Example' : 'Original Example', activity_count: changed ? 9 : 3, identifiers: [{ participant_id: id, type: 'email', is_primary: true, value: changed ? 'updated@example.com' : 'absorbed@example.com' }] });
      }
      if (url.pathname === '/api/v1/people/1') return Response.json(person(1));
      reads += 1;
      const row = { ...candidate(17), right_kind: 'person', right_id: 1 };
      if (reads > 1) row.review_token = 'fresh-token';
      return page([row]);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();
    controller.requestSummaries();
    await vi.waitFor(() => expect(controller.summaries.get('participant:170')).toMatchObject({ name: 'Original Example', address: 'absorbed@example.com', count: 3 }));

    await vi.waitFor(() => expect(controller.summaries.get('person:1')).toMatchObject({ address: 'Saved person' }));
    expect(controller.summaries.get('person:1')?.count).toBeUndefined();
    await expect(settle(controller, 17, 'accept')).resolves.toMatchObject({
      ok: false, kind: 'stale', status: 409
    });
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
    expect(controller.rows[0]?.review_token).toBe('fresh-token');
    expect(controller.decisionError).toContain('Review the refreshed evidence');
    controller.requestSummaries();
    try {
      await vi.waitFor(() => expect(controller.summaries.get('participant:170')).toMatchObject({ name: 'Updated Example', address: 'updated@example.com', count: 9 }));
    } finally { controller.destroy(); }
  });

  it('reports a committed decision as successful when page reconciliation fails', async () => {
    let reads = 0;
    const accepted = candidate(17, 'accepted');
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (request.method === 'POST') {
        return Response.json({ candidate: accepted, identity_revision: 5, cache_state: 'stale' });
      }
      reads += 1;
      if (reads === 1) return page([candidate(17)]);
      return Response.json({ error: 'unavailable', message: 'Reload failed' }, { status: 503 });
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();

    await expect(settle(controller, 17, 'accept')).resolves.toEqual({
      ok: true,
      candidate: accepted,
      cacheState: 'stale'
    });
    expect(controller.rows).toEqual([accepted]);
    expect(controller.pageError).toBe('Reload failed');
    expect(controller.decisionError).toBeNull();
  });

  it('leaves the second pair untouched while the staged pair needs a survivor', async () => {
    const conflict = {
      error: 'person_merge_required',
      message: 'Choose a survivor',
      profiles: [
        { etag: '"person-7-r4"', person: { id: 7, revision: 4, display_name: 'Synthetic One' } },
        { etag: '"person-9-r2"', person: { id: 9, revision: 2, display_name: 'Synthetic Two' } }
      ]
    };
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'GET') return page([candidate(17), candidate(18)]);
      return Response.json(conflict, { status: 409 });
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();

    controller.setDecisionDraft(17, 'Review both profiles');
    controller.queueDecision(controller.rows[0]!, 'accept');
    await controller.queueDecision(controller.rows[1]!, 'accept');
    await vi.waitFor(() => expect(controller.mergeRequired?.candidateID).toBe(17));
    expect(controller.pending).toBeUndefined();
    expect(requests.filter(request => request.method === 'POST')).toHaveLength(1);
    expect(controller.hidden.has(18)).toBe(false);
    expect(controller.isDecisionPending(18)).toBe(false);
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

  it('records merge success, preserves decision notes, and refreshes the current page', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      requests.push(requestOf(input));
      return page([{ ...candidate(17), review_token: 'after-profile-merge' }, candidate(18)]);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    controller.rows = [candidate(17), candidate(18)];
    controller.setDecisionDraft(17, 'merge this one');
    controller.setDecisionDraft(18, 'keep this draft');
    const success = {
      result: mergeResult(person(7), 'stale'),
      survivor: person(7),
      responseETag: '"person-7-r5"'
    };

    const completion = controller.completePersonMerge(17, success);
    await controller.queueDecision(controller.rows[0] ?? candidate(17), 'accept');
    expect(controller.pending).toBeUndefined();
    await completion;

    expect(controller.lastMerge).toEqual({ candidateID: 17, ...success });
    expect(controller.rows[0]?.review_token).toBe('after-profile-merge');
    expect(controller.getDecisionDraft(17)).toBe('merge this one');
    expect(controller.getDecisionDraft(18)).toBe('keep this draft');
    expect(controller.status).toContain('Review the refreshed pair to finish.');
    expect(requests).toHaveLength(1);
  });

  it.each([409, 503])('retains %s recovery when choosing History during settlement', async status => {
    const decisionResponse = deferredResponse();
    const conflict = {
      error: 'person_merge_required',
      message: 'Choose a survivor',
      profiles: [
        { etag: '"person-7-r4"', person: { id: 7, revision: 4, display_name: 'Synthetic One' } },
        { etag: '"person-9-r2"', person: { id: 9, revision: 2, display_name: 'Synthetic Two' } }
      ]
    };
    const requests: Request[] = [];
    const historyRows = [candidate(88, 'accepted')];
    const controller = new DirectoryReviewController(createAPIClient(async input => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'POST') return decisionResponse.promise;
      const params = new URL(request.url).searchParams;
      return params.get('state') === 'accepted,rejected'
        ? page(historyRows, Number(params.get('offset')))
        : page([candidate(17)]);
    }));
    await controller.loadIdentityPage();
    controller.setDecisionDraft(17, 'Review both profiles');
    await controller.queueDecision(controller.rows[0]!, 'accept');
    const decision = controller.settleDecision();
    await vi.waitFor(() => expect(controller.isDecisionPending(17)).toBe(true));
    if (status === 409) {
      decisionResponse.resolve(Response.json(conflict, { status }));
      await expect(decision).resolves.toMatchObject({ ok: false, kind: 'merge_required', conflict });
      expect(controller.mergeRequired?.candidateID ?? null).toBe(17);
    }
    await controller.setIdentityState('accepted');
    await vi.waitFor(() => expect(controller.rows).toEqual(historyRows));
    if (status === 503) {
      decisionResponse.resolve(Response.json({ error: 'unavailable', message: 'Decision unavailable' }, { status }));
      await expect(decision).resolves.toMatchObject({ ok: false, kind: 'error', status, message: 'Decision unavailable' });
      expect(controller.identityState).toBe('accepted');
      expect(controller.offset).toBe(0);
      expect(controller.decisionError).toBe('Decision unavailable');
      expect(controller.mergeRequired).toBeNull();
    } else {
      expect(controller.mergeRequired?.candidateID ?? null).toBe(17);
      expect(controller.decisionError).toBeNull();
    }
    await vi.waitFor(() => expect(controller.rows).toEqual(historyRows));
    expect(controller.isDecisionPending(17)).toBe(false);
    expect(requests.filter(request => request.method === 'POST')).toHaveLength(1);
  });

  it('keeps decision draft editing isolated by candidate', () => {
    const controller = new DirectoryReviewController(createAPIClient(vi.fn()));

    controller.setDecisionDraft(17, 'First note');
    controller.setDecisionDraft(18, 'Second note');
    controller.clearDecisionDraft(17);

    expect(controller.getDecisionDraft(17)).toBe('');
    expect(controller.getDecisionDraft(18)).toBe('Second note');
  });

  it('does not let a duplicate pending decision overwrite its row draft', async () => {
    const pending = deferredResponse();
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'GET') return page([candidate(17)]);
      return pending.promise;
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();

    const first = settle(controller, 17, 'reject', 'Original note');
    await vi.waitFor(() => expect(controller.isDecisionPending(17)).toBe(true));
    controller.setDecisionDraft(17, 'Overwrite attempt');
    const second = controller.queueDecision(controller.rows[0]!, 'reject');

    expect(controller.getDecisionDraft(17)).toBe('Original note');
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
    pending.resolve(Response.json({ error: 'unavailable', message: 'Decision failed' }, { status: 503 }));
    await Promise.all([first, second]);
    controller.destroy();
  });

  it('aborts page and decision requests when destroyed', async () => {
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

it.each(['accept', 'reject'] as const)('settles %s with the displayed request and invalidates all cluster summaries only on acceptance', async decision => {
  const writes: Array<{ path: string; body: unknown; keepalive: boolean }> = [];
  const settled = vi.fn();
  let decided = false;
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const request = requestOf(input);
    if (request.method === 'POST') {
      writes.push({ path: new URL(request.url).pathname, body: await request.json(), keepalive: request.keepalive });
      decided = true;
      return Response.json({ candidate: candidate(1, decision === 'accept' ? 'accepted' : 'rejected'), cache_state: 'ready' });
    }
    return page(decided ? [candidate(2)] : [candidate(1), candidate(2)]);
  }), undefined, settled);
  try {
    await controller.loadIdentityPage();
    const saved = { id: 7, name: 'Saved', address: 'Saved person', lastAt: null };
    controller.summaries.set('participant:20', saved);
    controller.summaries.set('participant:11', saved);
    controller.setDecisionDraft(1, '  Different people  ');
    controller.queueDecision(controller.rows[0]!, decision);
    await controller.settleDecision();
    await vi.waitFor(() => expect(settled).toHaveBeenCalledOnce());
    expect(writes).toEqual([{ path: `/api/v1/identity/match-candidates/1/review/${decision}`, body: { review_token: 'token-1-candidate', notes: 'Different people' }, keepalive: true }]);
    expect(controller.rows.map(row => row.id)).toEqual([2]);
    expect(controller.getDecisionDraft(1)).toBe('');
    expect(controller.summaries.has('participant:20')).toBe(decision === 'reject');
    expect(controller.summaries.has('participant:11')).toBe(decision === 'reject');
  } finally { controller.destroy(); }
});

function queuedReview() {
  const decision = deferredResponse();
  let decided = false;
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const request = requestOf(input);
    if (request.method === 'POST') { const response = await decision.promise; decided = true; return response; }
    const url = new URL(request.url);
    if (url.searchParams.get('state') === 'accepted,rejected') return page(decided ? [candidate(1, 'rejected')] : []);
    return page([candidate(decided ? 2 : 1)]);
  }));
  return { controller, decision };
}

it('preserves newer To review restoration while settling History navigation', async () => {
  const { controller, decision } = queuedReview();
  try {
    await controller.loadIdentityPage();
    await controller.queueDecision(controller.rows[0]!, 'reject');
    const navigation = controller.setIdentityState('accepted');
    expect(controller.identityState).toBe('candidate');
    await vi.waitFor(() => expect(controller.isDecisionPending(1)).toBe(true));
    controller.applyURLState({ identityState: 'candidate' }, true);
    await vi.waitFor(() => expect(controller.loading).toBe(false));
    decision.resolve(Response.json({ candidate: candidate(1, 'rejected'), cache_state: 'ready' }));
    await navigation;
    await vi.waitFor(() => expect(controller.loading).toBe(false));
    expect(controller.identityState).toBe('candidate');
    expect(controller.offset).toBe(0);
    expect(controller.rows).toEqual([candidate(2)]);
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

it('settles a queued decision before an active invalidation replaces its page', async () => {
  const decision = deferredResponse();
  let reads = 0;
  const order: string[] = [];
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const request = requestOf(input);
    order.push(request.method);
    if (request.method === 'POST') {
      const response = await decision.promise;
      order.push('POST resolved');
      return response;
    }
    return page([candidate(++reads)]);
  }));
  try {
    await controller.loadIdentityPage();
    await controller.queueDecision(controller.rows[0]!, 'reject');
    const invalidation = controller.invalidate(true);
    await vi.waitFor(() => expect(order).toEqual(['GET', 'POST']));
    decision.resolve(Response.json({ candidate: candidate(1, 'rejected'), cache_state: 'ready' }));
    await invalidation;
    expect(order).toEqual(['GET', 'POST', 'POST resolved', 'GET', 'GET']);
    expect(controller.rows).toEqual([candidate(3)]);
    expect(controller.hidden.has(1)).toBe(false);
  } finally { decision.resolve(page([])); controller.destroy(); }
});

it.each(['next', 'previous', 'retry'] as const)('keeps the %s page destination after settling its queued decision', async direction => {
  const decision = deferredResponse(), destination = deferredResponse();
  const start = direction === 'previous' ? 10 : 0;
  const target = direction === 'previous' ? 0 : 10;
  let moving = false, decided = false, posted = false, destinationRead = false;
  const rows = Array.from({ length: 11 }, (_, index) => candidate(start + index + 1));
  const controller = new DirectoryReviewController(createAPIClient(async input => {
    const request = requestOf(input);
    if (request.method === 'POST') { posted = true; const response = await decision.promise; decided = true; return response; }
    const offset = Number(new URL(request.url).searchParams.get('offset'));
    if (!moving && offset === target && direction === 'retry') return Response.json({ message: 'Queue unavailable' }, { status: 503 });
    if (moving && offset === target) { destinationRead = true; return destination.promise; }
    return page(decided ? rows.slice(1) : rows, offset);
  }));
  try {
    await controller.loadIdentityPage(start);
    if (direction === 'retry') await controller.loadNextPage();
    await controller.queueDecision(controller.rows[0]!, 'reject');
    moving = true;
    const navigation = direction === 'next' ? controller.loadNextPage() : direction === 'previous' ? controller.loadPreviousPage() : controller.retryPage();
    await vi.waitFor(() => expect(posted).toBe(true));
    decision.resolve(Response.json({ candidate: candidate(start + 1, 'rejected'), cache_state: 'ready' }));
    await vi.waitFor(() => expect(controller.pendingDecisions.size).toBe(0));
    await vi.waitFor(() => expect(destinationRead).toBe(true));
    destination.resolve(page([candidate(101)], target));
    await navigation;
    expect(controller.offset).toBe(target);
    expect(controller.rows.map(row => row.id)).toEqual([101]);
  } finally { controller.destroy(); }
});
