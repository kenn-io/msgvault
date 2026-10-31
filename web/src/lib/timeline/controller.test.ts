import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../api/client';
import { TimelineController } from './controller.svelte';
import { golden } from './fixtures';

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

describe('TimelineController', () => {
  it('cancels the previous request and discards its late completion', async () => {
    const first = deferred<Response>();
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      requests.push(input as Request);
      return requests.length === 1 ? first.promise : Response.json({ ...golden(), mode: 'anchor' });
    });
    const controller = new TimelineController(createAPIClient(fetchFn));
    try {
      const old = controller.load({ mode: 'day' });
      await controller.around('msg:14');
      expect(requests[0]!.signal.aborted).toBe(true);
      expect(new URL(requests[1]!.url).searchParams.get('around')).toBe('msg:14');
      first.resolve(Response.json(golden()));
      await old;
      expect(controller.result?.mode).toBe('anchor');
      expect(controller.loading).toBe(false);
    } finally { controller.destroy(); }
  });

  it('clears results on a new request and retains typed 409 candidates', async () => {
    const fetchFn = vi.fn<typeof fetch>()
      .mockResolvedValueOnce(Response.json(golden()))
      .mockResolvedValueOnce(Response.json({ error: 'ambiguous_reference', candidates: [{
        person_id: 7, person_uid: 'example-7', participant_ids: [17], display_name: 'Alex Example', address: 'alex@example.test',
      }] }, { status: 409 }));
    const controller = new TimelineController(createAPIClient(fetchFn));
    try {
      await controller.load({ mode: 'day' });
      const next = controller.load({ mode: 'relationship', with: 'Alex' });
      expect(controller.result).toBeNull();
      await next;
      expect(controller.candidates[0]?.person_id).toBe(7);
      expect(controller.error).toBeNull();
    } finally { controller.destroy(); }
  });

  it('cancels in-flight requests when destroyed and ignores late success', async () => {
    const pending = deferred<Response>();
    let request: Request | undefined;
    const controller = new TimelineController(createAPIClient(vi.fn(async (input) => {
      request = input as Request;
      return pending.promise;
    })));
    const loading = controller.load({ mode: 'day' });
    controller.destroy();
    expect(request?.signal.aborted).toBe(true);
    pending.resolve(Response.json(golden()));
    await loading;
    expect(controller.result).toBeNull();
    expect(controller.loading).toBe(false);
  });

  it('continues the same request with the returned opaque cursor', async () => {
    const fetchFn = vi.fn<typeof fetch>()
      .mockResolvedValueOnce(Response.json({ ...golden(), cursor: 'opaque-page' }))
      .mockResolvedValueOnce(Response.json(golden()));
    const controller = new TimelineController(createAPIClient(fetchFn));
    try {
      await controller.load({ mode: 'anchor', around: 'msg:14', before: 0, after: 0 });
      await controller.nextPage();
      const url = new URL((fetchFn.mock.calls[1]![0] as Request).url);
      expect(Object.fromEntries(url.searchParams)).toMatchObject({
        mode: 'anchor', around: 'msg:14', before: '0', after: '0', cursor: 'opaque-page', format: 'json',
      });
    } finally { controller.destroy(); }
  });
});
