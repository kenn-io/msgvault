import { createAPIClient } from '../api/client';

export function createCaptureClient(fetchFn: typeof fetch, events: unknown[], requests?: Promise<void>[]) {
  return createAPIClient(async (input, init) => {
    const request = input as Request;
    if (new URL(request.url).pathname !== '/api/v1/telemetry/events') return fetchFn(input, init);
    const capture = request.json().then(event => { events.push(event); });
    requests?.push(capture);
    await capture;
    return Response.json({ status: 'queued' }, { status: 202 });
  });
}
