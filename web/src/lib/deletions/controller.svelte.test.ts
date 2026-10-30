import { afterEach, expect, it, vi } from 'vitest';
import { createAPIClient } from '../api/client';
import { DeletionsController } from './controller.svelte';

import { batch } from './fixtures.test-support';

const now = Date.parse('2026-10-29T12:00:00Z');
afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers(); });

it.each(['pending', 'in_progress', 'completed', 'failed'])('avoids retention detail reads when %s already keeps the sidebar visible', async status => {
  vi.spyOn(Date, 'now').mockReturnValue(now);
  const fetch = vi.fn(async () => Response.json({ manifests: [batch('visible', status, 2, '2026-10-29T10:00:00Z'), batch('old', 'completed')] }));
  const controller = new DeletionsController(createAPIClient(fetch));
  await controller.refresh();
  expect(controller.visible).toBe(true);
  expect(fetch).toHaveBeenCalledTimes(1);
  controller.destroy();
});

it('patches cancelled history locally and keeps it out of the sidebar without rereading details', async () => {
  let cancelled = false, detailReads = 0;
  const fetch = vi.fn<typeof globalThis.fetch>(async input => {
    const request = input as Request;
    if (request.method === 'DELETE') { cancelled = true; return Response.json({ id: 'cancelled', status: 'cancelled' }); }
    const current = batch('cancelled', cancelled ? 'cancelled' : 'pending', 2, '2026-10-29T10:00:00Z');
    if (request.url.endsWith('/deletions')) return Response.json({ manifests: [current] });
    detailReads++;
    return Response.json({ ...current, account: 'archive@example.com' });
  });
  const controller = new DeletionsController(createAPIClient(fetch));
  await controller.refresh();
  await controller.getDetail('cancelled');
  await controller.cancel('cancelled');
  expect(controller.batches[0]?.status).toBe('cancelled');
  expect((await controller.getDetail('cancelled'))?.status).toBe('cancelled');
  await controller.refresh();
  expect(controller.visible).toBe(false);
  expect(detailReads).toBe(1);
  controller.destroy();
});

it('shares in-flight lookups with page consumers and stops retention work after finding a recent completion', async () => {
  vi.spyOn(Date, 'now').mockReturnValue(now);
  const ids: string[] = [];
  const pending: Array<(response: Response) => void> = [];
  const controller = new DeletionsController(createAPIClient(async input => {
    const request = input as Request;
    if (request.url.endsWith('/deletions')) return Response.json({ manifests: Array.from({ length: 6 }, (_, i) => batch(`b${i}`, 'completed')) });
    ids.push(request.url.split('/').at(-1)!);
    return new Promise(resolve => pending.push(resolve));
  }));
  const refreshing = controller.refresh();
  await vi.waitFor(() => expect(ids).toContain('b0'));
  const page = controller.getDetail('b0');
  const laterPageRow = controller.getDetail('b4');
  const detail = { ...batch('b0', 'completed'), account: 'archive@example.com', execution: { completed_at: '2026-10-29T11:00:00Z' } };
  pending[0](Response.json(detail));
  expect(await page).toMatchObject({ account: 'archive@example.com' });
  await vi.waitFor(() => expect(ids).toContain('b4'));
  pending.slice(1).forEach((resolve, i) => resolve(Response.json({ ...batch(`b${i + 1}`, 'completed'), execution: { completed_at: '2020-01-02T00:00:00Z' } })));
  await Promise.all([refreshing, laterPageRow]);
  expect(controller.visible).toBe(true);
  expect(ids).toEqual(['b0', 'b1', 'b2', 'b3', 'b4']);
  expect(await controller.getDetail('b0')).toMatchObject({ account: 'archive@example.com' });
  controller.destroy();
});

it('caches failed page lookups and retries retention on the next polling refresh', async () => {
  vi.spyOn(Date, 'now').mockReturnValue(now);
  let reads = 0;
  const controller = new DeletionsController(createAPIClient(async input => {
    if ((input as Request).url.endsWith('/deletions')) return Response.json({ manifests: [batch('old', 'failed')] });
    return ++reads === 1 ? Response.json({}, { status: 500 }) : Response.json({ ...batch('old', 'failed'), execution: { completed_at: '2026-10-29T11:00:00Z' } });
  }));
  await controller.refresh();
  expect(await controller.getDetail('old')).toBeUndefined();
  expect(reads).toBe(1);
  await controller.refresh();
  expect(await controller.getDetail('old')).toMatchObject({ id: 'old' });
  expect(reads).toBe(2);
  expect(controller.visible).toBe(true);
  controller.destroy();
});

it('replaces a pending detail when execution completes and expires it seven days later', async () => {
  vi.spyOn(Date, 'now').mockReturnValue(now);
  let status = 'pending', reads = 0;
  const controller = new DeletionsController(createAPIClient(async input => {
    if ((input as Request).url.endsWith('/deletions')) return Response.json({ manifests: [batch('delayed', status)] });
    reads++;
    return Response.json({ ...batch('delayed', status), account: 'archive@example.com', ...(status === 'completed' ? { execution: { completed_at: '2026-10-29T11:00:00Z' } } : {}) });
  }));
  await controller.refresh();
  await controller.getDetail('delayed');
  status = 'completed';
  await controller.refresh();
  expect((await controller.getDetail('delayed'))?.status).toBe('completed');
  expect(controller.visible).toBe(true);
  expect(reads).toBe(2);
  vi.mocked(Date.now).mockReturnValue(Date.parse('2026-11-05T11:00:01Z'));
  await controller.refresh();
  await vi.waitFor(() => expect(controller.visible).toBe(false));
  expect(reads).toBe(2);
  controller.destroy();
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

it('refreshes execution progress while the batch remains in progress', async () => {
  let deleted = 0, reads = 0;
  const controller = new DeletionsController(createAPIClient(async input => {
    if ((input as Request).url.endsWith('/deletions')) return Response.json({ manifests: [batch('active', 'in_progress')] });
    reads++;
    return Response.json({ ...batch('active', 'in_progress'), execution: { succeeded: deleted } });
  }));
  await controller.refresh();
  await controller.getDetail('active');
  deleted = 100;
  await controller.refresh();
  expect((await controller.getDetail('active'))?.execution?.succeeded).toBe(100);
  expect(reads).toBe(2);
  controller.destroy();
});

it('publishes a staged batch before old retention hydration finishes and ignores its stale visibility', async () => {
  vi.spyOn(Date, 'now').mockReturnValue(now);
  let lists = 0, finish: (response: Response) => void;
  const old = batch('old', 'completed');
  const controller = new DeletionsController(createAPIClient(async input => {
    if ((input as Request).url.endsWith('/deletions')) return Response.json({ manifests: ++lists === 1 ? [old] : [old, batch('staged', 'pending')] });
    return new Promise(resolve => finish = resolve);
  }));
  await controller.refresh();
  const hydration = controller.getDetail('old');
  await controller.refresh();
  expect(lists).toBe(2);
  expect(controller.visible).toBe(true);
  finish!(Response.json({ ...old, execution: { completed_at: '2020-01-02T00:00:00Z' } }));
  await hydration;
  await new Promise<void>(resolve => queueMicrotask(resolve));
  expect(controller.visible).toBe(true);
  expect(controller.batches.map(batch => batch.id)).toEqual(['old', 'staged']);
  controller.destroy();
});
