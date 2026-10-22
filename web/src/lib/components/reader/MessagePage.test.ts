import { render, screen, waitFor } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';

import { createCaptureClient } from '../../telemetry/capture.test-support';
import MessagePage from './MessagePage.svelte';

it('reports a displayed linked message loading failure', async () => {
  const fetchFn = vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    if (new URL(request.url).pathname === '/api/v1/messages/2') {
      return Response.json({ message: 'Message unavailable' }, { status: 500 });
    }
    return Response.json({
      id: 7, anchor_id: 2, messages: [],
      has_before: false, has_after: false, total: 1
    });
  });
  const telemetryEvents: unknown[] = [];
  const client = createCaptureClient(fetchFn, telemetryEvents, []);
  render(MessagePage, { props: { client, messageID: 2 } });

  expect((await screen.findByRole('alert')).textContent).toContain('Message unavailable');
  await waitFor(() => expect(telemetryEvents).toEqual([
    { event: 'error_shown', properties: { surface: 'web', kind: 'message' } }
  ]));
});
