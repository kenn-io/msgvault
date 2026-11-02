import { render } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../api/client';
import VisualReadiness from './VisualReadiness.test-support.svelte';

it.each([
  { name: 'active generation without consent', body: { generation: { state: 'active', consented: false } }, ready: false },
  { name: 'inactive generation', body: { generation: { state: 'building', consented: true } }, ready: false },
  { name: 'active consented generation', body: { generation: { state: 'active', consented: true } }, ready: true },
])('reports visual search readiness as $ready for $name', async ({ body, ready }) => {
  vi.useFakeTimers();
  const fetchFn = vi.fn<typeof fetch>(async () => Response.json(body));
  const view = render(VisualReadiness, { client: createAPIClient(fetchFn) });
  try {
    await vi.advanceTimersByTimeAsync(0);
    expect(view.container.textContent).toContain(`Ready: ${ready}`);
  } finally {
    view.unmount();
    vi.useRealTimers();
  }
});
