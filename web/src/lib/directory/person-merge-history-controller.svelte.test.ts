import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import type { components } from '../api/generated/schema';
import {
  PERSON_MERGE_HISTORY_LIMIT,
  PersonMergeHistoryController
} from './person-merge-history-controller.svelte';

type Person = components['schemas']['Person'];
type PersonMergeDetail = components['schemas']['PersonMergeDetail'];

afterEach(() => vi.restoreAllMocks());

function requestOf(input: RequestInfo | URL): Request {
  return input instanceof Request ? input : new Request(input);
}

function person(id: number, revision = 4, name = `Synthetic Person ${id}`): Person {
  return {
    id,
    revision,
    display_name: name,
    participant_ids: [701, 702],
    created_at: '2026-08-01T00:00:00Z',
    updated_at: '2026-08-02T00:00:00Z',
    vcard_uid: `synthetic-${id}`
  };
}

function detail(overrides: Partial<PersonMergeDetail['merge']> = {}, participants: PersonMergeDetail['participants'] = [
  { merge_id: 41, participant_id: 701, origin_side: 'absorbed' },
  { merge_id: 41, participant_id: 702, origin_side: 'survivor' }
]): PersonMergeDetail {
  return {
    merge: {
      id: 41,
      survivor_person_id: 7,
      absorbed_person_id: 9,
      current_person_id: 12,
      survivor_vcard_uid: 'synthetic-7',
      absorbed_vcard_uid: 'synthetic-9',
      survivor_revision_before: 3,
      absorbed_revision_before: 2,
      survivor_revision_after: 4,
      actor: 'web',
      snapshot_version: 1,
      snapshot_sha256: 'synthetic-digest',
      created_at: '2026-08-03T00:00:00Z',
      ...overrides
    },
    participants,
    rows: [],
    splits: [],
    review_candidates: []
  };
}

function summary(id: number) {
  const mergeDetail = detail({ id });
  return {
    merge: mergeDetail.merge,
    participant_count: 1,
    pending_candidate_count: 0,
    row_action_counts: { restored: 2 },
    row_count: 2,
    split_count: 0
  };
}

function splitResult(exact = true): components['schemas']['PersonSplitResult'] {
  return {
    exact_reversal: exact,
    cache_state: 'ready',
    identity_revision: 8,
    source_person: person(12, 5, 'Synthetic Source'),
    new_person: person(19, 1, 'Synthetic Restored'),
    split: {
      id: 55,
      merge_id: 41,
      source_person_id: 12,
      new_person_id: 19,
      new_person_uid: 'synthetic-19',
      source_revision_before: 4,
      source_revision_after: 5,
      exact_reversal: exact,
      actor: 'web',
      created_at: '2026-08-04T00:00:00Z'
    },
    ambiguous_rows: exact ? [] : [{ table_name: 'person_names', original_row_key: 'concealed', action: 'ambiguous' }],
    unrestored_rows: [],
    uid_alias_disposition: 'restored'
  };
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((next) => { resolve = next; });
  return { promise, resolve };
}

describe('PersonMergeHistoryController', () => {
  it('uses exact pagination and keeps Previous on a successful empty nonzero page', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const offset = Number(new URL(request.url).searchParams.get('offset'));
      const merges = offset === 0 ? Array.from({ length: PERSON_MERGE_HISTORY_LIMIT }, (_, index) => summary(index + 1)) : [];
      return Response.json({ merges, limit: PERSON_MERGE_HISTORY_LIMIT, offset });
    });
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);

    await controller.loadHistory();
    await controller.nextHistoryPage();

    expect(new URL(requests[0]!.url).searchParams.toString()).toBe('limit=100&offset=0');
    expect(new URL(requests[1]!.url).searchParams.toString()).toBe('limit=100&offset=100');
    expect(controller.history).toEqual([]);
    expect(controller.historyOffset).toBe(100);
    expect(controller.hasPreviousHistoryPage).toBe(true);
    expect(controller.hasNextHistoryPage).toBe(false);
  });

  it('loads detail without exposing the verified snapshot until an explicit reveal', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (request.url.endsWith('/snapshot')) return Response.json({ version: 1, sha256: 'synthetic-digest', snapshot: { private: 'explicit' } });
      return Response.json(detail());
    });
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);

    await controller.selectMerge(41);
    expect(requests.map((request) => new URL(request.url).pathname)).toEqual(['/api/v1/person-merges/41']);
    expect(controller.snapshot).toBeNull();

    await controller.revealSnapshot();
    await controller.revealSnapshot();
    expect(requests.map((request) => new URL(request.url).pathname)).toEqual([
      '/api/v1/person-merges/41', '/api/v1/person-merges/41/snapshot'
    ]);
  });

  it('uses detail current_person_id and its fresh strong ETag for the exact split request', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/person-merges/41') return Response.json(detail());
      if (path === '/api/v1/people/12' && request.method === 'GET') {
        return Response.json(person(12, 4, 'Synthetic Source'), { headers: { ETag: '"person-12-r4"' } });
      }
      return Response.json(splitResult(), { headers: { ETag: '"person-12-r5"', 'X-New-Person-ETag': '"person-19-r1"' } });
    });
    vi.spyOn(globalThis.crypto, 'randomUUID').mockReturnValue('11111111-1111-4111-8111-111111111111');
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);

    await controller.selectMerge(41);
    await controller.openSplit();
    controller.setParticipantSelected(701, true);
    expect(controller.confirmSplit()).toBe(true);
    await controller.submitSplit();

    const post = requests.find((request) => request.method === 'POST')!;
    expect(new URL(post.url).pathname).toBe('/api/v1/people/12/split');
    expect(post.headers.get('If-Match')).toBe('"person-12-r4"');
    expect(post.headers.get('Idempotency-Key')).toBe('11111111-1111-4111-8111-111111111111');
    await expect(post.clone().json()).resolves.toEqual({ merge_id: 41, participant_ids: [701] });
  });

  it('requires a regular selection but permits an explicitly confirmed zero-participant lineage', async () => {
    let zeroLineage = true;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (new URL(request.url).pathname === '/api/v1/people/12') {
        return Response.json(person(12), { headers: { ETag: '"person-12-r4"' } });
      }
      return Response.json(detail({}, zeroLineage ? [] : undefined));
    });
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);
    await controller.selectMerge(41);
    await controller.openSplit();

    expect(controller.isZeroParticipantLineage).toBe(true);
    expect(controller.confirmSplit()).toBe(true);
    expect(controller.confirmedParticipantIDs).toEqual([]);

    zeroLineage = false;
    const regular = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);
    await regular.selectMerge(41);
    await regular.openSplit();
    expect(regular.confirmSplit()).toBe(false);
  });

  it('reuses a UUID only after a thrown transport failure of the unchanged confirmation', async () => {
    const requests: Request[] = [];
    let postAttempts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/person-merges/41') return Response.json(detail());
      if (request.method === 'GET') return Response.json(person(12), { headers: { ETag: '"person-12-r4"' } });
      postAttempts += 1;
      if (postAttempts === 1) throw new TypeError('connection reset');
      return Response.json(splitResult());
    });
    vi.spyOn(globalThis.crypto, 'randomUUID').mockReturnValue('11111111-1111-4111-8111-111111111111');
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);
    await controller.selectMerge(41);
    await controller.openSplit();
    controller.setParticipantSelected(701, true);
    controller.confirmSplit();

    await controller.submitSplit();
    await controller.submitSplit();

    const posts = requests.filter((request) => request.method === 'POST');
    expect(posts.map((request) => request.headers.get('Idempotency-Key'))).toEqual([
      '11111111-1111-4111-8111-111111111111', '11111111-1111-4111-8111-111111111111'
    ]);
  });

  it('rotates the UUID and requires reconfirmation after a thrown non-transport exception', async () => {
    const requests: Request[] = [];
    let postAttempts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/person-merges/41') return Response.json(detail());
      if (request.method === 'GET') return Response.json(person(12), { headers: { ETag: '"person-12-r4"' } });
      postAttempts += 1;
      if (postAttempts === 1) throw new SyntaxError('invalid response JSON');
      return Response.json(splitResult());
    });
    vi.spyOn(globalThis.crypto, 'randomUUID')
      .mockReturnValueOnce('11111111-1111-4111-8111-111111111111')
      .mockReturnValueOnce('22222222-2222-4222-8222-222222222222');
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);
    await controller.selectMerge(41);
    await controller.openSplit();
    controller.setParticipantSelected(701, true);
    controller.confirmSplit();

    await controller.submitSplit();
    expect(controller.confirmedParticipantIDs).toBeNull();
    controller.confirmSplit();
    await controller.submitSplit();

    expect(requests.filter((request) => request.method === 'POST').map((request) => request.headers.get('Idempotency-Key')))
      .toEqual(['11111111-1111-4111-8111-111111111111', '22222222-2222-4222-8222-222222222222']);
  });

  it('atomically reloads exact source/detail only for the recognized stale code and requires reconfirmation', async () => {
    const requests: Request[] = [];
    let sourceReads = 0;
    let detailReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/person-merges/41') {
        detailReads += 1;
        return Response.json(detail({ survivor_revision_after: detailReads === 1 ? 4 : 5 }));
      }
      if (path === '/api/v1/people/12') {
        sourceReads += 1;
        const revision = sourceReads === 1 ? 4 : 5;
        return Response.json(person(12, revision), { headers: { ETag: `"person-12-r${revision}"` } });
      }
      return Response.json({ error: 'person_merge_revision_conflict', message: 'Source changed' }, { status: 409 });
    });
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);
    await controller.selectMerge(41);
    await controller.openSplit();
    controller.setParticipantSelected(701, true);
    controller.confirmSplit();

    await controller.submitSplit();

    expect(sourceReads).toBe(2);
    expect(detailReads).toBe(2);
    expect(controller.sourcePerson?.revision).toBe(5);
    expect(controller.detail?.merge.survivor_revision_after).toBe(5);
    expect(controller.confirmedParticipantIDs).toBeNull();
    expect(controller.splitError).toContain('Confirm');
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
  });

  it('blocks stale state after an atomic reload failure until GET-only retry publishes fresh source and detail', async () => {
    const requests: Request[] = [];
    let sourceReads = 0;
    let detailReads = 0;
    let postAttempts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/person-merges/41') {
        detailReads += 1;
        if (detailReads === 3) return Response.json({ error: 'unavailable', message: 'Detail still unavailable' }, { status: 503 });
        return Response.json(detail({ survivor_revision_after: detailReads === 1 ? 4 : 6 }));
      }
      if (path === '/api/v1/people/12') {
        sourceReads += 1;
        if (sourceReads === 2) return Response.json(person(12, 5, 'Unpublishable Source'));
        if (sourceReads === 3) return Response.json({ error: 'unavailable', message: 'Source still unavailable' }, { status: 503 });
        const revision = sourceReads === 1 ? 4 : 6;
        return Response.json(person(12, revision, revision === 4 ? 'Synthetic Source' : 'Fresh Synthetic Source'), {
          headers: { ETag: `"person-12-r${revision}"` }
        });
      }
      postAttempts += 1;
      return postAttempts === 1
        ? Response.json({ error: 'person_merge_revision_conflict', message: 'Source changed' }, { status: 409 })
        : Response.json(splitResult());
    });
    vi.spyOn(globalThis.crypto, 'randomUUID')
      .mockReturnValueOnce('11111111-1111-4111-8111-111111111111')
      .mockReturnValueOnce('22222222-2222-4222-8222-222222222222');
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);
    await controller.selectMerge(41);
    await controller.openSplit();
    controller.setParticipantSelected(701, true);
    controller.confirmSplit();

    await controller.submitSplit();

    expect(controller.splitNeedsFreshState).toBe(true);
    expect(controller.sourcePerson?.revision).toBe(4);
    expect(controller.sourceETag).toBe('"person-12-r4"');
    expect(controller.detail?.merge.survivor_revision_after).toBe(4);
    expect(controller.confirmSplit()).toBe(false);
    await controller.submitSplit();
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);

    await expect(controller.retryStaleSplitState()).resolves.toBe(false);

    expect(controller.splitNeedsFreshState).toBe(true);
    expect(controller.sourcePerson?.revision).toBe(4);
    expect(controller.detail?.merge.survivor_revision_after).toBe(4);
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
    expect(requests.slice(-2).every((request) => request.method === 'GET')).toBe(true);

    await expect(controller.retryStaleSplitState()).resolves.toBe(true);

    expect(controller.splitNeedsFreshState).toBe(false);
    expect(controller.sourcePerson?.display_name).toBe('Fresh Synthetic Source');
    expect(controller.sourcePerson?.revision).toBe(6);
    expect(controller.sourceETag).toBe('"person-12-r6"');
    expect(controller.detail?.merge.survivor_revision_after).toBe(6);
    expect(controller.selectedParticipantIDs).toEqual([]);
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);

    controller.setParticipantSelected(701, true);
    expect(controller.confirmSplit()).toBe(true);
    await controller.submitSplit();

    const posts = requests.filter((request) => request.method === 'POST');
    expect(posts).toHaveLength(2);
    expect(posts[1]!.headers.get('If-Match')).toBe('"person-12-r6"');
    expect(posts[1]!.headers.get('Idempotency-Key')).toBe('22222222-2222-4222-8222-222222222222');
  });

  it.each([
    [409, 'person_split_idempotency_conflict'],
    [412, 'person_merge_revision_conflict']
  ])('does not stale-reload for %s/%s and rotates after the application failure', async (status, error) => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/person-merges/41') return Response.json(detail());
      if (request.method === 'GET') return Response.json(person(12), { headers: { ETag: '"person-12-r4"' } });
      return Response.json({ error, message: 'Application failure' }, { status });
    });
    vi.spyOn(globalThis.crypto, 'randomUUID')
      .mockReturnValueOnce('11111111-1111-4111-8111-111111111111')
      .mockReturnValueOnce('22222222-2222-4222-8222-222222222222');
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);
    await controller.selectMerge(41);
    await controller.openSplit();
    controller.setParticipantSelected(701, true);
    controller.confirmSplit();
    await controller.submitSplit();
    controller.confirmSplit();
    await controller.submitSplit();

    expect(requests.filter((request) => request.method === 'GET')).toHaveLength(2);
    expect(requests.filter((request) => request.method === 'POST').map((request) => request.headers.get('Idempotency-Key')))
      .toEqual(['11111111-1111-4111-8111-111111111111', '22222222-2222-4222-8222-222222222222']);
  });

  it('commits once, keeps malformed receipt tags out of state, and reports reconciliation separately', async () => {
    const reconcile = vi.fn(async () => { throw new Error('refresh failed'); });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/person-merges/41') return Response.json(detail());
      if (request.method === 'GET') return Response.json(person(12), { headers: { ETag: '"person-12-r4"' } });
      return Response.json(splitResult(false), { headers: { ETag: 'bad', 'X-New-Person-ETag': 'also-bad' } });
    });
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7, reconcile);
    await controller.selectMerge(41);
    await controller.openSplit();
    controller.setParticipantSelected(701, true);
    controller.confirmSplit();

    await controller.submitSplit();
    await controller.submitSplit();

    expect(reconcile).toHaveBeenCalledOnce();
    expect(reconcile).toHaveBeenCalledWith({ sourcePersonID: 12, newPersonID: 19 });
    expect(controller.committedResult?.result.exact_reversal).toBe(false);
    expect(controller.committedResult?.receiptETags).toEqual({ source: null, created: null });
    expect(controller.reconciliationError).toContain('refresh');
    expect(controller.canOfferSplit).toBe(false);
  });

  it('discards a late reconciliation rejection after person context changes', async () => {
    let rejectReconciliation!: (cause: unknown) => void;
    const reconciliation = new Promise<void>((_resolve, reject) => { rejectReconciliation = reject; });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/person-merges/41') return Response.json(detail());
      if (path === '/api/v1/people/12') return Response.json(person(12), { headers: { ETag: '"person-12-r4"' } });
      if (request.method === 'POST') return Response.json(splitResult());
      return Response.json({ merges: [], limit: 100, offset: 0 });
    });
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7, () => reconciliation);
    await controller.selectMerge(41);
    await controller.openSplit();
    controller.setParticipantSelected(701, true);
    controller.confirmSplit();

    const submit = controller.submitSplit();
    await vi.waitFor(() => expect(controller.committedResult).not.toBeNull());
    controller.setPerson(8);
    rejectReconciliation(new Error('late refresh failure'));
    await submit;

    expect(controller.committedResult).toBeNull();
    expect(controller.reconciliationError).toBeNull();
  });

  it('aborts and discards every late effect when person context changes', async () => {
    const late = deferredResponse();
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (new URL(request.url).pathname === '/api/v1/people/7/merges') return late.promise;
      return Response.json({ merges: [summary(99)], limit: 100, offset: 0 });
    });
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);
    const first = controller.loadHistory();
    await vi.waitFor(() => expect(requests).toHaveLength(1));
    controller.setPerson(8);
    await vi.waitFor(() => expect(requests).toHaveLength(2));
    late.resolve(Response.json({ merges: [summary(41)], limit: 100, offset: 0 }));
    await first;

    expect(requests[0]!.signal.aborted).toBe(true);
    expect(controller.personID).toBe(8);
    expect(controller.history[0]?.merge.id).toBe(99);
    expect(controller.snapshot).toBeNull();
  });

  it('discards late detail, snapshot, and committed split effects after their context is replaced', async () => {
    const lateDetail = deferredResponse();
    const lateSnapshot = deferredResponse();
    const lateSplit = deferredResponse();
    const requests: Request[] = [];
    let detailReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/person-merges/41') {
        detailReads += 1;
        return detailReads === 1 ? Response.json(detail()) : lateDetail.promise;
      }
      if (path.endsWith('/snapshot')) return lateSnapshot.promise;
      if (request.method === 'POST') return lateSplit.promise;
      if (path === '/api/v1/people/12') return Response.json(person(12), { headers: { ETag: '"person-12-r4"' } });
      return Response.json({ merges: [], limit: 100, offset: 0 });
    });
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);
    await controller.selectMerge(41);
    await controller.openSplit();
    controller.setParticipantSelected(701, true);
    controller.confirmSplit();

    const snapshotRead = controller.revealSnapshot();
    const splitWrite = controller.submitSplit();
    const detailRead = controller.selectMerge(41);
    await vi.waitFor(() => expect(requests).toHaveLength(5));
    controller.setPerson(8);

    lateDetail.resolve(Response.json(detail({ current_person_id: 99 })));
    lateSnapshot.resolve(Response.json({ version: 1, sha256: 'late', snapshot: { late: true } }));
    lateSplit.resolve(Response.json(splitResult()));
    await Promise.all([detailRead, snapshotRead, splitWrite]);

    const lateRequests = requests.slice(2, 5);
    expect(lateRequests.every((request) => request.signal.aborted)).toBe(true);
    expect(controller.personID).toBe(8);
    expect(controller.detail).toBeNull();
    expect(controller.snapshot).toBeNull();
    expect(controller.committedResult).toBeNull();
  });
});
