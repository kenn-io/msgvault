import { lanes } from './test/operations';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, expect, it } from 'vitest';

import App from './App.svelte';
import { createSessionController } from './lib/api/session.svelte';
import { sourceStatus } from './lib/jobs/fixtures.test-support';

function sessionFor(handle: (request: Request, path: string) => Promise<Response | undefined> | Response | undefined) {
  return createSessionController(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    const response = await handle(request, path);
    if (response) return response;
    if (path === '/api/session') return Response.json({ auth_mode: 'loopback', https: false });
    if (path === '/api/v1/settings') return Response.json({ settings: [], pending_restart: false });
    if (path === '/api/v1/saved-views') return Response.json({ saved_views: [] });
    if (path === '/api/v1/sources/status') return Response.json({ sources: [] });
    if (path === '/api/v1/operations/status') return Response.json({ lanes: lanes() });
    if (path === '/api/v1/telemetry/events') return Response.json({ status: 'disabled' }, { status: 202 });
    return Response.json({}, { status: 404 });
  });
}

afterEach(() => {
  cleanup();
  window.history.replaceState(null, '', '/');
});

it('refreshes shell status after Settings Contacts CardDAV Sync now', async () => {
  window.history.replaceState(null, '', '/?workspace=settings');
  let statusReads = 0;
  const status = () => ({
    configured: true, available: true, credential_configured: true,
    enabled: true, scheduled: false, schedule: '',
  });
  const session = sessionFor((request, path) => {
    if (path === '/api/v1/operations/status') {
      statusReads++;
      return Response.json({ lanes: lanes() });
    }
    if (path === '/api/v1/carddav/connections') return Response.json({ connections: [{ connection: 'default', status: status() }] });
    if (path === '/api/v1/carddav/status') return Response.json(status());
    if (path === '/api/v1/carddav/books') return Response.json({ books: [] });
    if (path === '/api/v1/carddav/runs') return Response.json({ runs: [] });
    if (path === '/api/v1/carddav/conflicts') return Response.json({ conflicts: [] });
    if (request.method === 'POST' && path === '/api/v1/carddav/sync') {
      return Response.json({ status: 'failed', connections: [{ connection: 'default', status: 'failed' }] });
    }
  });
  render(App, { session });
  await screen.findByRole('button', { name: 'Sync status: Idle' });
  await fireEvent.click(await screen.findByRole('button', { name: 'Contacts' }));
  const sync = await screen.findByRole('button', { name: 'Sync now' });
  await waitFor(() => expect(sync.matches(':disabled')).toBe(false));
  const before = statusReads;
  await fireEvent.click(sync);
  await waitFor(() => expect(statusReads).toBeGreaterThan(before));
});

it('refreshes shell status after Sources Sync now', async () => {
  window.history.replaceState(null, '', '/?workspace=sources');
  let statusReads = 0;
  const session = sessionFor(async (request, path) => {
    if (path === '/api/v1/operations/status') {
      statusReads++;
      return Response.json({ lanes: lanes() });
    }
    if (request.method === 'POST' && path === '/api/v1/sync/archive%40example.com') {
      return Response.json({ message: 'accepted' }, { status: 202 });
    }
    if (path === '/api/v1/sources/status') {
      return Response.json({ sources: [sourceStatus()] });
    }
  });
  render(App, { session });
  await screen.findByRole('button', { name: 'Sync status: Idle' });
  await screen.findByRole('button', { name: 'Sync now Example archive' });
  const before = statusReads;
  await fireEvent.click(screen.getByRole('button', { name: 'Sync now Example archive' }));
  await waitFor(() => expect(statusReads).toBeGreaterThan(before));
});

it('refreshes shell status after Settings completes Build index', async () => {
  window.history.replaceState(null, '', '/?workspace=settings');
  let built = false;
  let statusReads = 0;
  const session = sessionFor(async (request, path) => {
    if (path === '/api/v1/operations/status') {
      statusReads++;
      return Response.json({ lanes: lanes() });
    }
    if (path === '/api/v1/search/coverage') {
      if (built) return Response.json({
        eligible_count: 10, embedded_count: 10, percentage: 100, cache_revision: 'cache-2',
        status: 'ready', actions: [],
      });
      return Response.json({
        eligible_count: 10, embedded_count: 0, percentage: 0, cache_revision: 'cache-1',
        status: 'initializing', actions: ['build_index'],
      });
    }
    if (request.method === 'POST' && path === '/api/v1/cli/run') {
      built = true;
      return new Response('{"type":"complete"}\n');
    }
  });
  render(App, { session });
  await screen.findByRole('button', { name: 'Sync status: Idle' });
  const build = await screen.findByRole('button', { name: 'Build index, Message search' });
  const before = statusReads;
  await fireEvent.click(build);
  await fireEvent.click(screen.getByRole('button', { name: 'Confirm full rebuild' }));
  await waitFor(() => expect(statusReads).toBeGreaterThan(before));
});

it('refreshes shell status after Messages fails Build index', async () => {
  window.history.replaceState(null, '', '/?workspace=everything&mode=hybrid');
  let built = false;
  let statusReads = 0;
  const session = sessionFor(async (request, path) => {
    if (path === '/api/v1/operations/status') {
      statusReads++;
      return Response.json({ lanes: lanes() });
    }
    if (path === '/api/v1/search/coverage') {
      return Response.json({
        eligible_count: 10, embedded_count: 0, percentage: 0, cache_revision: 'cache-1',
        status: 'initializing', actions: built ? [] : ['build_index'],
      });
    }
    if (request.method === 'POST' && path === '/api/v1/cli/run') {
      built = true;
      return new Response('{"type":"complete","error":"Embedding endpoint failed"}\n');
    }
  });
  render(App, { session });
  await screen.findByRole('button', { name: 'Sync status: Idle' });
  const build = await screen.findByRole('button', { name: 'Build index' });
  const before = statusReads;
  await fireEvent.click(build);
  await fireEvent.click(screen.getByRole('button', { name: 'Confirm full rebuild' }));
  await waitFor(() => expect(statusReads).toBeGreaterThan(before));
});
