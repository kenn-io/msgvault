import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import { CardDAVController, type CardDAVBookRoles } from './controller.svelte';

const idleStatus = {
  configured: true,
  available: true,
  credential_configured: true,
  enabled: false,
  scheduled: false,
  schedule: ''
};

function run(id: number, state: 'running' | 'succeeded' = 'running', updated = 0) {
  return {
    id,
    trigger: 'manual' as const,
    full: false,
    state,
    started_at: '2026-08-28T10:00:00Z',
    ...(state === 'running' ? {} : { finished_at: '2026-08-28T10:01:00Z' }),
    books: 2,
    created: 1,
    updated,
    removed: 0
  };
}

function book(id: number, overrides: Record<string, unknown> = {}) {
  return {
    id,
    name: `Synthetic book ${id}`,
    url: `https://forbidden-url-${id}.example.test/dav`,
    subscribed: false,
    lookup_source: false,
    write_target: false,
    needs_full_reconcile: false,
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

afterEach(() => {
  vi.useRealTimers();
  Object.defineProperty(document, 'hidden', { configurable: true, value: false });
});

describe('CardDAVController', () => {
  it('loads status, books, and page-zero history independently', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/carddav/status') {
        return Response.json({ error: 'carddav_unavailable', message: 'private marker' }, { status: 503 });
      }
      if (path === '/api/v1/carddav/books') return Response.json({ books: [book(3)] });
      if (path === '/api/v1/carddav/runs') return Response.json({ runs: [] });
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));

    await controller.load();

    expect(requests.map((request) => new URL(request.url).pathname).sort()).toEqual([
      '/api/v1/carddav/books',
      '/api/v1/carddav/runs',
      '/api/v1/carddav/status'
    ]);
    expect(new URL(requests.find((request) => new URL(request.url).pathname.endsWith('/runs'))!.url).searchParams.get('limit')).toBe('25');
    expect(controller.status).toBeUndefined();
    expect(controller.statusError).toBe('Unable to load CardDAV status.');
    expect(controller.books.map(({ id, name }) => ({ id, name }))).toEqual([{ id: 3, name: 'Synthetic book 3' }]);
    expect(controller.books[0]).not.toHaveProperty('url');
    expect(controller.runs).toEqual([]);
    expect(controller.runsError).toBeNull();
    controller.destroy();
  });

  it('blocks sync after a failed status refresh instead of using stale status', async () => {
    let statusReads = 0;
    let posts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/status')) {
        statusReads += 1;
        if (statusReads === 1) return Response.json(idleStatus);
        return Response.json({ error: 'unavailable' }, { status: 503 });
      }
      if (path.endsWith('/books')) return Response.json({ books: [] });
      if (path.endsWith('/runs')) return Response.json({ runs: [] });
      posts += 1;
      return Response.json({ run: run(1) });
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();
    expect(controller.canSync).toBe(true);

    await controller.retryStatus();

    expect(controller.statusError).toBe('Unable to load CardDAV status.');
    expect(controller.canSync).toBe(false);
    await controller.sync(false);
    expect(posts).toBe(0);
    controller.destroy();
  });

  it('blocks address-book role writes after a failed refresh instead of using stale books', async () => {
    let bookReads = 0;
    let patches = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/status')) return Response.json(idleStatus);
      if (path.endsWith('/books')) {
        if (request.method === 'PATCH') {
          patches += 1;
          return Response.json(book(1));
        }
        bookReads += 1;
        if (bookReads === 1) return Response.json({ books: [book(1)] });
        return Response.json({ error: 'unavailable' }, { status: 503 });
      }
      if (path.endsWith('/runs')) return Response.json({ runs: [] });
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();
    expect(controller.canSetBookRoles).toBe(true);

    await controller.load();

    expect(controller.booksError).toBe('Unable to load CardDAV address books.');
    expect(controller.canSetBookRoles).toBe(false);
    await controller.setBookRoles(1, { subscribed: true, lookup_source: false, write_target: false });
    expect(patches).toBe(0);
    controller.destroy();
  });

  it('uses progress-aware polling, caps backoff, pauses while hidden, and reconciles terminal state', async () => {
    vi.useFakeTimers();
    let statusReads = 0;
    let bookReads = 0;
    let runReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path === '/api/v1/carddav/books') {
        bookReads += 1;
        return Response.json({ books: [book(1)] });
      }
      if (path === '/api/v1/carddav/runs') {
        runReads += 1;
        return Response.json({ runs: [] });
      }
      statusReads += 1;
      if (statusReads <= 2) return Response.json({ ...idleStatus, active: run(8, 'running', 1) });
      if (statusReads === 3) return Response.json({ ...idleStatus, active: run(8, 'running', 2) });
      if (statusReads <= 8) return Response.json({ ...idleStatus, active: run(8, 'running', 2) });
      return Response.json({ ...idleStatus, latest: run(8, 'succeeded', 2) });
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();

    await vi.advanceTimersByTimeAsync(500); // unchanged -> next 1s
    await vi.advanceTimersByTimeAsync(1_000); // progress -> next 500ms
    await vi.advanceTimersByTimeAsync(500); // unchanged -> 1s
    await vi.advanceTimersByTimeAsync(1_000); // unchanged -> 2s
    await vi.advanceTimersByTimeAsync(2_000); // unchanged -> 4s
    expect(statusReads).toBe(6);
    await vi.advanceTimersByTimeAsync(4_000); // unchanged -> capped 8s
    expect(statusReads).toBe(7);
    await vi.advanceTimersByTimeAsync(7_999);
    expect(statusReads).toBe(7);
    await vi.advanceTimersByTimeAsync(1);
    expect(statusReads).toBe(8);

    Object.defineProperty(document, 'hidden', { configurable: true, value: true });
    document.dispatchEvent(new Event('visibilitychange'));
    await vi.advanceTimersByTimeAsync(8_000);
    expect(statusReads).toBe(8);

    Object.defineProperty(document, 'hidden', { configurable: true, value: false });
    document.dispatchEvent(new Event('visibilitychange'));
    await vi.advanceTimersByTimeAsync(499);
    expect(statusReads).toBe(8);
    await vi.advanceTimersByTimeAsync(1);
    expect(statusReads).toBe(9);
    await vi.waitFor(() => expect(bookReads).toBe(2));
    expect(runReads).toBe(2);
    expect(controller.status?.active).toBeUndefined();
    controller.destroy();
  });

  it('does not let an older idle poll overwrite a newer active status read', async () => {
    vi.useFakeTimers();
    const olderPoll = deferredResponse();
    const newerRunsRead = deferredResponse();
    let statusReads = 0;
    let bookReads = 0;
    let runReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path.endsWith('/books')) {
        bookReads += 1;
        return Response.json({ books: [] });
      }
      if (path.endsWith('/runs')) {
        runReads += 1;
        if (runReads === 2) return newerRunsRead.promise;
        return Response.json({ runs: [] });
      }
      statusReads += 1;
      if (statusReads === 1) return Response.json({ ...idleStatus, active: run(12, 'running', 1) });
      if (statusReads === 2) return olderPoll.promise;
      return Response.json({ ...idleStatus, active: run(12, 'running', 2) });
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();
    await vi.advanceTimersByTimeAsync(500);
    expect(statusReads).toBe(2);

    const refresh = controller.retrySyncState();
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.status?.active?.updated).toBe(2);
    olderPoll.resolve(Response.json({ ...idleStatus, latest: run(12, 'succeeded', 2) }));
    await vi.advanceTimersByTimeAsync(0);

    expect(controller.status?.active?.updated).toBe(2);
    expect(controller.status?.latest).toBeUndefined();
    expect([bookReads, runReads]).toEqual([1, 2]);
    newerRunsRead.resolve(Response.json({ runs: [] }));
    await refresh;
    controller.destroy();
  });

  it('does not let an older ordinary read reactivate status after a newer terminal poll', async () => {
    vi.useFakeTimers();
    const olderRead = deferredResponse();
    let statusReads = 0;
    let bookReads = 0;
    let runReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path.endsWith('/books')) {
        bookReads += 1;
        return Response.json({ books: [] });
      }
      if (path.endsWith('/runs')) {
        runReads += 1;
        return Response.json({ runs: [] });
      }
      statusReads += 1;
      if (statusReads === 1) return Response.json({ ...idleStatus, active: run(13, 'running', 1) });
      if (statusReads === 2) return olderRead.promise;
      return Response.json({ ...idleStatus, latest: run(13, 'succeeded', 2) });
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();

    const retry = controller.retryStatus();
    expect(statusReads).toBe(2);
    await vi.advanceTimersByTimeAsync(500);
    expect(statusReads).toBe(3);
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.status?.latest?.state).toBe('succeeded');
    expect(controller.statusLoading).toBe(false);
    expect([bookReads, runReads]).toEqual([2, 2]);

    olderRead.resolve(Response.json({ ...idleStatus, active: run(13, 'running', 1) }));
    await retry;
    expect(controller.status?.active).toBeUndefined();
    expect(controller.status?.latest?.state).toBe('succeeded');
    controller.destroy();
  });

  it('resumes polling after a newer failed ordinary read supersedes an in-flight poll', async () => {
    vi.useFakeTimers();
    const olderPoll = deferredResponse();
    const newerRead = deferredResponse();
    let statusReads = 0;
    let bookReads = 0;
    let runReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path.endsWith('/books')) {
        bookReads += 1;
        return Response.json({ books: [] });
      }
      if (path.endsWith('/runs')) {
        runReads += 1;
        return Response.json({ runs: [] });
      }
      statusReads += 1;
      if (statusReads === 1) return Response.json({ ...idleStatus, active: run(14, 'running', 1) });
      if (statusReads === 2) return olderPoll.promise;
      if (statusReads === 3) return newerRead.promise;
      return Response.json({ ...idleStatus, latest: run(14, 'succeeded', 2) });
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();
    await vi.advanceTimersByTimeAsync(500);
    expect(statusReads).toBe(2);

    const retry = controller.retryStatus();
    expect(statusReads).toBe(3);
    newerRead.resolve(Response.json({ error: 'carddav_unavailable' }, { status: 503 }));
    await retry;
    expect(controller.statusError).toBe('Unable to load CardDAV status.');
    expect(controller.status?.active?.updated).toBe(1);

    olderPoll.resolve(Response.json({ ...idleStatus, active: run(14, 'running', 2) }));
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.status?.active?.updated).toBe(1);
    await vi.advanceTimersByTimeAsync(499);
    expect(statusReads).toBe(3);
    await vi.advanceTimersByTimeAsync(1);

    expect(statusReads).toBe(4);
    expect(controller.status?.active).toBeUndefined();
    expect(controller.status?.latest?.state).toBe('succeeded');
    expect(controller.statusError).toBeNull();
    expect([bookReads, runReads]).toEqual([2, 2]);
    controller.destroy();
  });

  it('lets a newer successful poll satisfy a superseded retrySyncState status read', async () => {
    vi.useFakeTimers();
    const olderRead = deferredResponse();
    const newerPoll = deferredResponse();
    let statusReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path.endsWith('/books')) return Response.json({ books: [] });
      if (path.endsWith('/runs')) return Response.json({ runs: [] });
      statusReads += 1;
      if (statusReads === 1) return Response.json({ ...idleStatus, active: run(15, 'running', 1) });
      if (statusReads === 2) return olderRead.promise;
      return newerPoll.promise;
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();
    controller.syncUnknown = true;

    const retry = controller.retrySyncState();
    expect(statusReads).toBe(2);
    await vi.advanceTimersByTimeAsync(500);
    expect(statusReads).toBe(3);

    olderRead.resolve(Response.json({ error: 'carddav_unavailable' }, { status: 503 }));
    await retry;
    expect(controller.syncUnknown).toBe(true);

    newerPoll.resolve(Response.json({ ...idleStatus, active: run(15, 'running', 2) }));
    await vi.advanceTimersByTimeAsync(0);

    expect(controller.syncPending).toBe(false);
    expect(controller.syncUnknown).toBe(false);
    expect(controller.status?.active?.updated).toBe(2);
    controller.destroy();
  });

  it('aborts a stale poll and ignores its late completion after destroy', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const deferred = deferredResponse();
    const signals: AbortSignal[] = [];
    let statusReads = 0;
    const fetchFn = vi.fn<typeof fetch>((input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/carddav/books') return Promise.resolve(Response.json({ books: [] }));
      if (path === '/api/v1/carddav/runs') return Promise.resolve(Response.json({ runs: [] }));
      statusReads += 1;
      signals.push(request.signal);
      if (statusReads === 1) return Promise.resolve(Response.json({ ...idleStatus, active: run(9) }));
      return deferred.promise;
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();
    await vi.advanceTimersByTimeAsync(500);
    await vi.waitFor(() => expect(signals).toHaveLength(2));

    controller.destroy();
    expect(signals[1]!.aborted).toBe(true);
    deferred.resolve(Response.json({ ...idleStatus, active: run(99) }));
    await Promise.resolve();
    expect(controller.status?.active?.id).toBe(9);
  });

  it('sends exact manual and full sync bodies, suppresses duplicates, and reconciles ambiguous results with GET only', async () => {
    const sync = deferredResponse();
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>((input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/carddav/status') return Promise.resolve(Response.json(idleStatus));
      if (path === '/api/v1/carddav/books') return Promise.resolve(Response.json({ books: [] }));
      if (path === '/api/v1/carddav/runs') return Promise.resolve(Response.json({ runs: [] }));
      if (path === '/api/v1/carddav/sync') return sync.promise;
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();

    const first = controller.sync(false);
    await vi.waitFor(() => expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1));
    await controller.sync(true);
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
    const request = requests.find((candidate) => candidate.method === 'POST')!;
    await expect(request.clone().json()).resolves.toEqual({ full: false });
    sync.resolve(Response.json({ error: 'sync_failed', message: 'unsafe detail' }, { status: 503 }));
    await first;

    expect(requests.filter((candidate) => candidate.method === 'POST')).toHaveLength(1);
    expect(requests.filter((candidate) => candidate.method === 'GET' && new URL(candidate.url).pathname.endsWith('/status'))).toHaveLength(2);
    expect(requests.filter((candidate) => candidate.method === 'GET' && new URL(candidate.url).pathname.endsWith('/runs'))).toHaveLength(2);
    expect(controller.syncError).toBe('Unable to complete CardDAV sync. Current state was refreshed.');

    const fullFetch = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (request.method === 'POST') {
        await expect(request.clone().json()).resolves.toEqual({ full: true });
        return Response.json({ books: 1, created: 0, updated: 1, removed: 0 });
      }
      const path = new URL(request.url).pathname;
      if (path.endsWith('/status')) return Response.json(idleStatus);
      if (path.endsWith('/books')) return Response.json({ books: [] });
      return Response.json({ runs: [] });
    });
    const fullController = new CardDAVController(createAPIClient(fullFetch));
    await fullController.load();
    await fullController.sync(true);
    fullController.destroy();
    controller.destroy();
  });

  it('keeps sync blocked through failed ambiguity reconciliation and retries only the reads', async () => {
    const statusReconcile = deferredResponse();
    const runsReconcile = deferredResponse();
    let statusReads = 0;
    let runsReads = 0;
    let posts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/books')) return Response.json({ books: [] });
      if (path.endsWith('/status')) {
        statusReads += 1;
        if (statusReads === 1 || statusReads === 3) return Response.json(idleStatus);
        return statusReconcile.promise;
      }
      if (path.endsWith('/runs')) {
        runsReads += 1;
        if (runsReads === 1 || runsReads === 3) return Response.json({ runs: [] });
        return runsReconcile.promise;
      }
      posts += 1;
      return Response.json({ error: 'sync_failed', message: 'uncertain' }, { status: posts === 1 ? 503 : 400 });
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();

    const action = controller.sync(false);
    await vi.waitFor(() => expect([statusReads, runsReads]).toEqual([2, 2]));
    await controller.sync(false);
    expect(posts).toBe(1);
    statusReconcile.resolve(Response.json({ error: 'unavailable', message: 'unsafe' }, { status: 503 }));
    runsReconcile.resolve(Response.json({ error: 'unavailable', message: 'unsafe' }, { status: 503 }));
    await action;

    expect(controller.syncUnknown).toBe(true);
    expect(controller.canSync).toBe(false);
    await controller.retrySyncState();
    expect([statusReads, runsReads, posts]).toEqual([3, 3, 1]);
    expect(controller.syncUnknown).toBe(false);
    expect(controller.canSync).toBe(true);
    controller.destroy();
  });

  it('sends the complete normalized role tuple and reloads every book after success', async () => {
    const requests: Request[] = [];
    let booksRead = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/status')) return Response.json(idleStatus);
      if (path.endsWith('/runs')) return Response.json({ runs: [] });
      if (request.method === 'GET' && path.endsWith('/books')) {
        booksRead += 1;
        return Response.json({ books: [book(1, { write_target: booksRead > 1, subscribed: booksRead > 1 }), book(2)] });
      }
      if (request.method === 'PATCH') return Response.json(book(1, { write_target: true, subscribed: true }));
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();

    await controller.setBookRoles(1, { subscribed: false, lookup_source: true, write_target: true });

    const patch = requests.find((request) => request.method === 'PATCH')!;
    expect(new URL(patch.url).pathname).toBe('/api/v1/carddav/books/1');
    await expect(patch.clone().json()).resolves.toEqual({ subscribed: true, lookup_source: true, write_target: true });
    expect(booksRead).toBe(2);
    expect(controller.books.find((candidate) => candidate.id === 1)?.write_target).toBe(true);
    expect(controller.bookDraft(1)).toBeUndefined();
    controller.destroy();
  });

  it('blocks role mutations until status confirms a ready CardDAV runtime', async () => {
    let statusReads = 0;
    let patches = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/status')) {
        statusReads += 1;
        if (statusReads === 1) return Response.json({ error: 'unavailable', message: 'unsafe' }, { status: 503 });
        return Response.json(idleStatus);
      }
      if (request.method === 'GET' && path.endsWith('/books')) return Response.json({ books: [book(1)] });
      if (path.endsWith('/runs')) return Response.json({ runs: [] });
      patches += 1;
      return Response.json(book(1, { subscribed: true }));
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();

    expect(controller.canSetBookRoles).toBe(false);
    await controller.setBookRoles(1, { subscribed: true, lookup_source: false, write_target: false });
    expect(patches).toBe(0);

    await controller.retryStatus();
    expect(controller.canSetBookRoles).toBe(true);
    await controller.setBookRoles(1, { subscribed: true, lookup_source: false, write_target: false });
    expect(patches).toBe(1);
    controller.destroy();
  });

  it('retains intended roles across stale reconciliation and makes failed reconciliation GET-only', async () => {
    let booksRead = 0;
    let patches = 0;
    const requests: Request[] = [];
    const intended: CardDAVBookRoles = { subscribed: true, lookup_source: true, write_target: false };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/status')) return Response.json(idleStatus);
      if (path.endsWith('/runs')) return Response.json({ runs: [] });
      if (request.method === 'GET' && path.endsWith('/books')) {
        booksRead += 1;
        if (booksRead === 3) return Response.json({ error: 'unavailable', message: 'unsafe marker' }, { status: 503 });
        return Response.json({ books: [book(1)] });
      }
      patches += 1;
      return Response.json({ error: 'carddav_conflict', message: 'stale' }, { status: patches === 1 ? 409 : 503 });
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();

    await controller.setBookRoles(1, intended);
    expect(patches).toBe(1);
    expect(controller.bookDraft(1)).toEqual(intended);
    expect(controller.booksUnknown).toBe(false);

    await controller.setBookRoles(1, intended);
    expect(patches).toBe(2);
    expect(controller.booksUnknown).toBe(true);
    expect(controller.bookDraft(1)).toEqual(intended);
    await controller.setBookRoles(1, intended);
    expect(patches).toBe(2);

    await controller.retryBooks();
    expect(booksRead).toBe(4);
    expect(controller.booksUnknown).toBe(false);
    expect(patches).toBe(2);
    controller.destroy();
  });

  it('keeps role writes blocked until stale reconciliation finishes', async () => {
    const reconcile = deferredResponse();
    let booksRead = 0;
    let patches = 0;
    const intended: CardDAVBookRoles = { subscribed: true, lookup_source: false, write_target: false };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/status')) return Response.json(idleStatus);
      if (path.endsWith('/runs')) return Response.json({ runs: [] });
      if (request.method === 'GET') {
        booksRead += 1;
        if (booksRead === 1) return Response.json({ books: [book(1)] });
        return reconcile.promise;
      }
      patches += 1;
      return Response.json({ error: 'carddav_conflict', message: 'stale' }, { status: patches === 1 ? 409 : 400 });
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();

    const action = controller.setBookRoles(1, intended);
    await vi.waitFor(() => expect(booksRead).toBe(2));
    await controller.setBookRoles(1, intended);
    expect(patches).toBe(1);
    reconcile.resolve(Response.json({ books: [book(1)] }));
    await action;
    controller.destroy();
  });

  it('retains history and retries the same cursor, replaces the head, and deduplicates appended runs', async () => {
    const cursors: Array<string | null> = [];
    let cursorFailure = true;
    let head = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (url.pathname.endsWith('/status')) return Response.json(idleStatus);
      if (url.pathname.endsWith('/books')) return Response.json({ books: [] });
      const cursor = url.searchParams.get('before_id');
      cursors.push(cursor);
      if (cursor === '8' && cursorFailure) {
        cursorFailure = false;
        return Response.json({ error: 'unavailable', message: 'unsafe marker' }, { status: 503 });
      }
      if (cursor === '8') return Response.json({ runs: [run(9, 'succeeded'), run(8, 'succeeded'), run(7, 'succeeded')] });
      head += 1;
      return head === 1
        ? Response.json({ runs: [run(10, 'succeeded'), run(9, 'succeeded')], next_before_id: 8 })
        : Response.json({ runs: [run(12, 'succeeded')], next_before_id: 11 });
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();
    expect(controller.runs.map(({ id }) => id)).toEqual([10, 9]);

    await controller.loadMoreRuns();
    expect(controller.runs.map(({ id }) => id)).toEqual([10, 9]);
    expect(controller.runsPageError).toBe('Unable to load more CardDAV history.');
    await controller.retryRuns();
    expect(cursors).toEqual([null, '8', '8']);
    expect(controller.runs.map(({ id }) => id)).toEqual([10, 9, 8, 7]);

    await controller.refreshRuns();
    expect(controller.runs.map(({ id }) => id)).toEqual([12]);
    expect(controller.nextBeforeID).toBe(11);
    controller.destroy();
  });
});
