import { afterEach, expect, it, vi } from 'vitest';
import { createAPIClient } from '../api/client';
import { DeletionsController } from './controller.svelte';

import { batch } from './fixtures.test-support';

afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers(); });

it('shows 200 old completed batches without detail reads and hides an empty list', async () => {
  const fetch = vi.fn<typeof globalThis.fetch>()
    .mockResolvedValueOnce(Response.json({ manifests: Array.from({ length: 200 }, (_, i) => batch(`old-${i}`, 'completed')) }))
    .mockResolvedValueOnce(Response.json({ manifests: [] }));
  const controller = new DeletionsController(createAPIClient(fetch));
  try {
    await controller.refresh();
    expect(controller.visible).toBe(true);
    await controller.refresh();
    expect(controller.visible).toBe(false);
    expect(fetch.mock.calls.map(([input]) => new URL((input as Request).url).pathname)).toEqual(['/api/v1/deletions', '/api/v1/deletions']);
  } finally { controller.destroy(); }
});

it('patches cancelled history locally and follows the default list for sidebar visibility', async () => {
  let cancelled = false, detailReads = 0;
  const controller = new DeletionsController(createAPIClient(async input => {
    const request = input as Request;
    if (request.method === 'DELETE') { cancelled = true; return Response.json({ id: 'cancelled', status: 'cancelled' }); }
    const current = batch('cancelled', cancelled ? 'cancelled' : 'pending');
    if (request.url.endsWith('/deletions')) return Response.json({ manifests: cancelled ? [] : [current] });
    detailReads++;
    return Response.json({ ...current, account: 'archive@example.com' });
  }));
  try {
    await controller.refresh();
    await controller.getDetail('cancelled');
    await controller.cancel('cancelled');
    expect(controller.batches[0]?.status).toBe('cancelled');
    expect((await controller.getDetail('cancelled'))?.status).toBe('cancelled');
    expect(controller.visible).toBe(true);
    await controller.refresh();
    expect(controller.visible).toBe(false);
    expect(detailReads).toBe(1);
  } finally { controller.destroy(); }
});

it('shares in-flight page lookups and caches their account details', async () => {
  let finish!: (response: Response) => void;
  const fetch = vi.fn<typeof globalThis.fetch>(async input => {
    if ((input as Request).url.endsWith('/deletions')) return Response.json({ manifests: [batch('old', 'completed')] });
    return new Promise(resolve => { finish = resolve; });
  });
  const controller = new DeletionsController(createAPIClient(fetch));
  try {
    await controller.refresh();
    const first = controller.getDetail('old');
    const second = controller.getDetail('old');
    finish(Response.json({ ...batch('old', 'completed'), account: 'archive@example.com' }));
    expect(await first).toMatchObject({ account: 'archive@example.com' });
    expect(await second).toEqual(await controller.getDetail('old'));
    expect(fetch).toHaveBeenCalledTimes(2);
  } finally { controller.destroy(); }
});

it('replaces cached page detail when the list status changes', async () => {
  let status = 'pending', reads = 0;
  const controller = new DeletionsController(createAPIClient(async input => {
    if ((input as Request).url.endsWith('/deletions')) return Response.json({ manifests: [batch('delayed', status)] });
    reads++;
    return Response.json({ ...batch('delayed', status), account: 'archive@example.com' });
  }));
  try {
    await controller.refresh();
    await controller.getDetail('delayed');
    status = 'completed';
    await controller.refresh();
    expect((await controller.getDetail('delayed'))?.status).toBe('completed');
    expect(reads).toBe(2);
  } finally { controller.destroy(); }
});

it('bounds detail reads to four, releases timed-out slots and aborts owned requests on teardown', async () => {
  vi.useFakeTimers();
  const signals: AbortSignal[] = [];
  const controller = new DeletionsController(createAPIClient(async input => {
    signals.push((input as Request).signal);
    return new Promise<Response>(() => undefined);
  }), 100);
  const pending = Array.from({ length: 5 }, (_, i) => controller.getDetail(`b${i}`));
  expect(signals).toHaveLength(4);
  await vi.advanceTimersByTimeAsync(100);
  expect(signals).toHaveLength(5);
  expect(signals.slice(0, 4).every(signal => signal.aborted)).toBe(true);
  controller.destroy();
  await Promise.all(pending);
  expect(signals[4].aborted).toBe(true);
});

it('queues a fresh list after a write while an older list is pending', async () => {
  let finish: (response: Response) => void;
  const fetch = vi.fn<typeof globalThis.fetch>().mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockImplementationOnce(async () => Response.json({ manifests: [batch('staged', 'pending')] }));
  const controller = new DeletionsController(createAPIClient(fetch));
  const poll = controller.refresh();
  const afterStage = controller.refresh(true);
  finish!(Response.json({ manifests: [] }));
  await Promise.all([poll, afterStage]);
  expect(fetch).toHaveBeenCalledTimes(2);
  expect(controller.batches.map(batch => batch.id)).toEqual(['staged']);
  expect(controller.visible).toBe(true);
  controller.destroy();
});

it('keeps execution detail cached until a fresh read reconciles the completed status', async () => {
  let deleted = 0, reads = 0;
  const controller = new DeletionsController(createAPIClient(async input => {
    if ((input as Request).url.endsWith('/deletions')) return Response.json({ manifests: [batch('active', 'in_progress')] });
    reads++;
    return Response.json({ ...batch('active', deleted ? 'completed' : 'in_progress'), execution: { succeeded: deleted, completed_at: '2026-10-29T11:00:00Z' } });
  }));
  await controller.refresh();
  await controller.getDetail('active');
  deleted = 100;
  await controller.refresh();
  expect((await controller.getDetail('active'))?.execution?.succeeded).toBe(0);
  expect(reads).toBe(1);
  expect((await controller.getDetail('active', { fresh: true }))?.execution?.succeeded).toBe(100);
  expect(controller.batches[0]?.status).toBe('completed');
  expect(controller.visible).toBe(true);
  expect(reads).toBe(2);
  controller.destroy();
});

it('waits for the current detail after a status refresh supersedes the first read, but leaves genuine failures alone', async () => {
  let status = 'pending', activeReads = 0, failedReads = 0;
  let finish!: (response: Response) => void;
  const controller = new DeletionsController(createAPIClient(async input => {
    const request = input as Request;
    if (request.url.endsWith('/deletions')) return Response.json({ manifests: [batch('active', status), batch('broken', 'pending')] });
    if (request.url.endsWith('/broken')) { failedReads++; return Response.json({}, { status: 503 }); }
    if (++activeReads === 1) return new Promise<Response>(() => undefined);
    return new Promise<Response>(resolve => { finish = resolve; });
  }));
  try {
    await controller.refresh();
    const opening = controller.getDetail('active', { fresh: true });
    status = 'in_progress';
    await controller.refresh();
    const replacement = controller.getDetail('active');
    const current = { ...batch('active', 'in_progress'), account: 'archive@example.com' };
    finish(Response.json(current));
    expect(await opening).toMatchObject(current);
    expect(await replacement).toMatchObject(current);
    expect(controller.details.active).toMatchObject(current);
    expect(activeReads).toBe(2);
    expect(await controller.getDetail('broken')).toBeUndefined();
    expect(failedReads).toBe(1);
  } finally { controller.destroy(); }
});

it('follows the replacement when a status refresh supersedes a queued detail read', async () => {
  let status = 'pending', targetReads = 0;
  const blocked: Array<(response: Response) => void> = [];
  const controller = new DeletionsController(createAPIClient(async input => {
    const request = input as Request;
    if (request.url.endsWith('/deletions')) return Response.json({ manifests: [batch('target', status), ...Array.from({ length: 4 }, (_, i) => batch(`busy-${i}`, 'pending'))] });
    if (request.url.endsWith('/target')) { targetReads++; return Response.json({ ...batch('target', 'in_progress'), account: 'archive@example.com' }); }
    return new Promise<Response>(resolve => blocked.push(resolve));
  }));
  const busy: Array<Promise<unknown>> = [];
  try {
    await controller.refresh();
    busy.push(...Array.from({ length: 4 }, (_, i) => controller.getDetail(`busy-${i}`)));
    const opening = controller.getDetail('target');
    expect(targetReads).toBe(0);
    status = 'in_progress';
    await controller.refresh();
    const replacement = controller.getDetail('target');
    blocked[0](Response.json(batch('busy-0', 'pending')));
    expect(await opening).toMatchObject({ id: 'target', status: 'in_progress', account: 'archive@example.com' });
    expect(await replacement).toEqual(controller.details.target);
    expect(targetReads).toBe(1);
  } finally { controller.destroy(); await Promise.all(busy); }
});
