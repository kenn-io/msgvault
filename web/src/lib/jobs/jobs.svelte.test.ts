import { expect, it } from 'vitest';
import { createAPIClient } from '../api/client';
import { JobsStore } from './jobs.svelte';
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

it('keeps a joined global source read alive when the page leaves', async () => {
  let answer!: (response: Response) => void;
  let request!: Request;
  const client = createAPIClient(async input => {
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
  page.abort();
  await pageLeft;
  expect(request.signal.aborted).toBe(false);
  answer(Response.json({ sources: [sourceStatus({ active_sync: syncRunStatus({ status: 'running', completed_at: null }) })] }));
  await globalRead;
  expect(store.health).toBe('running');
  store.dispose();
  const after = store.refresh();
  await after;
  expect(store.health).toBe('running');
  operations.destroy();
});

it('keeps a superseding source read pending and discards an older completion', async () => {
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
  const freshRead = store.readSources(new AbortController().signal, true);
  expect(requests[0]!.signal.aborted).toBe(true);
  answers[0]!(Response.json({ sources: [] }));
  await oldRead;
  expect(store.loaded).toBe(false);
  expect(store.error).toBeUndefined();
  answers[1]!(Response.json({ sources: [sourceStatus({ source_type: 'mbox', sync_unavailable_reason: 'source_not_schedulable', latest_sync: syncRunStatus({ status: 'running', completed_at: null }) })] }));
  await freshRead;
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
  const globalRead = store.refresh();
  const pageRead = store.readSources(new AbortController().signal);
  await expect(pageRead).rejects.toMatchObject({ name: 'TimeoutError' });
  await globalRead;
  expect(reads).toBe(1);
  expect(store.error).toBe('Unable to refresh all job status.');
  await store.refresh();
  expect(reads).toBe(2);
  expect(store.error).toBeUndefined();
  store.dispose();
  operations.destroy();
}, 25_000);
