import { afterEach, expect, it, vi } from 'vitest';
import { createAPIClient } from '../api/client';
import { reportAction, reportSearch, startTelemetryReporting, stopTelemetryReporting, telemetryReporter } from './actions';
import { startScreenViewReporting } from './screen-views';

let stop: () => void;
afterEach(() => { stop?.(); vi.restoreAllMocks(); vi.useRealTimers(); });

it('routes daily actions and waits for visible screens with a bounded request', async () => {
  const requests: Request[] = [];
  const client = createAPIClient((input) => {
    const request = input as Request;
    requests.push(request);
    if (requests.length === 1) return Promise.resolve(Response.json({ status: 'queued' }, { status: 202 }));
    return new Promise<Response>((_resolve, reject) => {
      request.signal.addEventListener('abort', () => reject(request.signal.reason), { once: true });
    });
  });
  const reporter = startTelemetryReporting(client);
  stop = () => stopTelemetryReporting(client);
  await vi.waitFor(() => expect(requests).toHaveLength(1));
  const daily = vi.spyOn(reporter, 'reportDaily').mockImplementation(() => undefined);
  await reportSearch(client, '  ', 'semantic');
  expect(daily).not.toHaveBeenCalled();
  await reportAction(client, 'message_opened');
  await reportSearch(client, 'needle', 'full_text');
  await reportSearch(client, 'needle', 'hybrid');
  await reportSearch(client, 'needle', 'semantic');
  expect(daily.mock.calls).toEqual([
    ['message_opened', { event: 'message_opened', properties: { surface: 'web' } }],
    ['search_run:text', { event: 'search_run', properties: { surface: 'web', search_type: 'text' } }],
    ['search_run:hybrid', { event: 'search_run', properties: { surface: 'web', search_type: 'hybrid' } }],
    ['search_run:semantic', { event: 'search_run', properties: { surface: 'web', search_type: 'semantic' } }],
  ]);
  daily.mockRestore();
  const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
  const dispose = startScreenViewReporting(client, 'settings');
  window.dispatchEvent(new Event('focus'));
  expect(requests).toHaveLength(1);
  visibility.mockReturnValue('visible');
  window.dispatchEvent(new Event('focus'));
  await vi.waitFor(() => expect(requests).toHaveLength(2));
  expect(await requests[1].clone().json()).toEqual({
    event: 'screen_viewed', properties: { screen: 'settings', surface: 'web' },
  });
  await vi.waitFor(() => expect(requests[1].signal.aborted).toBe(true), { timeout: 4000 });
  dispose();
});

it('leaves screens unreported until the app registers a reporter', async () => {
  const requests: Request[] = [];
  const client = createAPIClient((input) => {
    requests.push(input as Request);
    return Promise.resolve(Response.json({ status: 'queued' }, { status: 202 }));
  });
  const dispose = startScreenViewReporting(client, 'settings');
  await reportAction(client, 'message_opened');
  window.dispatchEvent(new Event('focus'));
  await Promise.resolve();
  expect(telemetryReporter(client)).toBeUndefined();
  expect(requests).toHaveLength(0);
  dispose();
});
