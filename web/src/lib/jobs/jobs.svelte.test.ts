import { expect, it } from 'vitest';
import { lanes as operationLanes } from '../../test/operations';
import { createAPIClient } from '../api/client';
import { JobsStore } from './jobs.svelte';
import { jobLane, jobRun, sourceStatus, syncRunStatus } from './fixtures.test-support';
import canonical from '../../../tests/e2e/fixtures/operations-public.json';
import type { OperationRunSummary } from '../api/generated/models';
import { OperationsController } from '../operations/controller.svelte';

const active = canonical.runs.find(run => run.kind === 'visual_embedding') as OperationRunSummary;
const knownJobs = [
  { name: 'failures', sources: [sourceStatus({ scheduler_last_error: 'Token expired' })], lanes: [jobLane('document_extraction')], details: ['Token expired', 'Upstream operation failed.'], health: 'failed' },
  { name: 'running work', sources: [], lanes: [jobLane('visual_embedding', { active, latest: active })], details: ['Running'], health: 'running' },
];

it.each(knownJobs.flatMap(fixture => ['sources', 'operations', 'history'].map(failedEndpoint => ({ ...fixture, failedEndpoint }))))('keeps known $name when $failedEndpoint cannot refresh', async ({ sources, lanes, details, health, failedEndpoint }) => {
  let failing = false;
  const client = createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    const source = path.includes('/sources');
    if (failing && failedEndpoint !== 'history' && source === (failedEndpoint === 'sources')) return Response.json({}, { status: 500 });
    return Response.json(source ? { sources } : { lanes: operationLanes(...lanes.map(lane => failing && failedEndpoint === 'history' ? { ...lane, active: undefined, latest: undefined, history_availability: 'unavailable' as const, unavailable_code: `${lane.kind}_history_unavailable` } : lane)) });
  });
  const operations = new OperationsController(client);
  const store = new JobsStore(client, operations);
  await store.refresh();
  expect(store.items.map(item => item.detail)).toEqual(details);
  const targets = store.items.map(item => item.target);
  failing = true;
  if (failedEndpoint === 'history') await operations.refreshStatus();
  else await store.refresh();
  expect(store.items.map(item => item.detail)).toEqual(details);
  expect(store.health).toBe(health);
  expect(store.items.map(item => item.target)).toEqual(targets);
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
    return Response.json(path.includes('/sources') ? { sources: [] } : { lanes: operationLanes(jobLane('document_extraction', { configured, latest: undefined, history_availability: 'unavailable', unavailable_code: 'document_extraction_history_unavailable' })) });
  });
  const operations = new OperationsController(client);
  const store = new JobsStore(client, operations);
  await store.refresh();
  expect(store.health).toBe(configured ? 'unavailable' : 'ok');
  expect(Boolean(store.error)).toBe(configured);
  store.dispose();
  operations.destroy();
});

it.each(['sources', 'operations'] as const)('discards a held %s observation after a newer refresh', async endpoint => {
  let changed = false;
  let hold = false;
  let release!: () => void;
  let captured!: () => void;
  let oldSnapshot!: Response;
  const capturedStatus = new Promise<void>(resolve => { captured = resolve; });
  const gate = new Promise<void>(resolve => { release = resolve; });
  const client = createAPIClient(async input => {
    const source = new URL((input as Request).url).pathname.includes('/sources');
    const snapshot = Response.json(source ? { sources: endpoint === 'sources' ? [sourceStatus({
      active_sync: changed ? syncRunStatus({ status: 'running', completed_at: null }) : null,
      can_sync: !changed, sync_unavailable_reason: changed ? 'sync_already_running' : undefined,
    })] : [] } : { lanes: operationLanes(...(endpoint === 'operations' ? [jobLane('message_embedding', {
      latest: changed ? { ...jobRun('message_embedding'), state: 'succeeded', error: undefined, counters: [{ name: 'attempted', unit: 'messages', value: 1 }, { name: 'succeeded', unit: 'messages', value: 1 }, { name: 'failed', unit: 'messages', value: 0 }] } : jobRun('message_embedding'),
    })] : [])) });
    if (hold && source === (endpoint === 'sources')) { hold = false; oldSnapshot = snapshot; captured(); await gate; }
    return snapshot;
  });
  const operations = new OperationsController(client);
  const store = new JobsStore(client, operations);
  try {
    await store.refresh();
    expect(store.health).toBe(endpoint === 'sources' ? 'ok' : 'failed');
    hold = true;
    const stale = store.refresh();
    await capturedStatus;
    changed = true;
    await store.refresh(endpoint === 'operations');
    expect(store.health).toBe(endpoint === 'sources' ? 'running' : 'ok');
    release();
    await stale;
    expect(oldSnapshot.bodyUsed).toBe(true);
    expect(store.health).toBe(endpoint === 'sources' ? 'running' : 'ok');
  } finally {
    release();
    store.dispose();
    operations.destroy();
  }
});
