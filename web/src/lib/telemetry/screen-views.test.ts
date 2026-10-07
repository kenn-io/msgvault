import { afterEach, expect, it, vi } from 'vitest';
import { createAPIClient } from '../api/client';
import { startScreenViewReporting } from './screen-views';

afterEach(() => vi.useRealTimers());

it('retries on visible focus and aborts requests when disposed', async () => {
  vi.useFakeTimers();
  const requests: Request[] = [];
  const client = createAPIClient((input) => {
    requests.push(input as Request);
    return requests.length === 1 ? Promise.reject(new Error('offline')) : new Promise<Response>(() => undefined);
  });
  const dispose = startScreenViewReporting(client, 'settings');
  await Promise.resolve();
  window.dispatchEvent(new Event('focus'));
  expect(requests).toHaveLength(2);
  expect(await requests[0].clone().json()).toEqual({
    event: 'screen_viewed',
    properties: { screen: 'settings', surface: 'web' },
  });
  vi.advanceTimersByTime(3000);
  expect(requests[1].signal.aborted).toBe(true);
  const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
  window.dispatchEvent(new Event('focus'));
  expect(requests).toHaveLength(2);
  visibility.mockRestore();
  window.dispatchEvent(new Event('focus'));
  dispose();
  expect(requests[2].signal.aborted).toBe(true);
  window.dispatchEvent(new Event('focus'));
  expect(requests).toHaveLength(3);
  expect(vi.getTimerCount()).toBe(0);
});
