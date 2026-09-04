import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import {
  CardDAVConflictsController,
  type CardDAVConflictChoice
} from './conflicts-controller.svelte';

const forbiddenMarkers = {
  raw_vcard: 'BEGIN:VCARD\nFN:FORBIDDEN-VCARD\nEND:VCARD',
  url: 'https://forbidden-url.example.test/dav',
  href: '/forbidden-href/contact.vcf',
  etag: 'forbidden-etag',
  hash: 'forbidden-hash',
  uid: 'forbidden-uid',
  header: 'Authorization: forbidden-header',
  credential: 'forbidden-credential'
};

function listItem(id: number, overrides: Record<string, unknown> = {}) {
  return {
    id,
    address_book: { id: 7, name: 'Synthetic contacts', ...forbiddenMarkers },
    status: 'unresolved',
    local_state: 'present',
    remote_state: 'deleted',
    allowed_resolutions: ['keep_local', 'keep_remote'],
    updated_at: '2026-08-28T10:00:00Z',
    ...forbiddenMarkers,
    ...overrides
  };
}

function summary(state: 'present' | 'deleted' | 'unavailable', overrides: Record<string, unknown> = {}) {
  return {
    state,
    emails: [],
    phones: [],
    ...forbiddenMarkers,
    ...overrides
  };
}

function detail(id: number, overrides: Record<string, unknown> = {}) {
  return {
    id,
    address_book: { id: 7, name: 'Synthetic contacts', ...forbiddenMarkers },
    status: 'unresolved',
    base: summary('present', { display_name: 'Synthetic Base', emails: ['base@example.test'] }),
    local: summary('present', { display_name: 'Synthetic Local', phones: ['+1 555 0100'] }),
    remote: summary('deleted'),
    allowed_resolutions: ['keep_local', 'keep_remote'],
    created_at: '2026-08-27T10:00:00Z',
    updated_at: '2026-08-28T10:00:00Z',
    ...forbiddenMarkers,
    ...overrides
  };
}

function requestOf(input: RequestInfo | URL): Request {
  return input instanceof Request ? input : new Request(input);
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((settle) => { resolve = settle; });
  return { promise, resolve };
}

afterEach(() => vi.restoreAllMocks());

describe('CardDAVConflictsController', () => {
  it('loads the compact queue and exact selected detail while projecting only safe fields', async () => {
    const paths: string[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      paths.push(path);
      if (path === '/api/v1/carddav/conflicts') {
        return Response.json({ conflicts: [listItem(41)] });
      }
      if (path === '/api/v1/carddav/conflicts/41') return Response.json(detail(41));
      throw new Error(`Unexpected GET ${path}`);
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));

    await controller.load();
    await controller.select(41);

    expect(paths).toEqual(['/api/v1/carddav/conflicts', '/api/v1/carddav/conflicts/41']);
    expect(controller.conflicts).toEqual([{
      id: 41,
      address_book: { id: 7, name: 'Synthetic contacts' },
      status: 'unresolved',
      local_state: 'present',
      remote_state: 'deleted',
      allowed_resolutions: ['keep_local', 'keep_remote'],
      updated_at: '2026-08-28T10:00:00Z'
    }]);
    expect(controller.selectedDetail).toEqual({
      id: 41,
      address_book: { id: 7, name: 'Synthetic contacts' },
      status: 'unresolved',
      base: { state: 'present', display_name: 'Synthetic Base', emails: ['base@example.test'], phones: [] },
      local: { state: 'present', display_name: 'Synthetic Local', emails: [], phones: ['+1 555 0100'] },
      remote: { state: 'deleted', emails: [], phones: [] },
      allowed_resolutions: ['keep_local', 'keep_remote'],
      created_at: '2026-08-27T10:00:00Z',
      updated_at: '2026-08-28T10:00:00Z'
    });
    expect(JSON.stringify({ list: controller.conflicts, detail: controller.selectedDetail })).not.toMatch(
      /FORBIDDEN-VCARD|forbidden-(?:url|href|etag|hash|uid|header|credential)/i
    );
    controller.destroy();
  });

  it('keeps list and detail errors independent while invalidating failed detail refreshes', async () => {
    let listReads = 0;
    let detailReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path === '/api/v1/carddav/conflicts') {
        listReads += 1;
        if (listReads === 1) return Response.json({ conflicts: [listItem(41)] });
        if (listReads === 2) return Response.json({ error: 'unavailable', message: forbiddenMarkers.raw_vcard }, { status: 503 });
        return Response.json({ conflicts: [] });
      }
      detailReads += 1;
      if (detailReads === 1) return Response.json(detail(41));
      if (detailReads === 2) return Response.json({ error: 'unavailable', message: forbiddenMarkers.credential }, { status: 503 });
      return Response.json(detail(41, { updated_at: '2026-08-28T11:00:00Z' }));
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    await controller.select(41);

    await controller.retryList();
    expect(controller.conflicts).toHaveLength(1);
    expect(controller.listError).toBe('Unable to load CardDAV conflicts.');
    expect(controller.detailError).toBeNull();
    await controller.retrySelectedState();
    expect(controller.selectedDetail).toBeUndefined();
    expect(controller.isResolutionAllowed('keep_local')).toBe(false);
    expect(controller.detailError).toBe('Unable to load CardDAV conflict details.');
    expect(controller.listError).toBe('Unable to load CardDAV conflicts.');

    await controller.retryList();
    await controller.retrySelectedState();
    expect(controller.conflicts).toEqual([]);
    expect(controller.selectedDetail?.updated_at).toBe('2026-08-28T11:00:00Z');
    expect([controller.listError, controller.detailError]).toEqual([null, null]);
    expect([listReads, detailReads]).toEqual([3, 3]);
    controller.destroy();
  });

  it('treats typed unavailable detail as global optional state without retaining conflict data', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path === '/api/v1/carddav/conflicts') return Response.json({ conflicts: [listItem(41)] });
      return Response.json({
        error: 'carddav_unavailable',
        message: forbiddenMarkers.credential
      }, { status: 503 });
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();

    await controller.select(41);

    expect(controller.unavailable).toBe(true);
    expect(controller.conflicts).toEqual([]);
    expect(controller.selectedID).toBeUndefined();
    expect(controller.selectedDetail).toBeUndefined();
    expect(controller.detailError).toBeNull();
    controller.destroy();
  });

  it('detail unavailable aborts and invalidates an older list lane until an explicit recovery load', async () => {
    const olderList = deferredResponse();
    let listReads = 0;
    let olderListSignal: AbortSignal | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/carddav/conflicts') {
        listReads += 1;
        if (listReads === 1) return Response.json({ conflicts: [listItem(41)] });
        if (listReads === 2) {
          olderListSignal = request.signal;
          return olderList.promise;
        }
        return Response.json({ conflicts: [listItem(42)] });
      }
      return Response.json({ error: 'carddav_unavailable' }, { status: 503 });
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();

    const staleList = controller.retryList();
    await controller.select(41);

    expect(olderListSignal?.aborted).toBe(true);
    expect(controller.unavailable).toBe(true);
    expect(controller.conflicts).toEqual([]);
    expect([controller.listLoading, controller.detailLoading]).toEqual([false, false]);

    olderList.resolve(Response.json({ conflicts: [listItem(99)] }));
    await staleList;
    expect(controller.unavailable).toBe(true);
    expect(controller.conflicts).toEqual([]);
    expect([controller.listError, controller.detailError]).toEqual([null, null]);

    await controller.load();
    expect(controller.unavailable).toBe(false);
    expect(controller.conflicts.map(({ id }) => id)).toEqual([42]);
    expect([controller.listLoading, controller.detailLoading]).toEqual([false, false]);
    controller.destroy();
  });

  it('list unavailable aborts and settles an older detail lane until an explicit recovery load', async () => {
    const olderDetail = deferredResponse();
    let listReads = 0;
    let detailReads = 0;
    let olderDetailSignal: AbortSignal | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/carddav/conflicts') {
        listReads += 1;
        if (listReads === 2) return Response.json({ error: 'carddav_unavailable' }, { status: 503 });
        return Response.json({ conflicts: [listItem(41)] });
      }
      detailReads += 1;
      if (detailReads === 1) {
        olderDetailSignal = request.signal;
        return olderDetail.promise;
      }
      return Response.json(detail(41));
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();

    const staleDetail = controller.select(41);
    await controller.retryList();

    expect(olderDetailSignal?.aborted).toBe(true);
    expect(controller.unavailable).toBe(true);
    expect(controller.selectedID).toBeUndefined();
    expect([controller.listLoading, controller.detailLoading]).toEqual([false, false]);

    olderDetail.resolve(Response.json(detail(41)));
    await staleDetail;
    expect(controller.unavailable).toBe(true);
    expect(controller.selectedDetail).toBeUndefined();
    expect([controller.listError, controller.detailError]).toEqual([null, null]);

    await controller.load();
    expect(controller.unavailable).toBe(false);
    await controller.select(41);
    expect(controller.selectedDetail?.id).toBe(41);
    expect([controller.listLoading, controller.detailLoading]).toEqual([false, false]);
    controller.destroy();
  });

  it('ignores an older selected-detail response after a newer conflict is selected', async () => {
    const older = deferredResponse();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path.endsWith('/41')) return older.promise;
      if (path.endsWith('/42')) return Response.json(detail(42, { address_book: { id: 8, name: 'Second book' } }));
      return Response.json({ conflicts: [listItem(41), listItem(42)] });
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();

    const first = controller.select(41);
    await controller.select(42);
    older.resolve(Response.json(detail(41)));
    await first;

    expect(controller.selectedID).toBe(42);
    expect(controller.selectedDetail?.id).toBe(42);
    expect(controller.selectedDetail?.address_book.name).toBe('Second book');
    controller.destroy();
  });

  it('sends one exact generated choice and applies a clean receipt once', async () => {
    const post = deferredResponse();
    const requestFacts: Array<{ method: string; path: string; choice?: string }> = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') {
        const body = await request.clone().json();
        requestFacts.push({ method: request.method, path, choice: String(body.choice) });
        return post.promise;
      }
      requestFacts.push({ method: request.method, path });
      if (path.endsWith('/41')) return Response.json(detail(41));
      return Response.json({ conflicts: [listItem(41), listItem(42)] });
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    await controller.select(41);

    const first = controller.resolve(41, 'keep_local');
    const duplicate = await controller.resolve(41, 'keep_local');
    expect(controller.pendingResolutionID).toBe(41);
    expect(duplicate).toEqual({ kind: 'ignored' });
    post.resolve(Response.json({ id: 41, status: 'resolved', resolution: 'keep_local' }));
    expect(await first).toEqual({ kind: 'resolved' });

    expect(requestFacts.filter(({ method }) => method === 'POST')).toEqual([{
      method: 'POST', path: '/api/v1/carddav/conflicts/41/resolve', choice: 'keep_local'
    }]);
    expect(controller.conflicts.map(({ id }) => id)).toEqual([42]);
    expect(controller.selectedDetail?.status).toBe('resolved');
    expect(controller.selectedDetail?.allowed_resolutions).toEqual([]);
    expect(controller.announcement).toBe('CardDAV conflict 41 resolved by keeping the local card.');
    expect(controller.focusRequest).toEqual({ key: 1, conflictID: 42 });
    controller.destroy();
  });

  it.each([
    { name: 'typed stale response', response: () => Response.json({ error: 'carddav_conflict_stale', message: forbiddenMarkers.href }, { status: 409 }) },
    { name: 'typed pending response', response: () => Response.json({ error: 'carddav_conflict_pending', message: forbiddenMarkers.etag }, { status: 409 }) },
    { name: 'server ambiguity', response: () => Response.json({ error: 'unavailable', message: forbiddenMarkers.header }, { status: 503 }) },
    { name: 'transport ambiguity', response: () => new TypeError('connection reset') }
  ])('performs detail/list GET-only reconciliation after $name', async ({ response }) => {
    let posts = 0;
    let listReads = 0;
    let detailReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') {
        posts += 1;
        const result = response();
        if (result instanceof Error) throw result;
        return result;
      }
      if (path === '/api/v1/carddav/conflicts') {
        listReads += 1;
        return Response.json({ conflicts: [listItem(41, { updated_at: listReads === 1 ? '2026-08-28T10:00:00Z' : '2026-08-28T12:00:00Z' })] });
      }
      detailReads += 1;
      return Response.json(detail(41, {
        updated_at: detailReads === 1 ? '2026-08-28T10:00:00Z' : '2026-08-28T12:00:00Z',
        allowed_resolutions: detailReads === 1 ? ['keep_local', 'keep_remote'] : ['keep_remote']
      }));
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    await controller.select(41);

    expect(await controller.resolve(41, 'keep_local')).toEqual({ kind: 'reconciled' });

    expect([posts, listReads, detailReads]).toEqual([1, 2, 2]);
    expect(controller.selectedDetail?.updated_at).toBe('2026-08-28T12:00:00Z');
    expect(controller.selectedDetail?.allowed_resolutions).toEqual(['keep_remote']);
    expect(controller.resolutionUnknown).toBe(false);
    expect(controller.resolutionError).toContain('Current conflict state was refreshed');
    expect(JSON.stringify(controller)).not.toMatch(/forbidden-(?:href|etag|header)/i);
    controller.destroy();
  });

  it('locks mutation after failed reconciliation and a GET-only retry recovers both snapshots', async () => {
    let posts = 0;
    let listReads = 0;
    let detailReads = 0;
    let reconciliationFails = true;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') {
        posts += 1;
        return Response.json({ error: 'carddav_conflict_stale' }, { status: 409 });
      }
      if (path === '/api/v1/carddav/conflicts') {
        listReads += 1;
        if (listReads > 1 && reconciliationFails) return Response.json({ error: 'unavailable' }, { status: 503 });
        return Response.json({ conflicts: [listItem(41, { updated_at: listReads === 1 ? '2026-08-28T10:00:00Z' : '2026-08-28T13:00:00Z' })] });
      }
      detailReads += 1;
      if (detailReads > 1 && reconciliationFails) return Response.json({ error: 'unavailable' }, { status: 503 });
      return Response.json(detail(41, { updated_at: detailReads === 1 ? '2026-08-28T10:00:00Z' : '2026-08-28T13:00:00Z' }));
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    await controller.select(41);

    expect(await controller.resolve(41, 'keep_local')).toEqual({ kind: 'unknown' });
    expect(controller.resolutionUnknown).toBe(true);
    expect(controller.selectedDetail?.updated_at).toBe('2026-08-28T10:00:00Z');
    expect(controller.conflicts[0]?.updated_at).toBe('2026-08-28T10:00:00Z');
    expect(controller.isResolutionAllowed('keep_local')).toBe(false);
    expect(await controller.resolve(41, 'keep_local')).toEqual({ kind: 'ignored' });

    reconciliationFails = false;
    await controller.retrySelectedState();
    expect(controller.resolutionUnknown).toBe(false);
    expect(controller.selectedDetail?.updated_at).toBe('2026-08-28T13:00:00Z');
    expect(controller.conflicts[0]?.updated_at).toBe('2026-08-28T13:00:00Z');
    expect(controller.isResolutionAllowed('keep_local')).toBe(true);
    expect([posts, listReads, detailReads]).toEqual([1, 3, 3]);
    controller.destroy();
  });

  it('reports typed CardDAV unavailability as unknown after an ambiguous resolution', async () => {
    let listReads = 0;
    let detailReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') {
        return Response.json({ error: 'carddav_conflict_stale' }, { status: 409 });
      }
      if (path === '/api/v1/carddav/conflicts') {
        listReads += 1;
        if (listReads > 1) return Response.json({ error: 'carddav_unavailable' }, { status: 503 });
        return Response.json({ conflicts: [listItem(41)] });
      }
      detailReads += 1;
      if (detailReads > 1) return Response.json({ error: 'carddav_unavailable' }, { status: 503 });
      return Response.json(detail(41));
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    await controller.select(41);

    expect(await controller.resolve(41, 'keep_local')).toEqual({ kind: 'unknown' });
    expect(controller.unavailable).toBe(true);
    expect(controller.announcement).toBeNull();
    expect([listReads, detailReads]).toEqual([2, 2]);
    controller.destroy();
  });

  it.each([
    { name: 'the selected row', selectedDuringRetry: 41 },
    { name: 'a different row', selectedDuringRetry: 42 }
  ])('keeps retry reconciliation atomic when selecting $name', async ({ selectedDuringRetry }) => {
    const retryList = deferredResponse();
    const retryDetail = deferredResponse();
    let posts = 0;
    let listReads = 0;
    let detailReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') {
        posts += 1;
        return Response.json({ error: 'carddav_conflict_stale' }, { status: 409 });
      }
      if (path === '/api/v1/carddav/conflicts') {
        listReads += 1;
        if (listReads === 1) return Response.json({ conflicts: [listItem(41), listItem(42)] });
        if (listReads === 2) return Response.json({ error: 'unavailable' }, { status: 503 });
        return retryList.promise;
      }
      detailReads += 1;
      if (detailReads === 1) return Response.json(detail(41));
      if (detailReads === 2) return Response.json({ error: 'unavailable' }, { status: 503 });
      if (detailReads === 3) return retryDetail.promise;
      return Response.json(detail(Number(path.split('/').at(-1)), { updated_at: '2026-08-28T15:00:00Z' }));
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    await controller.select(41);
    expect(await controller.resolve(41, 'keep_local')).toEqual({ kind: 'unknown' });

    const retry = controller.retrySelectedState();
    await vi.waitFor(() => expect([listReads, detailReads]).toEqual([3, 3]));
    await controller.select(selectedDuringRetry);
    retryList.resolve(Response.json({
      conflicts: [
        listItem(41, { updated_at: '2026-08-28T14:00:00Z' }),
        listItem(42, { updated_at: '2026-08-28T14:00:00Z' })
      ]
    }));
    retryDetail.resolve(Response.json(detail(41, { updated_at: '2026-08-28T14:00:00Z' })));
    await retry;

    expect(controller.selectedID).toBe(41);
    expect(controller.selectedDetail?.updated_at).toBe('2026-08-28T14:00:00Z');
    expect(controller.conflicts[0]?.updated_at).toBe('2026-08-28T14:00:00Z');
    expect(controller.listLoading).toBe(false);
    expect(controller.detailLoading).toBe(false);
    expect(controller.resolutionUnknown).toBe(false);
    expect([posts, listReads, detailReads]).toEqual([1, 3, 3]);

    if (selectedDuringRetry === 42) {
      await controller.select(42);
      expect(controller.selectedID).toBe(42);
      expect(controller.selectedDetail?.id).toBe(42);
      expect(controller.selectedDetail?.updated_at).toBe('2026-08-28T15:00:00Z');
    }
    controller.destroy();
  });

  it('reports a refreshed resolved detail without asking for another choice after an ambiguous POST', async () => {
    let posts = 0;
    let listReads = 0;
    let detailReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') {
        posts += 1;
        return Response.json({ error: 'unavailable' }, { status: 503 });
      }
      if (path === '/api/v1/carddav/conflicts') {
        listReads += 1;
        return Response.json({ conflicts: listReads === 1 ? [listItem(41)] : [] });
      }
      detailReads += 1;
      if (detailReads === 1) return Response.json(detail(41));
      return Response.json(detail(41, {
        status: 'resolved',
        resolution: 'keep_remote',
        resolved_at: '2026-08-28T14:00:00Z',
        allowed_resolutions: [],
        updated_at: '2026-08-28T14:00:00Z'
      }));
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    await controller.select(41);

    expect(await controller.resolve(41, 'keep_local')).toEqual({ kind: 'reconciled' });

    expect([posts, listReads, detailReads]).toEqual([1, 2, 2]);
    expect(controller.selectedDetail?.status).toBe('resolved');
    expect(controller.selectedDetail?.resolution).toBe('keep_remote');
    expect(controller.selectedDetail?.allowed_resolutions).toEqual([]);
    expect(controller.isResolutionAllowed('keep_local')).toBe(false);
    expect(controller.resolutionUnknown).toBe(false);
    expect(controller.resolutionError).toBeNull();
    expect(controller.announcement).toBe('CardDAV conflict 41 state was refreshed and is already resolved.');
    controller.destroy();
  });

  it('consumes a keyed requested conflict once', async () => {
    let detailReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path.endsWith('/41')) {
        detailReads += 1;
        return Response.json(detail(41));
      }
      return Response.json({ conflicts: [listItem(41)] });
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));

    expect(await controller.openRequestedConflict({ conflictID: 41, key: 3 })).toBe(true);
    expect(await controller.openRequestedConflict({ conflictID: 41, key: 3 })).toBe(false);
    expect(await controller.openRequestedConflict({ conflictID: 41, key: 4 })).toBe(true);
    expect(detailReads).toBe(2);
    controller.destroy();
  });

  it('aborts owned reads and resolution transport and blocks every late write after destroy', async () => {
    const mutation = deferredResponse();
    const signals: AbortSignal[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      signals.push(request.signal);
      if (request.method === 'POST') return mutation.promise;
      if (path.endsWith('/41')) return Response.json(detail(41));
      return Response.json({ conflicts: [listItem(41)] });
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    await controller.select(41);
    const resolution = controller.resolve(41, 'keep_remote');

    controller.destroy();
    expect(signals.at(-1)?.aborted).toBe(true);
    mutation.resolve(Response.json({ id: 41, status: 'resolved', resolution: 'keep_remote' }));
    await resolution;

    expect(controller.conflicts.map(({ id }) => id)).toEqual([41]);
    expect(controller.selectedDetail?.status).toBe('unresolved');
    expect(controller.announcement).toBeNull();
    expect(controller.focusRequest).toBeUndefined();
  });

  it('aborts independent list and detail reads and retains both confirmed surfaces after destroy', async () => {
    const lateList = deferredResponse();
    const lateDetail = deferredResponse();
    const lateSignals: AbortSignal[] = [];
    let listReads = 0;
    let detailReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/carddav/conflicts') {
        listReads += 1;
        if (listReads === 1) return Response.json({ conflicts: [listItem(41)] });
        lateSignals.push(request.signal);
        return lateList.promise;
      }
      detailReads += 1;
      if (detailReads === 1) return Response.json(detail(41));
      lateSignals.push(request.signal);
      return lateDetail.promise;
    });
    const controller = new CardDAVConflictsController(createAPIClient(fetchFn));
    await controller.load();
    await controller.select(41);

    const listRetry = controller.retryList();
    const detailRetry = controller.retrySelectedState();
    await vi.waitFor(() => expect(lateSignals).toHaveLength(2));
    controller.destroy();

    expect(lateSignals.every((signal) => signal.aborted)).toBe(true);
    lateList.resolve(Response.json({ conflicts: [] }));
    lateDetail.resolve(Response.json(detail(41, { updated_at: '2026-08-29T10:00:00Z' })));
    await Promise.all([listRetry, detailRetry]);
    expect(controller.conflicts.map(({ id }) => id)).toEqual([41]);
    expect(controller.selectedDetail?.updated_at).toBe('2026-08-28T10:00:00Z');
  });
});
