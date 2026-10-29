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

it.each([false, true])('carries exact participant IDs and task setup through omitted bodies when outbound=%s', async (isFromMe) => {
  const onOpenPerson = vi.fn();
  const onOpenSettings = vi.fn();
  const fetchFn = vi.fn<typeof fetch>(async (input) => {
    const path = new URL((input as Request).url).pathname;
    if (path === '/api/v1/messages/2') return Response.json({ id: 2, conversation_id: 7, from: 'Same name', to: ['Same name'], subject: 'Follow up', body: 'Loaded body' });
    if (path === '/api/v1/conversations/7') return Response.json({ id: 7, anchor_id: 2, messages: [{ id: 2, conversation_id: 7, message_type: 'email', from: 'Same name', from_participant_id: 11, is_from_me: isFromMe, to: ['Same name'], to_participants: [{ display: 'Same name', participant_id: 22 }], subject: 'Follow up', body: '', body_omitted: true }], has_before: false, has_after: false, total: 1 });
    if (path.endsWith('/tasks/status')) return Response.json({ state: 'disabled', message: 'Set up Tasks' });
    if (path.endsWith('/kata/status')) return Response.json({ state: 'disabled' });
    return Response.json({ recordings: [], state: 'disabled', complete: true, tasks: [] });
  });
  const client = createCaptureClient(fetchFn, [], []);
  render(MessagePage, { client, messageID: 2, onOpenPerson, onOpenSettings });
  await screen.findByText('Loaded body');
  const people = screen.getAllByRole('button', { name: 'Same name' });
  if (isFromMe) {
    expect(screen.getByText('You')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'You' })).toBeNull();
  } else await fireEvent.click(people[0]!);
  await fireEvent.click(people[isFromMe ? 0 : 1]!);
  expect(onOpenPerson.mock.calls).toEqual(isFromMe ? [[22]] : [[11], [22]]);
  await fireEvent.click(await screen.findByRole('button', { name: 'Set up' }));
  expect(onOpenSettings).toHaveBeenCalledOnce();
});
