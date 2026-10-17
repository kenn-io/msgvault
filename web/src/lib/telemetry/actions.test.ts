import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../api/client';
import { reportAction, reportSearch } from './actions';

it.each(['search_run', 'message_opened'])('sends a later %s after disabled telemetry is enabled', async (event) => {
  let enabled = false;
  const captured: unknown[] = [];
  const fetchFn = vi.fn<typeof fetch>(async (input) => {
    const body = await (input as Request).json();
    if (enabled) captured.push(body);
    return Response.json({ status: 'queued' }, { status: 202 });
  });
  const client = createAPIClient(fetchFn);
  const report = () => event === 'search_run' ? reportSearch(client, 'needle', 'full_text') : reportAction(client, event);
  await report();
  enabled = true;
  await report();
  expect(captured).toEqual([{ event, properties: { surface: 'web', ...(event === 'search_run' ? { search_type: 'text' } : {}) } }]);
});
