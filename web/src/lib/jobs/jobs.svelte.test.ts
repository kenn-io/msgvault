import { expect, it } from 'vitest';
import { createAPIClient } from '../api/client';
import { JobsStore } from './jobs.svelte';

it.each(['sources', 'operations'])('keeps known failures when %s status cannot refresh', async failedEndpoint => {
  let failing = false;
  const store = new JobsStore(createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    const source = path.includes('/sources');
    if (failing && source === (failedEndpoint === 'sources')) return Response.json({}, { status: 500 });
    return Response.json(source
      ? { sources: [{ id: 1, identifier: 'archive@example.com', scheduler_last_error: 'Token expired' }] }
      : { lanes: [{ kind: 'document_extraction', configured: true, latest: { state: 'failed', error: { message: 'Parser failed' } } }] });
  }));
  await store.refresh();
  expect(store.items.map(item => item.detail)).toEqual(['Token expired', 'Parser failed']);
  failing = true;
  await store.refresh();
  expect(store.items.map(item => item.detail)).toEqual(['Token expired', 'Parser failed']);
  expect(store.health).toBe('failed');
  expect(store.error).toBe('Unable to refresh all job status.');
  failing = false;
  await store.refresh();
  expect(store.error).toBeUndefined();
  store.dispose();
});

it('reports unavailable status instead of Synced when an initial endpoint fails', async () => {
  const store = new JobsStore(createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    return path.includes('/sources') ? Response.json({}, { status: 500 }) : Response.json({ lanes: [] });
  }));
  await store.refresh();
  expect(store.health).toBe('unavailable');
  store.dispose();
});

it.each([true, false])('reports unavailable history only for configured lanes, configured=%s', async configured => {
  const store = new JobsStore(createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    return Response.json(path.includes('/sources') ? { sources: [] } : { lanes: [{ kind: 'document_extraction', configured, history_availability: 'unavailable' }] });
  }));
  await store.refresh();
  expect(store.health).toBe(configured ? 'unavailable' : 'ok');
  expect(Boolean(store.error)).toBe(configured);
  store.dispose();
});
