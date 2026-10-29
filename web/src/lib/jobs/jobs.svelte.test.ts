import { expect, it } from 'vitest';
import { createAPIClient } from '../api/client';
import { JobsStore } from './jobs.svelte';
import { jobLane, sourceStatus } from './fixtures.test-support';
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
