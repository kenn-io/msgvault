import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../api/client';
import { JobsStore, SOURCE_STATUS_TIMEOUT_MS } from './jobs.svelte';
import { jobLane, sourceStatus, syncRunStatus } from './fixtures.test-support';
import canonical from '../../../tests/e2e/fixtures/operations-public.json';
import type { OperationRunSummary } from '../api/generated/models';
import { OperationsController } from '../operations/controller.svelte';

const active = canonical.runs.find(run => run.kind === 'visual_embedding') as OperationRunSummary;
const knownJobs = [
  { name: 'failures', sources: [sourceStatus({ scheduler_last_error: 'Token expired' })], lanes: [jobLane('document_extraction')], details: ['Token expired', 'Upstream operation failed.'], health: 'failed' },
  { name: 'running work', sources: [], lanes: [jobLane('visual_embedding', { active, latest: active })], details: ['Running'], health: 'running' },
];

it.each(knownJobs.flatMap(fixture => ['sources', 'operations'].map(failedEndpoint => ({ ...fixture, failedEndpoint }))))('keeps known $name when $failedEndpoint cannot refresh', async ({ sources, lanes, details, health, failedEndpoint }) => {
  let failing = false;
  const client = createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    const source = path.includes('/sources');
    if (failing && source === (failedEndpoint === 'sources')) return Response.json({}, { status: 500 });
    return Response.json(source ? { sources } : { lanes });
  });
  const operations = new OperationsController(client);
  const store = new JobsStore(client, operations);
  await store.refresh();
  expect(store.items.map(item => item.detail)).toEqual(details);
  failing = true;
  await store.refresh();
  expect(store.items.map(item => item.detail)).toEqual(details);
  expect(store.health).toBe(health);
  expect(store.error).toBe('Unable to refresh all job status.');
  failing = false;
  await store.refresh();
  expect(store.error).toBeUndefined();
  store.dispose();
  operations.destroy();
});

it.each([true, false])('reports unavailable history only for configured lanes, configured=%s', async configured => {
  const client = createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    return Response.json(path.includes('/sources') ? { sources: [] } : { lanes: [jobLane('document_extraction', { configured, latest: undefined, history_availability: 'unavailable', unavailable_code: 'document_extraction_history_unavailable' })] });
  });
  const operations = new OperationsController(client);
  const store = new JobsStore(client, operations);
  await store.refresh();
  expect(store.health).toBe(configured ? 'unavailable' : 'ok');
  expect(Boolean(store.error)).toBe(configured);
  store.dispose();
  operations.destroy();
});

it.each(['page', 'owner'] as const)('releases joined source readers when the %s leaves', async leaving => {
  let answer!: (response: Response) => void;
  let request!: Request;
  let reads = 0;
  const client = createAPIClient(async input => {
    reads += 1;
    const current = input as Request;
    if (new URL(current.url).pathname.includes('/sources')) {
      request = current;
      return new Promise<Response>(resolve => { answer = resolve; });
    }
    return Response.json({ lanes: [] });
  });
  const operations = new OperationsController(client);
  const store = new JobsStore(client, operations);
  const page = new AbortController();
  const pageRead = store.readSources(page.signal);
  const pageLeft = expect(pageRead).rejects.toMatchObject({ name: 'AbortError' });
  const globalRead = store.refresh();
  if (leaving === 'owner') store.dispose();
  else page.abort();
  if (leaving === 'page') await pageLeft;
  expect(request.signal.aborted).toBe(leaving === 'owner');
  answer(Response.json({ sources: [sourceStatus({ active_sync: syncRunStatus({ status: 'running', completed_at: null }) })] }));
  await globalRead;
  await pageLeft;
  expect(store.health).toBe(leaving === 'page' ? 'running' : 'unavailable');
  store.dispose();
  const after = store.refresh();
  await after;
  await expect(store.readSources(new AbortController().signal)).rejects.toMatchObject({ name: 'AbortError' });
  expect(store.health).toBe(leaving === 'page' ? 'running' : 'unavailable');
  expect(reads).toBe(2);
  operations.destroy();
});

it.each(['complete', 'abort', 'timeout'] as const)('adopts the superseding source read while preserving caller %s', async outcome => {
  const answers: ((response: Response) => void)[] = [];
  const requests: Request[] = [];
  const client = createAPIClient(async input => {
    const request = input as Request;
    if (new URL(request.url).pathname.includes('/sources')) {
      requests.push(request);
      return new Promise<Response>(resolve => { answers.push(resolve); });
    }
    return Response.json({ lanes: [] });
  });
  const operations = new OperationsController(client);
  const store = new JobsStore(client, operations);
  const oldRead = store.refresh();
  const page = new AbortController();
  const pageRead = store.readSources(page.signal);
  const pageResult = outcome === 'complete' ? pageRead : expect(pageRead).rejects.toMatchObject({ name: outcome === 'timeout' ? 'TimeoutError' : 'AbortError' });
  const freshRead = store.readSources(new AbortController().signal, true);
  expect(requests[0]!.signal.aborted).toBe(true);
  answers[0]!(Response.json({ sources: [] }));
  await new Promise(resolve => setTimeout(resolve, 0));
  expect(store.loaded).toBe(false);
  expect(store.error).toBeUndefined();
  if (outcome !== 'complete') {
    page.abort(new DOMException('Caller left', outcome === 'timeout' ? 'TimeoutError' : 'AbortError'));
    await pageResult;
    expect(requests[1]!.signal.aborted).toBe(false);
  }
  answers[1]!(Response.json({ sources: [sourceStatus({ source_type: 'mbox', sync_unavailable_reason: 'source_not_schedulable', latest_sync: syncRunStatus({ status: 'running', completed_at: null }) })] }));
  await Promise.all([oldRead, freshRead, pageResult]);
  if (outcome === 'complete') expect((await pageRead).data?.sources[0]?.latest_sync?.status).toBe('running');
  expect(store.loaded).toBe(true);
  expect(store.health).toBe('running');
  expect(requests).toHaveLength(2);
  store.dispose();
  await store.refresh();
  expect(requests).toHaveLength(2);
  operations.destroy();
});

it('bounds a hung shared source read and permits a fresh retry', async () => {
  let reads = 0;
  const client = createAPIClient(async input => {
    const request = input as Request;
    if (new URL(request.url).pathname.includes('/sources')) {
      reads += 1;
      if (reads > 1) return Response.json({ sources: [] });
      return new Promise<Response>((_resolve, reject) => {
        request.signal.addEventListener('abort', () => reject(request.signal.reason), { once: true });
      });
    }
    return Response.json({ lanes: [] });
  });
  const operations = new OperationsController(client);
  const store = new JobsStore(client, operations);
  const nativeTimeout = AbortSignal.timeout.bind(AbortSignal);
  const deadline = vi.spyOn(AbortSignal, 'timeout').mockImplementation(ms => nativeTimeout(ms === SOURCE_STATUS_TIMEOUT_MS ? 20 : ms));
  try {
    const globalRead = store.refresh();
    const pageRead = store.readSources(new AbortController().signal);
    await expect(pageRead).rejects.toMatchObject({ name: 'TimeoutError' });
    await globalRead;
    expect(reads).toBe(1);
    expect(store.error).toBe('Unable to refresh all job status.');
    await store.refresh();
    expect(reads).toBe(2);
    expect(store.error).toBeUndefined();
  } finally {
    deadline.mockRestore();
    store.dispose();
    operations.destroy();
  }
});
