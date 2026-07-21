import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import App from './App.svelte';
import { createAPIClient } from './lib/api/client';
import { createSessionController } from './lib/api/session.svelte';

describe('application foundation', () => {
  it('mounts the Relationships landmark as the default landing workspace', () => {
    const session = createSessionController(async () =>
      Response.json({ auth_mode: 'loopback', https: false, plain_http_warning: true })
    );
    render(App, { session });

    expect(screen.getByRole('main', { name: 'Relationships' })).toBeDefined();
  });

  it('shows login only when bootstrap reports authentication required', async () => {
    const session = createSessionController(async () =>
      Response.json({ auth_mode: 'required', https: false, plain_http_warning: true })
    );
    render(App, { session });

    expect(screen.getByRole('main', { name: 'Relationships' })).toBeDefined();
    expect(screen.queryByRole('form', { name: 'Log in' })).toBeNull();

    await session.bootstrap();

    expect(await screen.findByRole('form', { name: 'Log in' })).toBeDefined();
    expect(screen.queryByRole('main', { name: 'Relationships' })).toBeNull();
  });

  it('requests health from the same-origin API', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () =>
      Response.json({
        status: 'ok',
        version: 'test'
      })
    );
    const client = createAPIClient(fetchFn);

    await client.GET('/api/v1/health');

    expect(fetchFn).toHaveBeenCalledOnce();
    const request = fetchFn.mock.calls[0]?.[0];
    expect(request).toBeInstanceOf(Request);
    expect(new URL((request as Request).url).pathname).toBe('/api/v1/health');
    expect(new URL((request as Request).url).origin).toBe(window.location.origin);
  });

  it('navigates to settings and uses the session-aware mutation client', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (new URL(request.url).pathname === '/api/session') {
        return Response.json({
          auth_mode: 'session',
          csrf_token: 'csrf-token',
          https: true,
          plain_http_warning: false
        });
      }
      if (request.method === 'GET') {
        return settingsResponse('system', '"etag-a"');
      }
      return settingsResponse('dark', '"etag-b"', true);
    });
    const session = createSessionController(fetchFn);
    render(App, { session });

    await session.bootstrap();
    await fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    await fireEvent.change(await screen.findByLabelText('Theme'), { target: { value: 'dark' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save settings' }));

    await waitFor(() => expect(requests.some((request) => request.method === 'PATCH')).toBe(true));
    const patch = requests.find((request) => request.method === 'PATCH');
    expect(patch?.headers.get('X-CSRF-Token')).toBe('csrf-token');
    expect(patch?.headers.get('If-Match')).toBe('"etag-a"');
  });

  it('returns to login when a settings mutation is unauthorized', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/session') {
        return Response.json({
          auth_mode: 'session',
          csrf_token: 'csrf-token',
          https: true,
          plain_http_warning: false
        });
      }
      if (request.method === 'GET') return settingsResponse('system', '"etag-a"');
      return Response.json({ error: 'unauthorized', message: 'Session expired' }, { status: 401 });
    });
    const session = createSessionController(fetchFn);
    render(App, { session });

    await session.bootstrap();
    await fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    await fireEvent.change(await screen.findByLabelText('Theme'), { target: { value: 'dark' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save settings' }));

    expect(await screen.findByRole('form', { name: 'Log in' })).toBeDefined();
  });

  it('loads appearance once after interactive login while a session override wins', async () => {
    window.history.replaceState(null, '', '/');
    sessionStorage.setItem('msgvault.appearance.override', JSON.stringify({ theme: 'light' }));
    let settingsRequests = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/session') {
        return Response.json({ auth_mode: 'required', https: true, plain_http_warning: false });
      }
      if (path === '/api/session/login') {
        return Response.json({
          auth_mode: 'session', csrf_token: 'csrf-token', https: true, plain_http_warning: false
        });
      }
      if (path === '/api/v1/settings') {
        settingsRequests += 1;
        return Response.json({
          settings: [
            { key: 'web.theme', value: { string: 'dark' } },
            { key: 'web.density', value: { string: 'comfortable' } }
          ],
          pending_restart: false
        });
      }
      if (path === '/api/v1/explore') {
        return Response.json({ rows: [], total_count: 0, cache_revision: 'login', search_provenance: {} });
      }
      return Response.json({}, { status: 404 });
    });
    const session = createSessionController(fetchFn);
    render(App, { session });
    expect(await screen.findByRole('form', { name: 'Log in' })).toBeDefined();

    await fireEvent.input(screen.getByLabelText('API key'), { target: { value: 'test-key' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Log in' }));

    await waitFor(() => expect(document.documentElement.dataset.density).toBe('comfortable'));
    expect(document.documentElement.dataset.theme).toBe('light');
    expect(settingsRequests).toBe(1);
    await new Promise((resolve) => setTimeout(resolve));
    expect(settingsRequests).toBe(1);
  });
});

function settingsResponse(theme: string, etag: string, pendingRestart = false): Response {
  return Response.json(
    {
      settings: [
        {
          key: 'web.theme',
          group: 'browser',
          kind: 'string',
          value: { string: theme },
          options: ['system', 'light', 'dark'],
          restart_required: true
        }
      ],
      pending_restart: pendingRestart
    },
    { headers: { ETag: etag } }
  );
}
