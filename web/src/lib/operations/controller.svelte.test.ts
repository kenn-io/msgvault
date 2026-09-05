import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import type { Status as VisualStatus, SyncResult } from '../api/generated/models';
import { OperationsController } from './controller.svelte';
import type {
  OperationAction,
  OperationKind,
  OperationLane,
  OperationRunDetail,
  OperationRunsResponse,
  OperationRunSummary,
  OperationStatusResponse,
  OperationsURLState,
} from './models';

const RUN_ONE = `op2.${'a'.repeat(32)}.syntheticRunOne`;
const RUN_TWO = `op2.${'b'.repeat(32)}.syntheticRunTwo`;

const KIND_LANES: ReadonlyArray<readonly [OperationKind, OperationLane]> = [
  ['source_sync', 'messages'],
  ['message_embedding', 'messages'],
  ['person_sweep', 'person_facts'],
  ['person_embedding', 'person_facts'],
  ['person_enrichment', 'person_facts'],
  ['carddav_sync', 'contacts'],
  ['document_extraction', 'documents'],
  ['document_embedding', 'documents'],
  ['visual_embedding', 'visual_attachments']
];

function operationState(overrides: Partial<OperationsURLState> = {}): OperationsURLState {
  return {
    operationLane: '',
    operationKind: '',
    operationState: '',
    operationStartedFrom: '',
    operationStartedBefore: '',
    operationRunID: null,
    operationStatus: '',
    ...overrides
  };
}

function run(
  id: string,
  kind: OperationKind = 'source_sync',
  lane: OperationLane = 'messages',
  state: OperationRunSummary['state'] = 'succeeded'
): OperationRunSummary {
  return {
    id,
    kind,
    lane,
    trigger: 'manual',
    state,
    started_at: '2026-08-30T10:00:00Z',
    ...(state === 'queued' || state === 'running' ? {} : { finished_at: '2026-08-30T10:01:00Z' }),
    counters: [{ name: 'processed', unit: 'messages', value: 3 }]
  };
}

function detail(
  id: string,
  actions: OperationAction[] = []
): OperationRunDetail {
  return {
    ...run(id),
    related_status: 'listSourceStatus',
    supported_actions: actions
  };
}

function statusResponse(overrides: Partial<Record<OperationKind, {
  configured?: boolean;
  history_availability?: 'available' | 'unavailable';
  supported_actions?: OperationAction[];
}>> = {}): OperationStatusResponse {
  return {
    lanes: KIND_LANES.map(([kind, lane]) => ({
      kind,
      lane,
      configured: overrides[kind]?.configured ?? true,
      history_availability: overrides[kind]?.history_availability ?? 'available',
      supported_actions: overrides[kind]?.supported_actions ?? [],
      latest: run(`${RUN_ONE}-${kind}`, kind, lane),
      latest_successful: run(`${RUN_TWO}-${kind}`, kind, lane),
      related_status: kind === 'carddav_sync' ? 'getCardDAVStatus'
        : kind === 'document_extraction' ? 'getDocumentIndexStatus'
          : kind === 'document_embedding' ? 'getDocumentVectorStatus'
            : kind === 'visual_embedding' ? 'getVisualAttachmentStatus'
              : kind === 'source_sync' ? 'listSourceStatus'
                : undefined,
      ...(overrides[kind]?.history_availability === 'unavailable'
        ? { unavailable_code: 'synthetic_unavailable' }
        : {})
    }))
  };
}

function runsResponse(
  runs: OperationRunSummary[],
  overrides: Partial<Pick<OperationRunsResponse, 'membership_revision' | 'next_cursor' | 'unavailable_kinds'>> = {}
): OperationRunsResponse {
  return {
    runs,
    membership_revision: overrides.membership_revision ?? 7,
    unavailable_kinds: overrides.unavailable_kinds ?? [],
    ...(overrides.next_cursor ? { next_cursor: overrides.next_cursor } : {})
  };
}

function syncResult(): SyncResult {
  return { books: 1, created: 1, updated: 0, removed: 0 };
}

function visualStatus(): VisualStatus {
  return {
    active_leases: 0,
    converged: 1,
    convergence_ratio: 1,
    convergence_total: 1,
    current: 1,
    duplicate_cost: {
      at_least_once: false,
      detail: 'Synthetic test policy.',
      provider_idempotent: true
    },
    eligible: 1,
    formats: [],
    generation: {
      consented: true,
      dimension: 3,
      fingerprint: 'synthetic-generation',
      id: 1,
      model: 'synthetic-model',
      source_fence: 1,
      state: 'active'
    },
    journal_cursor: 1,
    journal_high_water: 1,
    journal_lag: 0,
    reconciliation_complete: true,
    retryable: 0,
    stale: 0,
    terminal: 0,
    tombstoned: 0,
    unavailable: 0,
    unknown_role: 0,
    usage: { billed_units: 0, input_bytes: 0, requests: 0, usage_available: false }
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

describe('OperationsController', () => {
  it('loads status and filtered page one into one readonly initial snapshot', async () => {
    const status = deferredResponse();
    const history = deferredResponse();
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      return new URL(request.url).pathname.endsWith('/status') ? status.promise : history.promise;
    });
    const controller = new OperationsController(createAPIClient(fetchFn));
    const state = operationState({
      operationLane: 'documents',
      operationKind: 'document_extraction',
      operationState: 'partial',
      operationStartedFrom: '2026-08-01T00:00:00Z',
      operationStartedBefore: '2026-09-01T00:00:00Z'
    });

    const loading = controller.applyURLState(state, 'archive-a');
    expect(controller.snapshot.initialLoading).toBe(true);
    expect((controller.snapshot as { statusReadable?: boolean }).statusReadable).toBe(false);
    expect((controller.snapshot as { historyReadable?: boolean }).historyReadable).toBe(false);
    expect(controller.snapshot.rows).toEqual([]);
    status.resolve(Response.json(statusResponse()));
    history.resolve(Response.json(runsResponse([run(RUN_ONE, 'document_extraction', 'documents', 'partial')])));
    await loading;

    const historyRequest = requests.find((request) => new URL(request.url).pathname.endsWith('/runs'))!;
    expect(Object.fromEntries(new URL(historyRequest.url).searchParams)).toEqual({
      lane: 'documents',
      kind: 'document_extraction',
      state: 'partial',
      started_from: '2026-08-01T00:00:00Z',
      started_before: '2026-09-01T00:00:00Z',
      limit: '25'
    });
    expect(controller.snapshot.initialLoading).toBe(false);
    expect((controller.snapshot as { statusReadable?: boolean }).statusReadable).toBe(true);
    expect((controller.snapshot as { historyReadable?: boolean }).historyReadable).toBe(true);
    expect(controller.snapshot.statusLanes.map((item) => item.lane)).toEqual([
      'messages', 'person_facts', 'contacts', 'documents', 'visual_attachments'
    ]);
    expect(controller.snapshot.rows.map((item) => item.id)).toEqual([RUN_ONE]);
    expect(Object.isFrozen(controller.snapshot)).toBe(true);
    controller.destroy();
  });

  it('keeps current cards and rows visible during a background refresh', async () => {
    let refreshStatus: ReturnType<typeof deferredResponse> | undefined;
    let refreshRuns: ReturnType<typeof deferredResponse> | undefined;
    let reads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      reads += 1;
      if (reads <= 2) {
        return Response.json(path.endsWith('/status')
          ? statusResponse()
          : runsResponse([run(RUN_ONE)]));
      }
      if (path.endsWith('/status')) {
        refreshStatus = deferredResponse();
        return refreshStatus.promise;
      }
      refreshRuns = deferredResponse();
      return refreshRuns.promise;
    });
    const controller = new OperationsController(createAPIClient(fetchFn));
    await controller.applyURLState(operationState());

    const refreshing = controller.refresh();
    await vi.waitFor(() => expect(refreshStatus && refreshRuns).toBeTruthy());
    expect(controller.snapshot.backgroundLoading).toBe(true);
    expect(controller.snapshot.rows.map((item) => item.id)).toEqual([RUN_ONE]);
    expect(controller.snapshot.statusLanes).toHaveLength(5);
    refreshStatus!.resolve(Response.json(statusResponse()));
    refreshRuns!.resolve(Response.json(runsResponse([run(RUN_TWO)])));
    await refreshing;

    expect(controller.snapshot.backgroundLoading).toBe(false);
    expect(controller.snapshot.rows.map((item) => item.id)).toEqual([RUN_TWO]);
    controller.destroy();
  });

  it('aborts and discards stale filter and archive responses', async () => {
    const deferred = Array.from({ length: 4 }, deferredResponse);
    const requests: Request[] = [];
    let requestIndex = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      requests.push(requestOf(input));
      return deferred[requestIndex++]!.promise;
    });
    const controller = new OperationsController(createAPIClient(fetchFn));
    const oldLoad = controller.applyURLState(operationState({ operationLane: 'messages' }), 'archive-a');
    await vi.waitFor(() => expect(requests).toHaveLength(2));
    const newLoad = controller.applyURLState(operationState({ operationLane: 'contacts' }), 'archive-b');
    await vi.waitFor(() => expect(requests).toHaveLength(4));

    expect(requests[0]!.signal.aborted).toBe(true);
    expect(requests[1]!.signal.aborted).toBe(true);
    deferred[2]!.resolve(Response.json(statusResponse()));
    deferred[3]!.resolve(Response.json(runsResponse([run(RUN_TWO, 'carddav_sync', 'contacts')])));
    await newLoad;
    deferred[0]!.resolve(Response.json(statusResponse()));
    deferred[1]!.resolve(Response.json(runsResponse([run(RUN_ONE)])));
    await oldLoad;

    expect(controller.snapshot.rows.map((item) => item.id)).toEqual([RUN_TWO]);
    expect(controller.snapshot.nextCursor).toBeNull();
    controller.destroy();
  });

  it('clears selected detail and pagination through the URL owner on a committed archive change', async () => {
    const commits = vi.fn();
    const replacementHistory = deferredResponse();
    let historyReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path.endsWith('/status')) return Response.json(statusResponse());
      if (path.endsWith('/runs')) {
        historyReads += 1;
        if (historyReads === 2) return replacementHistory.promise;
        return Response.json(runsResponse([run(RUN_ONE)], { next_cursor: 'archive-bound-cursor' }));
      }
      return Response.json(detail(RUN_ONE));
    });
    const controller = new OperationsController(createAPIClient(fetchFn), commits);
    const state = operationState({ operationRunID: RUN_ONE });
    await controller.applyURLState(state, 'archive-a');
    expect(controller.snapshot.detail?.id).toBe(RUN_ONE);
    expect(controller.snapshot.nextCursor).toBe('archive-bound-cursor');

    const replacing = controller.applyURLState(state, 'archive-b');

    expect(commits).toHaveBeenCalledWith({ operationRunID: null });
    expect(controller.snapshot.detail).toBeNull();
    expect(controller.snapshot.nextCursor).toBeNull();
    replacementHistory.resolve(Response.json(runsResponse([run(RUN_TWO)])));
    await replacing;
    controller.destroy();
  });

  it('continues with the opaque cursor and rejects a changed membership revision', async () => {
    const requests: Request[] = [];
    let page = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/status')) return Response.json(statusResponse());
      page += 1;
      return Response.json(page === 1
        ? runsResponse([run(RUN_ONE)], { next_cursor: 'opaque.next.cursor', membership_revision: 7 })
        : runsResponse([run(RUN_TWO)], { membership_revision: 8 }));
    });
    const controller = new OperationsController(createAPIClient(fetchFn));
    await controller.applyURLState(operationState({ operationKind: 'source_sync' }));

    await controller.loadMore();

    const secondPage = requests.filter((request) => new URL(request.url).pathname.endsWith('/runs'))[1]!;
    expect(new URL(secondPage.url).searchParams.get('cursor')).toBe('opaque.next.cursor');
    expect(new URL(secondPage.url).searchParams.get('limit')).toBe('25');
    expect(controller.snapshot.rows.map((item) => item.id)).toEqual([RUN_ONE]);
    expect(controller.snapshot.restartRequired).toBe(true);
    expect(controller.snapshot.conflict).toBe('Operation history changed. Restart from the first page.');
    controller.destroy();
  });

  it('releases aborted pagination during refresh without letting it clear a newer page owner', async () => {
    const stalePage = deferredResponse();
    const runThree = `op2.${'c'.repeat(32)}.syntheticRunThree`;
    const runRequests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (url.pathname.endsWith('/status')) return Response.json(statusResponse());
      runRequests.push(request);
      const cursor = url.searchParams.get('cursor');
      if (cursor === 'first-page-cursor') return stalePage.promise;
      if (cursor === 'replacement-cursor') {
        return Response.json(runsResponse([run(runThree)], { membership_revision: 8 }));
      }
      return runRequests.length === 1
        ? Response.json(runsResponse([run(RUN_ONE)], {
          next_cursor: 'first-page-cursor', membership_revision: 7
        }))
        : Response.json(runsResponse([run(RUN_TWO)], {
          next_cursor: 'replacement-cursor', membership_revision: 8
        }));
    });
    const controller = new OperationsController(createAPIClient(fetchFn));
    await controller.applyURLState(operationState());

    const oldPaging = controller.loadMore();
    await vi.waitFor(() => expect(runRequests).toHaveLength(2));
    const staleRequest = runRequests[1]!;
    await controller.refresh();

    expect(staleRequest.signal.aborted).toBe(true);
    expect(controller.snapshot.paging).toBe(false);
    expect(controller.snapshot.nextCursor).toBe('replacement-cursor');
    await controller.loadMore();
    expect(runRequests).toHaveLength(4);
    expect(controller.snapshot.rows.map((item) => item.id)).toEqual([RUN_TWO, runThree]);

    stalePage.resolve(Response.json(runsResponse([run(RUN_ONE)], { membership_revision: 7 })));
    await oldPaging;
    expect(controller.snapshot.rows.map((item) => item.id)).toEqual([RUN_TWO, runThree]);
    expect(controller.snapshot.paging).toBe(false);
    controller.destroy();
  });

  it('keeps selected detail identity when its same-timestamp row leaves the refreshed page', async () => {
    const commits = vi.fn();
    const peerBefore = `${RUN_ONE}-peer`;
    const peerAfter = `${RUN_TWO}-peer`;
    let historyReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (path.endsWith('/status')) return Response.json(statusResponse());
      if (path.endsWith('/runs')) {
        historyReads += 1;
        return Response.json(historyReads === 1
          ? runsResponse([run(RUN_ONE), run(peerBefore)])
          : runsResponse([run(peerAfter)]));
      }
      return Response.json(detail(RUN_ONE));
    });
    const controller = new OperationsController(createAPIClient(fetchFn), commits);
    await controller.applyURLState(operationState());
    await controller.applyURLState(operationState({ operationRunID: RUN_ONE }));
    expect(controller.snapshot.detail?.id).toBe(RUN_ONE);
    commits.mockClear();

    await controller.refresh();

    expect(controller.snapshot.rows.map((item) => item.id)).toEqual([peerAfter]);
    expect(controller.snapshot.detail?.id).toBe(RUN_ONE);
    expect(commits).not.toHaveBeenCalled();
    controller.destroy();
  });

  it('restores detail directly and discards a stale detail response after selection changes', async () => {
    const firstDetail = deferredResponse();
    const detailRequests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/status')) return Response.json(statusResponse());
      if (path.endsWith('/runs')) return Response.json(runsResponse([]));
      detailRequests.push(request);
      if (path.endsWith(encodeURIComponent(RUN_ONE))) return firstDetail.promise;
      return Response.json(detail(RUN_TWO));
    });
    const controller = new OperationsController(createAPIClient(fetchFn));
    const initial = controller.applyURLState(operationState({ operationRunID: RUN_ONE }));
    await vi.waitFor(() => expect(detailRequests).toHaveLength(1));
    const replacement = controller.applyURLState(operationState({ operationRunID: RUN_TWO }));
    await replacement;
    firstDetail.resolve(Response.json(detail(RUN_ONE)));
    await initial;

    expect(detailRequests[0]!.signal.aborted).toBe(true);
    expect(controller.snapshot.detail?.id).toBe(RUN_TWO);
    expect(controller.snapshot.rows).toEqual([]);
    controller.destroy();
  });

  it('clears completed detail and action failures when filters establish a new context', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') {
        return Response.json({ error: 'operation_conflict', message: 'synthetic conflict' }, { status: 409 });
      }
      if (path.endsWith('/status')) {
        return Response.json(statusResponse({
          visual_embedding: { supported_actions: ['visual_build'] }
        }));
      }
      if (path === '/api/v1/operations/runs') return Response.json(runsResponse([]));
      return Response.json({ error: 'operation_not_found', message: 'synthetic missing detail' }, { status: 404 });
    });
    const controller = new OperationsController(createAPIClient(fetchFn));
    await controller.applyURLState(operationState({ operationRunID: RUN_ONE }), 'archive-a');
    await controller.runAction('visual_build');
    await controller.runAction('carddav_sync');
    expect(controller.snapshot.detailError).not.toBeNull();
    expect(controller.snapshot.actionConflict).not.toBeNull();
    expect(controller.snapshot.actionError).not.toBeNull();

    await controller.applyURLState(operationState({ operationLane: 'documents' }), 'archive-a');

    expect(controller.snapshot).toMatchObject({
      detailError: null,
      detailLoading: false,
      actionPending: null,
      actionConflict: null,
      actionError: null
    });
    controller.destroy();
  });

  it('clears in-flight detail and action context on archive change and discards late failures', async () => {
    const staleDetail = deferredResponse();
    const staleAction = deferredResponse();
    const commits = vi.fn();
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') return staleAction.promise;
      if (path.endsWith('/status')) {
        return Response.json(statusResponse({
          visual_embedding: { supported_actions: ['visual_build'] }
        }));
      }
      if (path === '/api/v1/operations/runs') return Response.json(runsResponse([]));
      return staleDetail.promise;
    });
    const controller = new OperationsController(createAPIClient(fetchFn), commits);
    await controller.applyURLState(operationState(), 'archive-a');
    const detailLoad = controller.applyURLState(operationState({ operationRunID: RUN_ONE }), 'archive-a');
    const actionLoad = controller.runAction('visual_build');
    await vi.waitFor(() => expect(requests.some((request) => request.method === 'POST')).toBe(true));

    await controller.applyURLState(operationState({ operationRunID: RUN_ONE }), 'archive-b');

    expect(commits).toHaveBeenCalledWith({ operationRunID: null });
    expect(controller.snapshot).toMatchObject({
      detailError: null,
      detailLoading: false,
      actionPending: null,
      actionConflict: null,
      actionError: null
    });
    staleDetail.resolve(Response.json(
      { error: 'operation_not_found', message: 'late synthetic detail failure' },
      { status: 404 }
    ));
    staleAction.resolve(Response.json(
      { error: 'operation_conflict', message: 'late synthetic action conflict' },
      { status: 409 }
    ));
    const [, actionOutcome] = await Promise.all([detailLoad, actionLoad]);
    expect(actionOutcome).toBe('discarded');
    expect(controller.snapshot).toMatchObject({
      detailError: null,
      detailLoading: false,
      actionPending: null,
      actionConflict: null,
      actionError: null
    });
    controller.destroy();
  });

  it('keeps status cards while exposing dynamically unavailable history kinds', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      return Response.json(path.endsWith('/status')
        ? statusResponse({ document_embedding: { history_availability: 'unavailable' } })
        : runsResponse([run(RUN_ONE)], {
          unavailable_kinds: [{
            kind: 'document_embedding',
            lane: 'documents',
            unavailable_code: 'synthetic_schema_unavailable'
          }]
        }));
    });
    const controller = new OperationsController(createAPIClient(fetchFn));

    await controller.applyURLState(operationState());

    expect(controller.snapshot.statusLanes.find((item) => item.lane === 'documents')?.kinds).toHaveLength(2);
    expect(controller.snapshot.rows).toHaveLength(1);
    expect(controller.snapshot.unavailableKinds).toEqual([{
      kind: 'document_embedding',
      lane: 'documents',
      unavailable_code: 'synthetic_schema_unavailable'
    }]);
    controller.destroy();
  });

  it('surfaces a typed paging 409 and restarts from page one on request', async () => {
    let runReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/status')) return Response.json(statusResponse());
      runReads += 1;
      if (runReads === 1) return Response.json(runsResponse([run(RUN_ONE)], { next_cursor: 'opaque-restart' }));
      if (runReads === 2) {
        return Response.json({ error: 'operation_history_conflict', message: 'private server detail' }, { status: 409 });
      }
      return Response.json(runsResponse([run(RUN_TWO)], { membership_revision: 9 }));
    });
    const controller = new OperationsController(createAPIClient(fetchFn));
    await controller.applyURLState(operationState());
    await controller.loadMore();

    expect(controller.snapshot.restartRequired).toBe(true);
    expect(controller.snapshot.conflict).not.toContain('private server detail');
    await controller.restart();

    expect(controller.snapshot.restartRequired).toBe(false);
    expect(controller.snapshot.rows.map((item) => item.id)).toEqual([RUN_TWO]);
    controller.destroy();
  });

  it.each([
    ['carddav_sync', '/api/v1/carddav/sync', { full: false }],
    ['visual_build', '/api/v1/multimodal/build', { consent: true }],
    ['visual_resume', '/api/v1/multimodal/run', undefined]
  ] as const)('invokes advertised %s through its generated route and refreshes', async (action, actionPath, body) => {
    const requests: Request[] = [];
    let reads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') {
        return Response.json(action === 'carddav_sync' ? syncResult() : visualStatus());
      }
      reads += 1;
      return Response.json(path.endsWith('/status')
        ? statusResponse({ [action === 'carddav_sync' ? 'carddav_sync' : 'visual_embedding']: { supported_actions: [action] } })
        : runsResponse([]));
    });
    const controller = new OperationsController(createAPIClient(fetchFn));
    await controller.applyURLState(operationState());

    const outcome = await controller.runAction(action);

    const actionRequest = requests.find((request) => request.method === 'POST')!;
    expect(new URL(actionRequest.url).pathname).toBe(actionPath);
    expect(body === undefined ? await actionRequest.text() : await actionRequest.json()).toEqual(body ?? '');
    expect(reads).toBe(4);
    expect(controller.snapshot.actionPending).toBeNull();
    expect(controller.snapshot.actionError).toBeNull();
    expect(outcome).toBe('succeeded');
    controller.destroy();
  });

  it('returns failure and fixed copy when mutation succeeds but reconciliation refresh fails', async () => {
    let reads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') return Response.json(syncResult());
      reads += 1;
      if (reads <= 2) {
        return Response.json(path.endsWith('/status')
          ? statusResponse({ carddav_sync: { supported_actions: ['carddav_sync'] } })
          : runsResponse([]));
      }
      return Response.json({ error: 'private_refresh_failure', message: 'private server detail' }, { status: 503 });
    });
    const controller = new OperationsController(createAPIClient(fetchFn));
    await controller.applyURLState(operationState());

    const outcome = await controller.runAction('carddav_sync');

    expect(outcome).toBe('failed');
    expect(controller.snapshot.actionError).toBe('The operation started, but current state could not be refreshed.');
    expect(JSON.stringify(controller.snapshot)).not.toContain('private server detail');
    controller.destroy();
  });

  it('does not claim an uncertain mutation was refreshed when reconciliation fails', async () => {
    let reads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') throw new Error('private transport detail');
      reads += 1;
      if (reads <= 2) {
        return Response.json(path.endsWith('/status')
          ? statusResponse({ carddav_sync: { supported_actions: ['carddav_sync'] } })
          : runsResponse([]));
      }
      return Response.json({ error: 'private_refresh_failure', message: 'private server detail' }, { status: 503 });
    });
    const controller = new OperationsController(createAPIClient(fetchFn));
    await controller.applyURLState(operationState());

    const outcome = await controller.runAction('carddav_sync');

    expect(outcome).toBe('failed');
    expect(controller.snapshot.actionError).toBe('The operation result is uncertain, and current state could not be refreshed.');
    expect(JSON.stringify(controller.snapshot)).not.toContain('private');
    controller.destroy();
  });

  it('does not invoke an action that the server did not advertise', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      return Response.json(path.endsWith('/status') ? statusResponse() : runsResponse([]));
    });
    const controller = new OperationsController(createAPIClient(fetchFn));
    await controller.applyURLState(operationState());

    await controller.runAction('visual_build');

    expect(requests.every((request) => request.method === 'GET')).toBe(true);
    expect(controller.snapshot.actionError).toBe('This operation is not currently available.');
    controller.destroy();
  });
});
