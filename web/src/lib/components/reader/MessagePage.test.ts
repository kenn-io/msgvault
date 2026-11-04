import { disabledTaskLookup } from '../tasks/fixtures.test-support';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it, onTestFinished, vi } from 'vitest';

import { createCaptureClient } from '../../telemetry/capture.test-support';
import MessagePage from './MessagePage.svelte';

it.each([41, 42])('restores initiating message %i after participant navigation and a Back reload', async (initiatingMessageID) => {
  window.history.replaceState({ existing: 'preserved' }, '', '/messages/42');
  onTestFinished(() => window.history.replaceState(null, '', '/'));
  const messages = [41, 42].map(id => ({
    id, conversation_id: 7, message_type: 'email', from: 'Sender Example',
    from_participant_id: 11, subject: 'Follow up', body: `Body ${id}`
  }));
  const fetchFn = vi.fn<typeof fetch>(async (input) => {
    const url = new URL((input as Request).url);
    if (url.pathname === '/api/v1/messages/42') return Response.json(messages[1]);
    if (url.pathname === '/api/v1/conversations/7') return Response.json({
      id: 7, anchor_id: Number(url.searchParams.get('anchor')), messages,
      has_before: false, has_after: false, total: 2
    });
    if (url.pathname.endsWith('/tasks/status')) return Response.json({ state: 'disabled', message: 'Set up Tasks' });
    if (url.pathname.endsWith('/kata/status')) return Response.json({ state: 'disabled' });
    return Response.json({ recordings: [], ...disabledTaskLookup(41) });
  });
  const client = createCaptureClient(fetchFn, [], []);
  const onOpenPerson = vi.fn(() => {
    // Model the new document's history entry; jsdom cannot location.assign.
    window.history.pushState(null, '', '/?workspace=relationships&target=person:11');
  });
  const view = render(MessagePage, { client, messageID: 42, onOpenPerson });
  await screen.findByText('Body 42');
  await fireEvent.click(screen.getByRole('button', { name: /Expand message 41/ }));
  await screen.findByText('Body 41');
  await fireEvent.click(within(screen.getByRole('article', { name: `Message ${initiatingMessageID}` })).getByRole('button', { name: 'Sender Example' }));
  expect(onOpenPerson).toHaveBeenCalledWith(11);
  view.unmount();

  window.history.back();
  await waitFor(() => expect(window.location.pathname).toBe('/messages/42'));
  expect(window.history.state).toMatchObject({ existing: 'preserved' });
  render(MessagePage, { client, messageID: 42, onOpenPerson });
  expect(await screen.findByText(`Body ${initiatingMessageID}`)).toBeTruthy();
});

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
