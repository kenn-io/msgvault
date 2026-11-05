import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, expect, it } from 'vitest';
import App from './App.svelte';
import { parseExploreURLState } from './lib/explore/state.svelte';
import { createSessionController } from './lib/api/session.svelte';

afterEach(() => history.replaceState(null, '', '/'));

it.each(['email', 'whatsapp'])('opens a direct %s message without scanning Explore', async (messageType) => {
  history.replaceState(null, '', '/messages/42001');
  const requested: string[] = [];
  const exploreRequests: Record<string, unknown>[] = [];
  const message = {
    id: 42001, source_id: 3, source_message_id: 'source-message', conversation_id: 71,
    subject: 'Linked message', message_type: messageType, from: 'sender@example.com',
    to: ['reader@example.com'], sent_at: '2020-01-01T12:00:00Z', snippet: 'Old message',
    labels: [], has_attachments: false, size_bytes: 20, body: 'The requested old message', attachments: [],
  };
  const session = createSessionController(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    const url = new URL(request.url);
    requested.push(url.pathname + url.search);
    if (url.pathname === '/api/session') return Response.json({ auth_mode: 'loopback', https: false, plain_http_warning: false });
    if (url.pathname === '/api/v1/settings') return Response.json({ settings: [], pending_restart: false });
    if (url.pathname === '/api/v1/explore') {
      exploreRequests.push(await request.json());
      return Response.json({ rows: [], total_count: 0, cache_revision: 'links', search_provenance: {} });
    }
    if (url.pathname === '/api/v1/messages/42001') return Response.json(message);
    if (url.pathname === '/api/v1/conversations/71') {
      expect(url.searchParams.get('anchor')).toBe('42001');
      expect(url.searchParams.get('before')).toBe('25');
      expect(url.searchParams.get('after')).toBe('25');
      return Response.json({ id: 71, anchor_id: 42001, messages: [message], has_before: true, has_after: true, total: 100000 });
    }
    return Response.json({ message: 'Unexpected request' }, { status: 404 });
  });
  render(App, { session });
  const reader = await screen.findByRole('complementary', { name: 'Reading pane: Linked message' });
  expect((await within(reader).findByRole('article', { name: 'Message 42001' })).textContent).toContain('The requested old message');
  expect(screen.getByRole('navigation', { name: 'Primary' })).toBeDefined();
  expect(screen.getByRole('searchbox', { name: 'Search messages' })).toBeDefined();
  expect(location.pathname).toBe('/');
  expect(parseExploreURLState(location.search).selectedRow).toBe('recording:42001');
  expect(requested.filter((path) => path === '/api/v1/messages/42001')).toHaveLength(1);
  expect(exploreRequests.length).toBeGreaterThan(0);
  expect(exploreRequests.every(body => !body.cursor)).toBe(true);
});

it('keeps a missing linked message in the shell and closes its reader', async () => {
  history.replaceState(null, '', '/messages/42001');
  const session = createSessionController(async (input) => {
    const path = new URL((input as Request).url).pathname;
    if (path === '/api/session') return Response.json({ auth_mode: 'loopback', https: false });
    if (path === '/api/v1/settings') return Response.json({ settings: [], pending_restart: false });
    if (path === '/api/v1/explore') return Response.json({ rows: [], total_count: 0, cache_revision: 'missing', search_provenance: {} });
    return Response.json({ message: 'Message not found' }, { status: 404 });
  });
  render(App, { session });
  expect(await screen.findByText('This message is no longer available.')).toBeDefined();
  expect(screen.getByRole('navigation', { name: 'Primary' })).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Close reading pane' }));
  await waitFor(() => expect(screen.queryByRole('complementary', { name: /Reading pane/ })).toBeNull());
  expect(screen.getByRole('main', { name: 'Messages' })).toBeDefined();
  expect(location.pathname).toBe('/');
  expect(parseExploreURLState(location.search).selectedRow).toBeNull();
});
