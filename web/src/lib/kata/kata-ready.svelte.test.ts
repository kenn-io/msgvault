import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../api/client';
import { IntegrationReadiness, KataReadiness, KATA_RETRY_MS } from './kata-ready.svelte';

it.each(['failed configuration', 'unreachable Kata'])('asks again on a read after two %s checks', async state => {
  vi.useFakeTimers();
  let finish!: (ready: boolean) => void;
  const probe = vi.fn(() => new Promise<boolean>(resolve => { finish = resolve; }));
  if (state === 'failed configuration') probe.mockRejectedValueOnce(new Error('Status unavailable')).mockRejectedValueOnce(new Error('Status still unavailable'));
  const fetchFn = vi.fn<typeof fetch>(() => probe().then(ready => Response.json({ state: ready ? 'ready' : 'unavailable', project: 'example' })))
    .mockResolvedValueOnce(Response.json({ state: 'unreachable', project: 'example' }))
    .mockResolvedValueOnce(Response.json({ state: 'unreachable', project: 'example' }));
  const readiness = state === 'unreachable Kata' ? new KataReadiness(createAPIClient(fetchFn)) : new IntegrationReadiness(probe);
  const check = state === 'unreachable Kata' ? fetchFn : probe;
  try {
    expect(readiness.ready).toBe(false);
    await vi.advanceTimersByTimeAsync(0);
    expect(check).toHaveBeenCalledOnce();
    await vi.advanceTimersByTimeAsync(KATA_RETRY_MS - 1000);
    expect(check).toHaveBeenCalledOnce();
    await vi.advanceTimersByTimeAsync(1000);
    expect(check).toHaveBeenCalledTimes(2);
    expect(readiness.ready).toBe(false);
    await vi.advanceTimersByTimeAsync(0);
    expect(check).toHaveBeenCalledTimes(3);
    finish(true);
    await vi.advanceTimersByTimeAsync(0);
    expect(readiness.ready).toBe(true);
  } finally {
    readiness.dispose();
    vi.useRealTimers();
  }
});
