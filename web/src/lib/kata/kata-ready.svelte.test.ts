import { expect, it, vi } from 'vitest';
import { IntegrationReadiness, KATA_RETRY_MS } from './kata-ready.svelte';

it('asks again on a read after two failed configuration checks', async () => {
  vi.useFakeTimers();
  let finish!: (ready: boolean) => void;
  const probe = vi.fn(() => new Promise<boolean>(resolve => { finish = resolve; }))
    .mockRejectedValueOnce(new Error('Status unavailable'))
    .mockRejectedValueOnce(new Error('Status still unavailable'));
  const readiness = new IntegrationReadiness(probe);
  try {
    expect(readiness.ready).toBe(false);
    await vi.advanceTimersByTimeAsync(0);
    expect(probe).toHaveBeenCalledOnce();
    await vi.advanceTimersByTimeAsync(KATA_RETRY_MS);
    expect(probe).toHaveBeenCalledTimes(2);
    expect(readiness.ready).toBe(false);
    await vi.advanceTimersByTimeAsync(0);
    expect(probe).toHaveBeenCalledTimes(3);
    finish(true);
    await vi.advanceTimersByTimeAsync(0);
    expect(readiness.ready).toBe(true);
  } finally {
    readiness.dispose();
    vi.useRealTimers();
  }
});
