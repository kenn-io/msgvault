import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import App from './App.svelte';
import { createAPIClient } from './lib/api/client';
import { createSessionController } from './lib/api/session.svelte';
import { SEARCH_MODE_PREFERENCE_KEY } from './lib/search/modes';

describe('application foundation', () => {
  it('mounts the Relationships landmark once bootstrap succeeds', async () => {
    const session = createSessionController(async () =>
      Response.json({ auth_mode: 'loopback', https: false, plain_http_warning: true })
    );
    render(App, { session });

    expect(await screen.findByRole('main', { name: 'Relationships' })).toBeDefined();
  });

  it('shows a quiet connecting state until bootstrap resolves, then login when required', async () => {
    const session = createSessionController(async () =>
      Response.json({ auth_mode: 'required', https: false, plain_http_warning: true })
    );
    render(App, { session });

    expect(screen.getByRole('main', { name: 'Connecting' })).toBeDefined();
    expect(screen.queryByRole('main', { name: 'Relationships' })).toBeNull();
    expect(screen.queryByRole('form', { name: 'Log in' })).toBeNull();

    await session.bootstrap();

    expect(await screen.findByRole('form', { name: 'Log in' })).toBeDefined();
    expect(screen.queryByRole('main', { name: 'Relationships' })).toBeNull();
  });

  it('shows a bootstrap error with retry instead of the shell, and recovers on retry', async () => {
    let sessionCalls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/session') {
        sessionCalls += 1;
        if (sessionCalls === 1) throw new TypeError('Failed to fetch');
        return Response.json({ auth_mode: 'loopback', https: false, plain_http_warning: false });
      }
      if (path === '/api/v1/settings') {
        return Response.json({
          settings: [{ key: 'web.density', value: { string: 'comfortable' } }],
          pending_restart: false
        });
      }
      if (path === '/api/v1/explore') {
        return Response.json({ rows: [], total_count: 0, cache_revision: 'boot', search_provenance: {} });
      }
      return Response.json({}, { status: 404 });
    });
    const session = createSessionController(fetchFn);
    render(App, { session });

    const retry = await screen.findByRole('button', { name: 'Retry' });
    expect(screen.getByRole('main', { name: 'Connection error' })).toBeDefined();
    expect(screen.getByRole('alert')).toBeDefined();
    expect(screen.queryByRole('main', { name: 'Relationships' })).toBeNull();
    expect(screen.queryByRole('form', { name: 'Log in' })).toBeNull();

    await fireEvent.click(retry);

    expect(await screen.findByRole('main', { name: 'Relationships' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
    await waitFor(() => expect(document.documentElement.dataset.density).toBe('comfortable'));
  });

  it('routes an unauthorized bootstrap to login, not the connection error state', async () => {
    const session = createSessionController(async () =>
      Response.json({ error: 'unauthorized', message: 'Session expired' }, { status: 401 })
    );
    render(App, { session });

    expect(await screen.findByRole('form', { name: 'Log in' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
    expect(screen.queryByRole('main', { name: 'Connection error' })).toBeNull();
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

  it.each([
    ['semantic', 'Semantic'],
    ['magic', 'Full text']
  ])('starts in the configured default search mode %s when URL and browser preference are silent', async (configured, expectedLabel) => {
    localStorage.removeItem(SEARCH_MODE_PREFERENCE_KEY);
    window.history.replaceState(
      null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`
    );
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/session') {
        return Response.json({ auth_mode: 'loopback', https: false, plain_http_warning: false });
      }
      if (path === '/api/v1/settings') {
        return Response.json({
          settings: [{ key: 'web.default_search_mode', value: { string: configured } }],
          pending_restart: false
        });
      }
      if (path === '/api/v1/explore') {
        return Response.json({ rows: [], total_count: 0, cache_revision: 'boot', search_provenance: {} });
      }
      return Response.json({}, { status: 404 });
    });
    const session = createSessionController(fetchFn);
    render(App, { session });
    await session.bootstrap();

    const settingsLoaded = () => fetchFn.mock.calls.some((call) => {
      const request = call[0];
      return request instanceof Request && new URL(request.url).pathname === '/api/v1/settings';
    });
    await waitFor(() => expect(settingsLoaded()).toBe(true));
    await new Promise((resolve) => setTimeout(resolve));

    await waitFor(() => expect(
      screen.getByRole('radio', { name: expectedLabel }).getAttribute('aria-checked')
    ).toBe('true'));
    window.history.replaceState(null, '', '/');
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
