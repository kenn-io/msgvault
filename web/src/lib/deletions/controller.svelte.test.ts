import { afterEach, expect, it, vi } from 'vitest';
import { createAPIClient } from '../api/client';
import { DeletionsController } from './controller.svelte';

const now = Date.parse('2026-10-29T12:00:00Z');
const batch = (id: string, status: string, created_at = '2020-01-01T00:00:00Z') => ({ id, status, created_at, message_count: 2, description: '', created_by: 'api' });
afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers(); });

it.each(['pending', 'in_progress', 'completed', 'failed'])('avoids retention detail reads when %s already keeps the sidebar visible', async status => {
  vi.spyOn(Date, 'now').mockReturnValue(now);
  const fetch = vi.fn(async () => Response.json({ manifests: [batch('visible', status, '2026-10-29T10:00:00Z'), batch('old', 'completed')] }));
  const controller = new DeletionsController(createAPIClient(fetch));
  await controller.refresh();
  expect(controller.visible).toBe(true);
  expect(fetch).toHaveBeenCalledTimes(1);
  controller.destroy();
});

it('keeps cancelled-only history out of the sidebar without fetching details', async () => {
  const fetch = vi.fn(async () => Response.json({ manifests: [batch('cancelled', 'cancelled', '2026-10-29T10:00:00Z')] }));
  const controller = new DeletionsController(createAPIClient(fetch));
  await controller.refresh();
  expect(controller.visible).toBe(false);
  expect(controller.batches[0]?.status).toBe('cancelled');
  expect(fetch).toHaveBeenCalledTimes(1);
  controller.destroy();
});

it('shares four in-flight lookups with page consumers and stops retention work after finding a recent completion', async () => {
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
  await vi.waitFor(() => expect(ids).toEqual(['b0', 'b1', 'b2', 'b3']));
  const page = controller.getDetail('b0');
  const laterPageRow = controller.getDetail('b4');
  expect(ids).toHaveLength(4);
  const detail = { ...batch('b0', 'completed'), account: 'archive@example.com', execution: { completed_at: '2026-10-29T11:00:00Z' } };
  pending[0](Response.json(detail));
  expect(await page).toMatchObject({ account: 'archive@example.com' });
  await vi.waitFor(() => expect(ids).toHaveLength(5));
  pending.slice(1).forEach(resolve => resolve(Response.json({ execution: { completed_at: '2020-01-02T00:00:00Z' } })));
  await Promise.all([refreshing, laterPageRow]);
  expect(controller.visible).toBe(true);
  expect(ids).toEqual(['b0', 'b1', 'b2', 'b3', 'b4']);
  expect(await controller.getDetail('b0')).toMatchObject({ account: 'archive@example.com' });
  expect(ids).toHaveLength(5);
  controller.destroy();
});

it.each(['refresh', 'open'])('suppresses timer retries after a failed lookup and retries on explicit %s', async retry => {
  vi.spyOn(Date, 'now').mockReturnValue(now);
  let reads = 0;
  const controller = new DeletionsController(createAPIClient(async input => {
    if ((input as Request).url.endsWith('/deletions')) return Response.json({ manifests: [batch('old', 'failed')] });
    return ++reads === 1 ? Response.json({}, { status: 500 }) : Response.json({ execution: { completed_at: '2026-10-29T11:00:00Z' } });
  }));
  await controller.refresh();
  await controller.refresh();
  expect(reads).toBe(1);
  if (retry === 'refresh') await controller.refresh(true);
  else await controller.getDetail('old', true);
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
  expect(controller.details.delayed.status).toBe('completed');
  expect(controller.visible).toBe(true);
  expect(reads).toBe(2);
  vi.mocked(Date.now).mockReturnValue(Date.parse('2026-11-05T11:00:01Z'));
  await controller.refresh();
  expect(controller.visible).toBe(false);
  expect(reads).toBe(2);
  controller.destroy();
});

it('releases timed-out detail slots and aborts owned requests on teardown', async () => {
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
