import { disabledTaskLookup } from '../tasks/fixtures.test-support';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
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

it('passes exact person navigation from the message reader', async () => {
  const onOpenPerson = vi.fn();
  const fetchFn = vi.fn<typeof fetch>(async (input) => {
    const path = new URL((input as Request).url).pathname;
    if (path === '/api/v1/messages/2') return Response.json({ id: 2, conversation_id: 7, from: 'Same name', to: ['Same name'], subject: 'Follow up', body: 'Loaded body' });
    if (path === '/api/v1/conversations/7') return Response.json({ id: 7, anchor_id: 2, messages: [{ id: 2, conversation_id: 7, message_type: 'email', from: 'Same name', from_participant_id: 11, to: [], subject: 'Follow up', body: 'Loaded body' }], has_before: false, has_after: false, total: 1 });
    if (path.endsWith('/tasks/status')) return Response.json({ state: 'disabled', message: 'Set up Tasks' });
    if (path.endsWith('/kata/status')) return Response.json({ state: 'disabled' });
    return Response.json({ recordings: [], ...disabledTaskLookup(2) });
  });
  const client = createCaptureClient(fetchFn, [], []);
  render(MessagePage, { client, messageID: 2, onOpenPerson });
  await screen.findByText('Loaded body');
  await fireEvent.click(screen.getAllByRole('button', { name: 'Same name' })[0]!);
  expect(onOpenPerson).toHaveBeenCalledWith(11);
});
