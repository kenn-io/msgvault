import { render } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../api/client';
import VisualReadiness from './VisualReadiness.test-support.svelte';

it.each([
  { name: 'uninitialized visual search', status: 503, body: { error: 'visual_search_not_ready' }, ready: false, absent: false },
  { name: 'no generation', status: 200, body: {}, ready: false, absent: false },
  { name: 'inactive generation', status: 200, body: { generation: { state: 'building', consented: true } }, ready: false, absent: false },
  { name: 'no consent', status: 200, body: { generation: { state: 'active', consented: false } }, ready: false, absent: false },
  { name: 'active consented generation', status: 200, body: { generation: { state: 'active', consented: true } }, ready: true, absent: false },
])('$name', async ({ status, body, ready, absent }) => {
  vi.useFakeTimers();
  const fetchFn = vi.fn<typeof fetch>(async () => Response.json(body, { status }));
  const view = render(VisualReadiness, { client: createAPIClient(fetchFn) });
  try {
    await vi.advanceTimersByTimeAsync(0);
    expect(view.container.textContent).toContain(`Ready: ${ready}, absent: ${absent}`);
  } finally {
    view.unmount();
    vi.useRealTimers();
  }
});
