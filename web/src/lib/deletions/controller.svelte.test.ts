import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../api/client';
import { DeletionsController } from './controller.svelte';
import { batch } from './fixtures.test-support';

it.each([
  [[], false],
  [['completed', 'cancelled'], false],
  [['completed', 'cancelled', 'failed'], true],
  [['pending'], true],
  [['in_progress'], true],
] as const)('shows the sidebar for statuses %j: %s', async (statuses, visible) => {
  const fetch = vi.fn<typeof globalThis.fetch>().mockResolvedValue(Response.json({
    manifests: statuses.map((status, i) => batch(`batch-${i}`, status)),
  }));
  const controller = new DeletionsController(createAPIClient(fetch));
  try {
    await controller.refresh();
    expect(controller.visible).toBe(visible);
  } finally { controller.destroy(); }
});

it('patches cancelled history locally and hides the sidebar', async () => {
  const fetch = vi.fn<typeof globalThis.fetch>()
    .mockResolvedValueOnce(Response.json({ manifests: [batch('staged', 'pending')] }))
    .mockResolvedValueOnce(Response.json({ id: 'staged', status: 'cancelled' }));
  const controller = new DeletionsController(createAPIClient(fetch));
  try {
    await controller.refresh();
    await controller.cancel('staged');
    expect(controller.batches[0]?.status).toBe('cancelled');
    expect(controller.visible).toBe(false);
    expect(fetch.mock.calls.map(([input]) => [(input as Request).method, new URL((input as Request).url).pathname])).toEqual([
      ['GET', '/api/v1/deletions'], ['DELETE', '/api/v1/deletions/staged'],
    ]);
  } finally { controller.destroy(); }
});

it('queues a fresh list after a write while an older list is pending', async () => {
  let finish: (response: Response) => void;
  const fetch = vi.fn<typeof globalThis.fetch>().mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockImplementationOnce(async () => Response.json({ manifests: [batch('staged', 'pending')] }));
  const controller = new DeletionsController(createAPIClient(fetch));
  const poll = controller.refresh();
  const afterStage = controller.refresh();
  finish!(Response.json({ manifests: [] }));
  await Promise.all([poll, afterStage]);
  expect(fetch).toHaveBeenCalledTimes(2);
  expect(controller.batches.map(batch => batch.id)).toEqual(['staged']);
  expect(controller.visible).toBe(true);
  controller.destroy();
});

it('keeps cancellation when a stale list arrives and the follow-up refresh fails', async () => {
  let finish: (response: Response) => void;
  const fetch = vi.fn<typeof globalThis.fetch>()
    .mockResolvedValueOnce(Response.json({ manifests: [batch('staged', 'pending')] }))
    .mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockResolvedValueOnce(Response.json({ id: 'staged', status: 'cancelled' }))
    .mockRejectedValueOnce(new Error('Refresh failed'));
  const controller = new DeletionsController(createAPIClient(fetch));
  try {
    await controller.refresh();
    const poll = controller.refresh();
    await controller.cancel('staged');
    const afterCancel = controller.refresh();
    finish!(Response.json({ manifests: [batch('staged', 'pending')] }));
    await Promise.all([poll, afterCancel]);
    expect(controller.error).toBe("Can't load staged deletions.");
    expect(controller.batches[0]?.status).toBe('cancelled');
    expect(controller.visible).toBe(false);
  } finally { controller.destroy(); }
});
