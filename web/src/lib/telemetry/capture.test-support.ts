import { afterEach } from 'vitest';
import { createAPIClient, type APIClient } from '../api/client';
import { startTelemetryReporting, stopTelemetryReporting } from './actions';

const reporting: APIClient[] = [];
afterEach(() => { for (const client of reporting.splice(0)) stopTelemetryReporting(client); });

// Registers the app's telemetry reporter and records every event except its app_opened permission check.
export function createCaptureClient(fetchFn: typeof fetch, events: unknown[], requests?: Promise<void>[]) {
  const client = createAPIClient(async (input, init) => {
    const request = input as Request;
    if (new URL(request.url).pathname !== '/api/v1/telemetry/events') return fetchFn(input, init);
    const capture = request.json().then(event => { if (event.event !== 'app_opened') events.push(event); });
    requests?.push(capture);
    await capture;
    return Response.json({ status: 'queued' }, { status: 202 });
  });
  startTelemetryReporting(client);
  reporting.push(client);
  return client;
}
