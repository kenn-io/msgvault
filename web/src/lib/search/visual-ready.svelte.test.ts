import { render } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../api/client';
import { KATA_RETRY_MS } from '../kata/kata-ready.svelte';
import VisualReadiness from './VisualReadiness.test-support.svelte';

it.each([
  { name: 'uninitialized visual search', status: 503, body: { error: 'visual_search_not_ready' }, ready: false, absent: true, reads: 1 },
  { name: 'no generation', status: 200, body: {}, ready: false, absent: true, reads: 1 },
  { name: 'inactive generation', status: 200, body: { generation: { state: 'building', consented: true } }, ready: false, absent: true, reads: 1 },
  { name: 'no consent', status: 200, body: { generation: { state: 'active', consented: false } }, ready: false, absent: true, reads: 1 },
  { name: 'active consented generation', status: 200, body: { generation: { state: 'active', consented: true } }, ready: true, absent: false, reads: 1 },
  { name: 'other service failure', status: 503, body: { error: 'unavailable' }, ready: false, absent: false, reads: 2 },
])('$name', async ({ status, body, ready, absent, reads }) => {
  vi.useFakeTimers();
  const fetchFn = vi.fn<typeof fetch>(async () => Response.json(body, { status }));
  const view = render(VisualReadiness, { client: createAPIClient(fetchFn) });
  try {
    await vi.advanceTimersByTimeAsync(0);
    expect(view.container.textContent).toContain(`Ready: ${ready}, absent: ${absent}`);
    expect(fetchFn).toHaveBeenCalledOnce();
    expect((fetchFn.mock.calls[0]![0] as Request).url).toContain('/api/v1/multimodal/status');
    await vi.advanceTimersByTimeAsync(KATA_RETRY_MS);
    expect(fetchFn).toHaveBeenCalledTimes(reads);
    expect(view.component.read()).toBe(ready);
    await vi.advanceTimersByTimeAsync(0);
    if (absent) expect(fetchFn).toHaveBeenCalledOnce();
  } finally {
    view.unmount();
    vi.useRealTimers();
  }
});
