import { addressFixture } from './merge-fixtures.test-support';
import { emptyProfile } from './profile-fixtures.test-support';
import { mergeResult, person, requestOf } from './entity-fixtures.test-support';
import { afterEach, describe, expect, it, vi } from 'vitest';

import * as requestID from '../api/request-id';
import { createAPIClient } from '../api/client';
import type {
  PersonMergeDetail as GeneratedPersonMergeDetail,
  PersonSplitResult as GeneratedPersonSplitResult,
} from '../api/generated/models';
import { PERSON_MERGE_HISTORY_LIMIT, PersonMergeHistoryController } from './person-merge-history-controller.svelte';

type PersonMergeDetail = GeneratedPersonMergeDetail;

afterEach(() => vi.restoreAllMocks());

function detail(
  overrides: Partial<PersonMergeDetail['merge']> = {},
  participants: PersonMergeDetail['participants'] = [
    { merge_id: 41, participant_id: 701, origin_side: 'absorbed' },
    { merge_id: 41, participant_id: 702, origin_side: 'survivor' },
  ],
): PersonMergeDetail {
  return {
    merge: { ...mergeResult(person(7)).merge, current_person_id: 12, ...overrides },
    labels: {
      profiles: [{ id: 7, display_name: 'Recorded Name', participant_ids: [702] }, { id: 9, display_name: 'Absorbed', participant_ids: [701] }],
      participant_ids: participants.map(({ participant_id }) => participant_id), addresses: participants.map(({ participant_id }) => addressFixture(participant_id, 'email', `address${participant_id}@example.org`)), absorbed_address_count: 1, incomplete: false,
    },
    participants,
    rows: [],
    splits: [],
    review_candidates: [],
  };
}

function summary(id: number) {
  const mergeDetail = detail({ id });
  return {
    merge: mergeDetail.merge,
    labels: { ...mergeDetail.labels, participants: mergeDetail.participants.map(participant => ({ ...participant, merge_id: id })) },
    participant_count: 1,
    pending_candidate_count: 0,
    row_action_counts: { restored: 2 },
    row_count: 2,
    split_count: 0,
  };
}

function splitResult(exact = true): GeneratedPersonSplitResult {
  return {
    exact_reversal: exact,
    cache_state: 'ready',
    identity_revision: 8,
    source_person: person(12, 5, [701, 702], 'Synthetic Source'),
    new_person: person(19, 1, [701, 702], 'Synthetic Restored'),
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
      created_at: '2026-08-04T00:00:00Z',
    },
    ambiguous_rows: exact ? [] : [{ table_name: 'person_names', original_row_key: 'concealed', action: 'ambiguous' }],
    unrestored_rows: [],
    uid_alias_disposition: 'restored',
  };
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((next) => {
    resolve = next;
  });
  return { promise, resolve };
}

describe('PersonMergeHistoryController', () => {
  it('keeps Split unavailable when the fresh merge read fails', async () => {
    let mergeReads = 0;
    const controller = new PersonMergeHistoryController(createAPIClient(async input => {
      const path = new URL(requestOf(input).url).pathname;
      if (path === '/api/v1/person-merges/41') return ++mergeReads === 1 ? Response.json(detail()) : Response.json({}, { status: 503 });
      if (path === '/api/v1/people/12') return Response.json(person(12, 5, [701, 702]), { headers: { ETag: '"person-12-r5"' } });
      throw new Error(`Unexpected request: ${path}`);
    }), 7);
    await controller.selectMerge(41);
    const oldDetail = controller.detail, oldLabels = controller.labels;
    await expect(controller.openSplit()).resolves.toBe(false);
    expect(controller.sourceError).toBeTruthy();
    expect(controller.sourceETag).toBeNull();
    expect(controller.detail).toBe(oldDetail);
    expect(controller.labels).toBe(oldLabels);
    expect(controller.confirmSplit()).toBe(false);
    controller.destroy();
  });

  it('reads profile before labels and recovers a link made between those reads through the stale path', async () => {
    const reads: string[] = [];
    const current = detail();
    current.labels.participant_ids.push(999);
    current.labels.addresses.push(addressFixture(999, 'email', 'later@example.org'));
    const firstProfile = deferredResponse();
    let revision = 4;
    const controller = new PersonMergeHistoryController(createAPIClient(async input => {
      const request = requestOf(input), path = new URL(request.url).pathname;
      if (path === '/api/v1/person-merges/41') {
        reads.push('merge');
        revision = 5;
        return Response.json(reads.filter(read => read === 'merge').length === 1 ? detail() : current);
      }
      if (path === '/api/v1/people/12') {
        reads.push('person');
        const currentRevision = revision;
        revision = 5;
        if (currentRevision === 4) return firstProfile.promise;
        return Response.json(person(12, currentRevision, [701, 702, 999]), { headers: { ETag: `"person-12-r${currentRevision}"` } });
      }
      if (path === '/api/v1/people/12/profile') return Response.json(emptyProfile(person(12)));
      if (path === '/api/v1/people/12/split') {
        expect(request.headers.get('If-Match')).toBe('"person-12-r4"');
        return Response.json({ error: 'person_merge_revision_conflict', message: 'Source changed' }, { status: 409 });
      }
      throw new Error(`Unexpected request: ${path}`);
    }), 7);
    await controller.selectMerge(41);
    revision = 4;
    const opening = controller.openSplit();
    await vi.waitFor(() => expect(reads).toContain('person'));
    expect(reads).toEqual(['merge', 'person']);
    firstProfile.resolve(Response.json(person(12, 4, [701, 702, 999]), { headers: { ETag: '"person-12-r4"' } }));
    await expect(opening).resolves.toBe(true);
    expect(controller.labels?.for).toBe(controller.detail);
    expect(controller.labels?.addresses.get(999)?.[0].title).toBe('later@example.org');
    expect(controller.sourceETag).toBe('"person-12-r4"');
    expect(reads).toEqual(['merge', 'person', 'merge']);
    controller.setParticipantSelected(701, true);
    expect(controller.confirmSplit()).toBe(true);
    await controller.submitSplit();
    expect(reads.slice(-2)).toEqual(['person', 'merge']);
    expect(controller.sourceETag).toBe('"person-12-r5"');
    expect(controller.selectedParticipantIDs).toEqual([]);
    expect(controller.splitNeedsFreshState).toBe(false);
    controller.destroy();
  });

  it('uses exact pagination and keeps Previous on a successful empty nonzero page', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const offset = Number(new URL(request.url).searchParams.get('offset'));
      const merges =
        offset === 0 ? Array.from({ length: PERSON_MERGE_HISTORY_LIMIT }, (_, index) => summary(index + 1)) : [];
      return Response.json({ merges, limit: PERSON_MERGE_HISTORY_LIMIT, offset });
    });
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);

    await controller.loadHistory();
    expect(requests.map(request => new URL(request.url).pathname)).toEqual(['/api/v1/people/7/merges']);
    expect(controller.historyRows.size).toBe(100);
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
      if (request.url.endsWith('/snapshot'))
        return Response.json({ version: 1, sha256: 'synthetic-digest', snapshot: { private: 'explicit' } });
      if (new URL(request.url).pathname === '/api/v1/person-merges/41') return Response.json(detail());
      throw new Error(`Unexpected request: ${request.url}`);
    });
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);

    await controller.selectMerge(41);
    expect(requests.filter(request => new URL(request.url).pathname.startsWith('/api/v1/person-merges/'))
      .map(request => new URL(request.url).pathname)).toEqual(['/api/v1/person-merges/41']);
    expect(controller.snapshot).toBeNull();

    await controller.revealSnapshot();
    await controller.revealSnapshot();
    expect(requests.filter(request => new URL(request.url).pathname.endsWith('/snapshot'))
      .map(request => new URL(request.url).pathname)).toEqual(['/api/v1/person-merges/41/snapshot']);
  });

  it.each(['person', 'merge'])('discards late labels after a newer %s', async replacement => {
    const late = deferredResponse();
    const requests: Request[] = [];
    const controller = new PersonMergeHistoryController(createAPIClient(async input => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/person-merges/41') return Response.json({ ...detail(), splits: [splitResult().split] });
      if (path === '/api/v1/people/19') return late.promise;
      if (path === '/api/v1/person-merges/42') return Response.json({ ...detail({ id: 42, survivor_person_id: 8 }, []), labels: { ...detail().labels, profiles: [{ id: 8, display_name: 'New Example', participant_ids: [] }] } });
      throw new Error(`Unexpected request: ${path}`);
    }), 7);
    const first = controller.selectMerge(41);
    await vi.waitFor(() => expect(controller.detail?.merge.id).toBe(41));
    expect(controller.labels?.for).toBe(controller.detail);
    expect(controller.labels?.names.has(19)).toBe(false);
    expect(controller.detailLoading).toBe(false);
    expect(controller.canOfferSplit).toBe(true);
    if (replacement === 'person') controller.setPerson(8);
    else await controller.selectMerge(42);
    const answer = Response.json(person(19, 1, [], 'Late Example'));
    const body = vi.spyOn(answer, 'text');
    late.resolve(answer);
    await vi.waitFor(() => expect(body).toHaveResolved());
    await first;
    await vi.waitFor(() => expect(controller.labels?.names.has(19)).not.toBe(true));
    expect(requests.find(request => new URL(request.url).pathname === '/api/v1/people/19')?.signal.aborted).toBe(true);
    expect(controller.labels?.names.get(7)).toBeUndefined();
    expect(controller.labels?.names.get(19)).toBeUndefined();
    if (replacement === 'merge') {
      expect(controller.labels?.for).toBe(controller.detail);
      expect(controller.labels?.names.get(8)).toBe('New Example');
    } else expect(controller.labels).toBeNull();
    controller.destroy();
  });

  it('retains loaded labels when a detail reload fails', async () => {
    let failing = false;
    const controller = new PersonMergeHistoryController(createAPIClient(async () => failing
      ? Response.json({}, { status: 503 }) : Response.json(detail())), 7);
    await controller.selectMerge(41);
    const labels = controller.labels;
    failing = true;
    await controller.retryDetail();
    expect(controller.detailError).toBeTruthy();
    expect(controller.labels).toBe(labels);
    expect(controller.labels?.for).toBe(controller.detail);
    controller.destroy();
  });

  it('uses detail current_person_id and its fresh strong ETag for the exact split request', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/person-merges/41') return Response.json(detail());
      if (path === '/api/v1/people/12' && request.method === 'GET') {
        return Response.json(person(12, 4, [701, 702], 'Synthetic Source'), { headers: { ETag: '"person-12-r4"' } });
      }
      return Response.json(splitResult(), {
        headers: { ETag: '"person-12-r5"', 'X-New-Person-ETag': '"person-19-r1"' },
      });
    });
    vi.spyOn(requestID, 'createBrowserRequestID').mockReturnValue('123-456-789-12');
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);

    await controller.selectMerge(41);
    await controller.openSplit();
    controller.setParticipantSelected(701, true);
    expect(controller.confirmSplit()).toBe(true);
    await controller.submitSplit();

    const post = requests.find((request) => request.method === 'POST')!;
    expect(new URL(post.url).pathname).toBe('/api/v1/people/12/split');
    expect(post.headers.get('If-Match')).toBe('"person-12-r4"');
    expect(post.headers.get('Idempotency-Key')).toBe('123-456-789-12');
    await expect(post.clone().json()).resolves.toEqual({ merge_id: 41, participant_ids: [701] });
  });

  it('requires a regular selection but permits an explicitly confirmed zero-participant lineage', async () => {
    let zeroLineage = true;
    let submitted: Request | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if(request.method === 'POST') { submitted = request; return Response.json(splitResult()); }
      if (new URL(request.url).pathname === '/api/v1/people/12') {
        return Response.json(person(12, 4, [701, 702], 'Synthetic Person 12'), { headers: { ETag: '"person-12-r4"' } });
      }
      return Response.json(detail({}, zeroLineage ? [] : undefined));
    });
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);
    await controller.selectMerge(41);
    await controller.openSplit();

    expect(controller.isZeroParticipantLineage).toBe(true);
    expect(controller.confirmSplit()).toBe(true);
    await controller.submitSplit();
    await expect(submitted!.clone().json()).resolves.toEqual({ merge_id: 41, participant_ids: [] });

    zeroLineage = false;
    const regular = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);
    await regular.selectMerge(41);
    await regular.openSplit();
    expect(regular.confirmSplit()).toBe(false);
  });

  it('reuses a request ID only after a thrown transport failure of the unchanged confirmation', async () => {
    const requests: Request[] = [];
    let postAttempts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/person-merges/41') return Response.json(detail());
      if (request.method === 'GET') return Response.json(person(12, 4, [701, 702], 'Synthetic Person 12'), { headers: { ETag: '"person-12-r4"' } });
      postAttempts += 1;
      if (postAttempts === 1) throw new TypeError('connection reset');
      return Response.json(splitResult());
    });
    const createRequestID = vi.spyOn(requestID, 'createBrowserRequestID')
      .mockReturnValueOnce('123-456-789-12')
      .mockReturnValueOnce('234-567-890-23');
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);
    await controller.selectMerge(41);
    await controller.openSplit();
    controller.setParticipantSelected(701, true);
    controller.confirmSplit();

    await controller.submitSplit();
    await controller.submitSplit();

    expect(createRequestID).toHaveBeenCalledOnce();
    const posts = requests.filter((request) => request.method === 'POST');
    expect(posts.map((request) => request.headers.get('Idempotency-Key'))).toEqual([
      '123-456-789-12',
      '123-456-789-12',
    ]);
  });

  it('rotates the request ID and requires reconfirmation after a thrown non-transport exception', async () => {
    const requests: Request[] = [];
    let postAttempts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/person-merges/41') return Response.json(detail());
      if (request.method === 'GET') return Response.json(person(12, 4, [701, 702], 'Synthetic Person 12'), { headers: { ETag: '"person-12-r4"' } });
      postAttempts += 1;
      if (postAttempts === 1) throw new SyntaxError('invalid response JSON');
      return Response.json(splitResult());
    });
    vi.spyOn(requestID, 'createBrowserRequestID')
      .mockReturnValueOnce('123-456-789-12')
      .mockReturnValueOnce('234-567-890-23');
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);
    await controller.selectMerge(41);
    await controller.openSplit();
    controller.setParticipantSelected(701, true);
    controller.confirmSplit();

    await controller.submitSplit();
    controller.confirmSplit();
    await controller.submitSplit();

    expect(
      requests.filter((request) => request.method === 'POST').map((request) => request.headers.get('Idempotency-Key')),
    ).toEqual(['123-456-789-12', '234-567-890-23']);
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
        return Response.json(person(12, revision, [701, 702], 'Synthetic Person 12'), { headers: { ETag: `"person-12-r${revision}"` } });
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
    expect(detailReads).toBe(3);
    expect(controller.sourcePerson?.revision).toBe(5);
    expect(controller.detail?.merge.survivor_revision_after).toBe(5);
    expect(controller.splitError).toBe('This profile changed since you opened it. Check its addresses, then split again.');
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
        if (detailReads === 4)
          return Response.json({ error: 'unavailable', message: 'Detail still unavailable' }, { status: 503 });
        return Response.json(detail({ survivor_revision_after: detailReads <= 2 ? 4 : 6 }));
      }
      if (path === '/api/v1/people/12') {
        sourceReads += 1;
        if (sourceReads === 2) return Response.json(person(12, 5, [701, 702], 'Unpublishable Source'));
        if (sourceReads === 3)
          return Response.json({ error: 'unavailable', message: 'Source still unavailable' }, { status: 503 });
        const revision = sourceReads === 1 ? 4 : 6;
        return Response.json(person(12, revision, [701, 702], revision === 4 ? 'Synthetic Source' : 'Fresh Name'), {
          headers: { ETag: `"person-12-r${revision}"` },
        });
      }
      if (request.method === 'GET') throw new Error(`Unexpected request: ${path}`);
      postAttempts += 1;
      return postAttempts === 1
        ? Response.json({ error: 'person_merge_revision_conflict', message: 'Source changed' }, { status: 409 })
        : Response.json(splitResult());
    });
    vi.spyOn(requestID, 'createBrowserRequestID')
      .mockReturnValueOnce('123-456-789-12')
      .mockReturnValueOnce('234-567-890-23');
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);
    await controller.selectMerge(41);
    expect(controller.labels?.names.get(7)).toBe('Recorded Name');
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
    expect(controller.sourcePerson?.display_name).toBe('Fresh Name');
    expect(controller.sourcePerson?.revision).toBe(6);
    expect(controller.sourceETag).toBe('"person-12-r6"');
    expect(controller.detail?.merge.survivor_revision_after).toBe(6);
    await vi.waitFor(() => expect(controller.labels?.names.get(7)).toBe('Recorded Name'));
    expect(controller.labels?.for).toBe(controller.detail);
    expect(requests.some(request => request.url.endsWith('/snapshot'))).toBe(false);
    expect(controller.selectedParticipantIDs).toEqual([]);
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);

    controller.setParticipantSelected(701, true);
    expect(controller.confirmSplit()).toBe(true);
    await controller.submitSplit();

    const posts = requests.filter((request) => request.method === 'POST');
    expect(posts).toHaveLength(2);
    expect(posts[1]!.headers.get('If-Match')).toBe('"person-12-r6"');
    expect(posts[1]!.headers.get('Idempotency-Key')).toBe('234-567-890-23');
  });

  it.each([
    [409, 'person_split_idempotency_conflict'],
    [412, 'person_merge_revision_conflict'],
  ])('does not stale-reload for %s/%s and rotates after the application failure', async (status, error) => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/person-merges/41') return Response.json(detail());
      if (request.method === 'GET') return Response.json(person(12, 4, [701, 702], 'Synthetic Person 12'), { headers: { ETag: '"person-12-r4"' } });
      return Response.json({ error, message: 'Application failure' }, { status });
    });
    vi.spyOn(requestID, 'createBrowserRequestID')
      .mockReturnValueOnce('123-456-789-12')
      .mockReturnValueOnce('234-567-890-23');
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);
    await controller.selectMerge(41);
    await controller.openSplit();
    controller.setParticipantSelected(701, true);
    controller.confirmSplit();
    await controller.submitSplit();
    controller.confirmSplit();
    await controller.submitSplit();

    expect(requests.filter(request => request.method === 'GET' &&
      ['/api/v1/person-merges/41', '/api/v1/people/12'].includes(new URL(request.url).pathname))
      .map(request => new URL(request.url).pathname)).toEqual(['/api/v1/person-merges/41', '/api/v1/people/12', '/api/v1/person-merges/41']);
    expect(
      requests.filter((request) => request.method === 'POST').map((request) => request.headers.get('Idempotency-Key')),
    ).toEqual(['123-456-789-12', '234-567-890-23']);
  });

  it('commits once, keeps malformed receipt tags out of state, and reports reconciliation separately', async () => {
    const reconcile = vi.fn(async (): Promise<void> => {
      throw new Error('refresh failed');
    });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/person-merges/41') return Response.json(detail());
      if (request.method === 'GET') return Response.json(person(12, 4, [701, 702], 'Synthetic Person 12'), { headers: { ETag: '"person-12-r4"' } });
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
    expect(controller.reconciliationError).toBe('The split is done. Open either profile to load it fresh.');
    expect(controller.canOfferSplit).toBe(false);
    reconcile.mockResolvedValueOnce(undefined);
    await controller.retryReconciliation();
    expect(controller.reconciliationError).toBeNull();
    expect(reconcile).toHaveBeenCalledTimes(2);
    expect(fetchFn.mock.calls.filter(([input]) => requestOf(input).method === 'POST')).toHaveLength(1);
  });

  it('retries a committed receipt with profile reads rather than another split', async () => {
    let failing = true;
    const requests: Request[] = [];
    const controller = new PersonMergeHistoryController(createAPIClient(async input => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/person-merges/41') return Response.json(detail());
      if (request.method === 'POST') return Response.json(splitResult());
      if (path === '/api/v1/people/12') return Response.json(person(12, 4, [701, 702]), { headers: { ETag: '"person-12-r4"' } });
      if (path === '/api/v1/people/12/profile') return Response.json(emptyProfile(person(12)));
      if (path === '/api/v1/people/19/profile') return failing
        ? Response.json({ message: 'Profile store busy' }, { status: 503 })
        : Response.json(emptyProfile(person(19)));
      throw new Error(`Unexpected ${request.method} ${path}`);
    }), 7);
    await controller.selectMerge(41);
    await controller.openSplit();
    controller.setParticipantSelected(701, true);
    controller.confirmSplit();
    await controller.submitSplit();
    const committed = controller.committedResult;
    expect(committed).not.toBeNull();
    expect(controller.receiptSavedError).toBe('Profile store busy');
    const beforeRetry = requests.length;
    failing = false;
    await controller.retryReceiptSaved();
    expect(requests.slice(beforeRetry).map(request => `${request.method} ${new URL(request.url).pathname}`)).toEqual([
      'GET /api/v1/people/12/profile', 'GET /api/v1/people/19/profile',
    ]);
    expect(requests.filter(request => request.method === 'POST')).toHaveLength(1);
    expect(controller.committedResult).toBe(committed);
    expect(controller.receiptSaved).toEqual({ source: [], created: [] });
    expect(controller.receiptSavedError).toBeNull();
    controller.destroy();
  });

  it('discards a late reconciliation rejection after person context changes', async () => {
    let rejectReconciliation!: (cause: unknown) => void;
    const reconciliation = new Promise<void>((_resolve, reject) => {
      rejectReconciliation = reject;
    });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/person-merges/41') return Response.json(detail());
      if (path === '/api/v1/people/12') return Response.json(person(12, 4, [701, 702], 'Synthetic Person 12'), { headers: { ETag: '"person-12-r4"' } });
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

  it('discards late history, detail, snapshot, and committed split effects after their context is replaced', async () => {
    const lateHistory = deferredResponse();
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
        return detailReads <= 2 ? Response.json(detail()) : lateDetail.promise;
      }
      if (path.endsWith('/snapshot')) return lateSnapshot.promise;
      if (request.method === 'POST') return lateSplit.promise;
      if (path === '/api/v1/people/12') return Response.json(person(12, 4, [701, 702], 'Synthetic Person 12'), { headers: { ETag: '"person-12-r4"' } });
      if (path === '/api/v1/people/7/merges') return lateHistory.promise;
      if (path.endsWith('/merges')) return Response.json({ merges: [summary(99)], limit: 100, offset: 0 });
      throw new Error(`Unexpected request: ${path}`);
    });
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);
    await controller.selectMerge(41);
    await controller.openSplit();
    controller.setParticipantSelected(701, true);
    controller.confirmSplit();

    const historyRead = controller.loadHistory();
    const snapshotRead = controller.revealSnapshot();
    const splitWrite = controller.submitSplit();
    const detailRead = controller.selectMerge(41);
    await vi.waitFor(() => {
      expect(requests.filter(request => new URL(request.url).pathname === '/api/v1/person-merges/41')).toHaveLength(3);
      expect(requests.filter(request => new URL(request.url).pathname.endsWith('/snapshot'))).toHaveLength(1);
      expect(requests.filter(request => request.method === 'POST')).toHaveLength(1);
    });
    controller.setPerson(8);

    lateHistory.resolve(Response.json({ merges: [summary(41)], limit: 100, offset: 0 }));
    lateDetail.resolve(Response.json(detail({ current_person_id: 99 })));
    lateSnapshot.resolve(Response.json({ version: 1, sha256: 'late', snapshot: { late: true } }));
    lateSplit.resolve(Response.json(splitResult()));
    await Promise.all([historyRead, detailRead, snapshotRead, splitWrite]);

    const lateRequests = [
      requests.find(request => new URL(request.url).pathname === '/api/v1/people/7/merges')!,
      requests.filter(request => new URL(request.url).pathname === '/api/v1/person-merges/41').at(-1)!,
      requests.find(request => new URL(request.url).pathname.endsWith('/snapshot'))!,
      requests.find(request => request.method === 'POST')!,
    ];
    expect(lateRequests.every((request) => request.signal.aborted)).toBe(true);
    expect(controller.personID).toBe(8);
    await vi.waitFor(() => expect(controller.history[0]?.merge.id).toBe(99));
    expect(controller.detail).toBeNull();
    expect(controller.snapshot).toBeNull();
    expect(controller.committedResult).toBeNull();
  });
  it.each(['merge', 'person', 'close and reopen'] as const)('opens Split before saved addresses load and drops them after a %s change', async change => {
    const saved = [deferredResponse(), deferredResponse()];
    let savedReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path === '/api/v1/person-merges/41') return Response.json(detail());
      if (path === '/api/v1/people/12') return Response.json(person(12, 4, [701, 702], 'Synthetic Source'), { headers: { ETag: '"person-12-r4"' } });
      if (path === '/api/v1/people/12/profile') return saved[savedReads++]!.promise;
      return Response.json({ merges: [], limit: 100, offset: 0 });
    });
    const controller = new PersonMergeHistoryController(createAPIClient(fetchFn), 7);
    await controller.selectMerge(41);

    await expect(controller.openSplit()).resolves.toBe(true);
    expect(controller.splitBusy).toBe(false);
    controller.setParticipantSelected(701, true);
    expect(controller.confirmSplit()).toBe(true);

    if (change === 'merge') await controller.selectMerge(41);
    else if (change === 'person') controller.setPerson(8);
    else {
      controller.closeSplit();
      expect(fetchFn.mock.calls.find(([input]) => new URL(requestOf(input).url).pathname === '/api/v1/people/12/profile')![0]).toHaveProperty('signal.aborted', true);
      await expect(controller.openSplit()).resolves.toBe(true);
    }
    const answer = Response.json({ ...emptyProfile(person(12)), contact_points: [{ address_kind: 'email', original_value: 'late@example.org', normalized_value: 'late@example.org' }] });
    const body = vi.spyOn(answer, 'text');
    saved[0]!.resolve(answer);
    await vi.waitFor(() => expect(body).toHaveResolved());
    await vi.waitFor(() => {
      expect(controller.sourceSaved).toBeNull();
      expect(controller.sourceSavedError).toBeNull();
    });
    saved[1]!.resolve(Response.json({}));
    controller.destroy();
  });

  it('records a failed snapshot reveal', async () => {
    const controller = new PersonMergeHistoryController(createAPIClient(async input => {
      const path = new URL(requestOf(input).url).pathname;
      if (path === '/api/v1/person-merges/41') return Response.json(detail());
      if (path.endsWith('/snapshot')) return Response.json({}, { status: 503 });
      throw new Error(`Unexpected request: ${path}`);
    }), 12);
    await controller.selectMerge(41);
    await controller.revealSnapshot();
    expect(controller.snapshotError).toBe('The server answered 503.');
    controller.destroy();
  });
});
