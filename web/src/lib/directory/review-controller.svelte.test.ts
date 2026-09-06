import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import { DirectoryReviewController, IDENTITY_REVIEW_PAGE_LIMIT } from './review-controller.svelte';

function requestOf(input: RequestInfo | URL): Request {
  return input instanceof Request ? input : new Request(input);
}

function candidate(id: number, state = 'candidate') {
  return {
    id,
    left_id: id * 10,
    left_kind: 'beeper_user',
    right_id: id * 10 + 1,
    right_kind: 'participant',
    basis: 'stable_provider_id',
    source: 'synthetic',
    state,
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
    expect(query.get('state')).toBe('conflict');
    expect(query.get('limit')).toBe('100');
    expect(query.get('offset')).toBe('0');
    expect(controller.rows).toEqual([candidate(2, 'conflict')]);

    first.resolve(page([candidate(1)]));
    await candidateLoad;
    expect(controller.rows).toEqual([candidate(2, 'conflict')]);
  });

  it('retains the prior page after a later-page failure and retries the same offset', async () => {
    const firstPage = Array.from({ length: IDENTITY_REVIEW_PAGE_LIMIT }, (_, index) => candidate(index + 1));
    let secondAttempt = 0;
    const offsets: string[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const offset = new URL(request.url).searchParams.get('offset') ?? '';
      offsets.push(offset);
      if (offset === '0') return page(firstPage);
      secondAttempt += 1;
      if (secondAttempt === 1) return Response.json({ error: 'unavailable', message: 'Queue unavailable' }, { status: 503 });
      return page([candidate(101)], 100);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));

    await controller.loadIdentityPage();
    expect(controller.hasNextPage).toBe(true);
    await controller.loadNextPage();
    expect(controller.rows).toEqual(firstPage);
    expect(controller.offset).toBe(0);
    expect(controller.pageError).toBe('Queue unavailable');

    await controller.retryPage();
    expect(offsets).toEqual(['0', '100', '100']);
    expect(controller.rows).toEqual([candidate(101)]);
    expect(controller.offset).toBe(100);
    expect(controller.hasPreviousPage).toBe(true);
    expect(controller.hasNextPage).toBe(false);
  });

  it('resets an offset page synchronously when history restores the same queue', async () => {
    const restored = deferredResponse();
    let restoredAttempts = 0;
    const offsets: string[] = [];
    const firstPage = Array.from({ length: IDENTITY_REVIEW_PAGE_LIMIT }, (_, index) => candidate(index + 1));
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const offset = new URL(request.url).searchParams.get('offset') ?? '';
      offsets.push(offset);
      if (offset === '100') return page([candidate(101)], 100);
      if (offsets.length === 1) return page(firstPage);
      restoredAttempts += 1;
      if (restoredAttempts === 1) return restored.promise;
      return page([candidate(1)]);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();
    await controller.loadNextPage();
    expect(controller.offset).toBe(100);
    expect(controller.rows).toEqual([candidate(101)]);

    controller.applyURLState({ reviewKind: 'identity', identityState: 'candidate' }, true);

    expect(controller.offset).toBe(0);
    expect(controller.rows).toEqual([]);
    expect(controller.loading).toBe(true);
    restored.resolve(Response.json({ error: 'unavailable', message: 'Restoration failed' }, { status: 503 }));
    await vi.waitFor(() => expect(controller.error).toBe('Restoration failed'));
    expect(controller.pageError).toBeNull();
    expect(controller.offset).toBe(0);
    expect(controller.rows).toEqual([]);

    await controller.retryPage();
    expect(offsets).toEqual(['0', '100', '0', '0']);
    expect(controller.rows).toEqual([candidate(1)]);
  });

  it('clears candidate rows before a conflict-state load can fail', async () => {
    const conflict = deferredResponse();
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (requests.length === 1) return page([candidate(17)]);
      return conflict.promise;
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();

    controller.setIdentityState('conflict');

    expect(controller.identityState).toBe('conflict');
    expect(controller.offset).toBe(0);
    expect(controller.rows).toEqual([]);
    conflict.resolve(Response.json({ error: 'unavailable', message: 'Conflict queue failed' }, { status: 503 }));
    await vi.waitFor(() => expect(controller.error).toBe('Conflict queue failed'));
    expect(controller.pageError).toBeNull();
    expect(controller.rows).toEqual([]);
    expect(new URL(requests[1]!.url).searchParams.get('state')).toBe('conflict');
  });

  it('clears identity rows and offset when review kind changes', async () => {
    const firstPage = Array.from({ length: IDENTITY_REVIEW_PAGE_LIMIT }, (_, index) => candidate(index + 1));
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const offset = new URL(requestOf(input).url).searchParams.get('offset');
      return offset === '100' ? page([candidate(101)], 100) : page(firstPage);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();
    await controller.loadNextPage();

    controller.setReviewKind('fact');

    expect(controller.reviewKind).toBe('fact');
    expect(controller.offset).toBe(0);
    expect(controller.rows).toEqual([]);
  });

  it('switches to imported relationships without making an identity request', () => {
    const fetchFn = vi.fn<typeof fetch>();
    const commit = vi.fn();
    const controller = new DirectoryReviewController(createAPIClient(fetchFn), commit);

    controller.setReviewKind('relationship');

    expect(controller.reviewKind).toBe('relationship');
    expect(controller.rows).toEqual([]);
    expect(commit).toHaveBeenCalledOnce();
    expect(commit).toHaveBeenCalledWith({ reviewKind: 'relationship' });
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it('does not let a late successful decision reactivate identity review after switching to facts', async () => {
    const decisionResponse = deferredResponse();
    const requests: Request[] = [];
    const commit = vi.fn();
    const accepted = candidate(17, 'accepted');
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'POST') return decisionResponse.promise;
      return page([candidate(17)]);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn), commit);
    await controller.loadIdentityPage();
    controller.setDecisionDraft(17, 'Confirmed');

    const decision = controller.acceptIdentity(17);
    await vi.waitFor(() => expect(controller.isDecisionPending(17)).toBe(true));
    controller.setReviewKind('fact');
    decisionResponse.resolve(Response.json({ candidate: accepted, identity_revision: 4, cache_state: 'ready' }));

    await expect(decision).resolves.toEqual({ ok: true, candidate: accepted, cacheState: 'ready' });
    expect(controller.reviewKind).toBe('fact');
    expect(controller.rows).toEqual([]);
    expect(controller.offset).toBe(0);
    expect(controller.status).toBeNull();
    expect(controller.getDecisionDraft(17)).toBe('');
    expect(controller.isDecisionPending(17)).toBe(false);
    expect(requests.filter((request) => request.method === 'GET')).toHaveLength(1);
    expect(commit).toHaveBeenCalledTimes(1);
    expect(commit).toHaveBeenCalledWith({ reviewKind: 'fact' });
  });

  it('does not let a late success overwrite a restored copy of the same URL context', async () => {
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
      return Response.json({ error: 'unavailable', message: 'Unexpected reconciliation' }, { status: 503 });
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();

    const decision = controller.acceptIdentity(17, 'Confirmed');
    await vi.waitFor(() => expect(controller.isDecisionPending(17)).toBe(true));
    controller.applyURLState({ reviewKind: 'identity', identityState: 'candidate' }, true);
    await vi.waitFor(() => expect(controller.rows).toEqual(restoredRows));
    decisionResponse.resolve(Response.json({ candidate: accepted, identity_revision: 4, cache_state: 'ready' }));

    await expect(decision).resolves.toEqual({ ok: true, candidate: accepted, cacheState: 'ready' });
    expect(controller.reviewKind).toBe('identity');
    expect(controller.identityState).toBe('candidate');
    expect(controller.rows).toEqual(restoredRows);
    expect(controller.offset).toBe(0);
    expect(controller.pageError).toBeNull();
    expect(controller.status).toBeNull();
    expect(requests.filter((request) => request.method === 'GET')).toHaveLength(2);
  });

  it('retains the current same-state page when previous-page loading fails', async () => {
    const firstPage = Array.from({ length: IDENTITY_REVIEW_PAGE_LIMIT }, (_, index) => candidate(index + 1));
    let firstPageLoads = 0;
    const offsets: string[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const offset = new URL(requestOf(input).url).searchParams.get('offset') ?? '';
      offsets.push(offset);
      if (offset === '100') return page([candidate(101)], 100);
      firstPageLoads += 1;
      if (firstPageLoads === 1) return page(firstPage);
      return Response.json({ error: 'unavailable', message: 'Previous page failed' }, { status: 503 });
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();
    await controller.loadNextPage();

    await controller.loadPreviousPage();

    expect(offsets).toEqual(['0', '100', '0']);
    expect(controller.offset).toBe(100);
    expect(controller.rows).toEqual([candidate(101)]);
    expect(controller.pageError).toBe('Previous page failed');
    expect(controller.error).toBeNull();
  });

  it('applies a successful decision before best-effort reconciliation and sends only trimmed notes', async () => {
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
    controller.setDecisionDraft(17, '  Confirmed by user  ');

    const decision = controller.acceptIdentity(17);
    await vi.waitFor(() => expect(controller.rows).toEqual([accepted]));
    expect(controller.isDecisionPending(17)).toBe(true);
    const post = requests.find((request) => request.method === 'POST')!;
    expect(post.headers.has('If-Match')).toBe(false);
    expect(post.headers.has('Idempotency-Key')).toBe(false);
    await expect(post.clone().json()).resolves.toEqual({ notes: 'Confirmed by user' });

    reconcile.resolve(page([accepted]));
    await expect(decision).resolves.toEqual({ ok: true, candidate: accepted, cacheState: 'ready' });
    expect(controller.isDecisionPending(17)).toBe(false);
    expect(controller.getDecisionDraft(17)).toBe('');
    expect(controller.status).toContain('accepted');
  });

  it('keeps a failed reject note and never automatically retries the decision', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'GET') return page([candidate(17)]);
      return Response.json({ error: 'unavailable', message: 'Decision unavailable' }, { status: 503 });
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();

    await expect(controller.rejectIdentity(17, 'Different people')).resolves.toEqual({
      ok: false,
      kind: 'error',
      status: 503,
      message: 'Decision unavailable'
    });

    expect(controller.getDecisionDraft(17)).toBe('Different people');
    expect(controller.rows).toEqual([candidate(17)]);
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
  });

  it('uses the generated reject operation and applies an ordinary rejection', async () => {
    let reads = 0;
    const rejected = candidate(17, 'rejected');
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'POST') {
        return Response.json({ candidate: rejected, identity_revision: 6, cache_state: 'ready' });
      }
      reads += 1;
      return reads === 1 ? page([candidate(17)]) : page([rejected]);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();

    await expect(controller.rejectIdentity(17)).resolves.toEqual({
      ok: true,
      candidate: rejected,
      cacheState: 'ready'
    });

    const post = requests.find((request) => request.method === 'POST')!;
    expect(new URL(post.url).pathname).toBe('/api/v1/identity/match-candidates/17/reject');
    await expect(post.clone().json()).resolves.toEqual({});
    expect(controller.rows).toEqual([rejected]);
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

    await expect(controller.acceptIdentity(17)).resolves.toEqual({
      ok: true,
      candidate: accepted,
      cacheState: 'stale'
    });
    expect(controller.rows).toEqual([accepted]);
    expect(controller.pageError).toBe('Reload failed');
    expect(controller.decisionError).toBeNull();
  });

  it('captures a typed person-merge-required conflict without repeating acceptance', async () => {
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
      if (request.method === 'GET') return page([candidate(17)]);
      return Response.json(conflict, { status: 409 });
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();

    await expect(controller.acceptIdentity(17, 'Review both profiles')).resolves.toEqual({
      ok: false,
      kind: 'merge_required',
      conflict
    });
    expect(controller.mergeRequired).toEqual({ candidateID: 17, conflict });
    expect(controller.getDecisionDraft(17)).toBe('Review both profiles');
    expect(controller.rows).toEqual([candidate(17)]);
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
  });

  it('keeps malformed merge-required payloads on the ordinary error path', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      error: 'person_merge_required',
      message: 'Malformed conflict',
      profiles: [
        { etag: 'W/"person-7-r4"', person: { id: 7, revision: 4 } },
        { etag: '"person-7-r4"', person: { id: 7, revision: 4 } }
      ]
    }, { status: 409 }));
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));

    await expect(controller.acceptIdentity(17)).resolves.toEqual({
      ok: false, kind: 'error', status: 409, message: 'Malformed conflict'
    });
    expect(controller.mergeRequired).toBeNull();
  });

  it('records merge success, clears only its candidate draft, and refreshes only its captured context', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      requests.push(requestOf(input));
      return page([candidate(18)]);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    controller.rows = [candidate(17), candidate(18)];
    controller.setDecisionDraft(17, 'merge this one');
    controller.setDecisionDraft(18, 'keep this draft');
    const context = controller.reviewContextSnapshot();
    const success = {
      result: { cache_state: 'stale', identity_revision: 8, merge: {} as never, person: { id: 7 } as never, review_candidates: [] },
      survivor: { id: 7, display_name: 'Synthetic survivor' } as never,
      responseETag: '"person-7-r5"'
    } as unknown as import('./person-merge').PersonMergeSuccess;

    await controller.completePersonMerge(17, context, success);

    expect(controller.lastMerge).toEqual({ candidateID: 17, ...success });
    expect(controller.getDecisionDraft(17)).toBe('');
    expect(controller.getDecisionDraft(18)).toBe('keep this draft');
    expect(controller.status).toContain('People merged into Synthetic survivor. Identity cache stale.');
    expect(requests).toHaveLength(1);

    const staleContext = controller.reviewContextSnapshot();
    controller.setIdentityState('conflict');
    await vi.waitFor(() => expect(requests).toHaveLength(2));
    await controller.completePersonMerge(18, staleContext, success);
    expect(requests).toHaveLength(2);
    expect(controller.getDecisionDraft(18)).toBe('');
  });

  it('does not expose a late merge requirement in a newer identity state', async () => {
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
    const conflictRows = [candidate(88, 'conflict')];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'POST') return decisionResponse.promise;
      const state = new URL(request.url).searchParams.get('state');
      return state === 'conflict' ? page(conflictRows) : page([candidate(17)]);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();

    const decision = controller.acceptIdentity(17, 'Review both profiles');
    await vi.waitFor(() => expect(controller.isDecisionPending(17)).toBe(true));
    controller.setIdentityState('conflict');
    await vi.waitFor(() => expect(controller.rows).toEqual(conflictRows));
    decisionResponse.resolve(Response.json(conflict, { status: 409 }));

    await expect(decision).resolves.toEqual({ ok: false, kind: 'merge_required', conflict });
    expect(controller.identityState).toBe('conflict');
    expect(controller.rows).toEqual(conflictRows);
    expect(controller.mergeRequired).toBeNull();
    expect(controller.decisionError).toBeNull();
    expect(controller.getDecisionDraft(17)).toBe('Review both profiles');
    expect(controller.isDecisionPending(17)).toBe(false);
    expect(requests.filter((request) => request.method === 'GET')).toHaveLength(2);
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
  });

  it('does not expose a late decision error in a newer identity state', async () => {
    const decisionResponse = deferredResponse();
    const conflictRows = [candidate(88, 'conflict')];
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (request.method === 'POST') return decisionResponse.promise;
      const state = new URL(request.url).searchParams.get('state');
      return state === 'conflict' ? page(conflictRows) : page([candidate(17)]);
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();

    const decision = controller.rejectIdentity(17, 'Different people');
    await vi.waitFor(() => expect(controller.isDecisionPending(17)).toBe(true));
    controller.setIdentityState('conflict');
    await vi.waitFor(() => expect(controller.rows).toEqual(conflictRows));
    decisionResponse.resolve(Response.json({ error: 'unavailable', message: 'Decision unavailable' }, { status: 503 }));

    await expect(decision).resolves.toEqual({
      ok: false,
      kind: 'error',
      status: 503,
      message: 'Decision unavailable'
    });
    expect(controller.identityState).toBe('conflict');
    expect(controller.rows).toEqual(conflictRows);
    expect(controller.decisionError).toBeNull();
    expect(controller.mergeRequired).toBeNull();
    expect(controller.getDecisionDraft(17)).toBe('Different people');
    expect(controller.isDecisionPending(17)).toBe(false);
    expect(requests.filter((request) => request.method === 'GET')).toHaveLength(2);
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
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

    const first = controller.rejectIdentity(17, 'Original note');
    await vi.waitFor(() => expect(controller.isDecisionPending(17)).toBe(true));
    await expect(controller.rejectIdentity(17, 'Overwrite attempt')).resolves.toEqual({
      ok: false,
      kind: 'error',
      status: 0,
      message: 'A decision is already pending.'
    });

    expect(controller.getDecisionDraft(17)).toBe('Original note');
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
    pending.resolve(Response.json({ error: 'unavailable', message: 'Decision failed' }, { status: 503 }));
    await first;
    expect(controller.getDecisionDraft(17)).toBe('Original note');
  });

  it('clears only a successful row draft while a concurrent failed row retains its note', async () => {
    const failed = deferredResponse();
    const succeeded = deferredResponse();
    const accepted = candidate(18, 'accepted');
    let reads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (request.method === 'GET') {
        reads += 1;
        return page(reads === 1 ? [candidate(17), candidate(18)] : [candidate(17), accepted]);
      }
      return new URL(request.url).pathname.endsWith('/17/reject') ? failed.promise : succeeded.promise;
    });
    const controller = new DirectoryReviewController(createAPIClient(fetchFn));
    await controller.loadIdentityPage();

    const rejection = controller.rejectIdentity(17, 'Keep row 17 note');
    const acceptance = controller.acceptIdentity(18, 'Clear row 18 note');
    await vi.waitFor(() => expect(controller.pendingDecisions.size).toBe(2));
    succeeded.resolve(Response.json({ candidate: accepted, identity_revision: 7, cache_state: 'ready' }));
    await acceptance;

    expect(controller.getDecisionDraft(18)).toBe('');
    expect(controller.getDecisionDraft(17)).toBe('Keep row 17 note');
    failed.resolve(Response.json({ error: 'unavailable', message: 'Decision failed' }, { status: 503 }));
    await rejection;
    expect(controller.getDecisionDraft(17)).toBe('Keep row 17 note');
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
    void controller.rejectIdentity(17);
    await vi.waitFor(() => expect(requests).toHaveLength(2));
    controller.destroy();

    expect(requests[1]!.signal.aborted).toBe(true);
    expect(controller.pendingDecisions.size).toBe(0);
    expect(controller.getDecisionDraft(17)).toBe('');
    expect(controller.loading).toBe(false);
  });
});
