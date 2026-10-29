import { lanes } from './test/operations';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, expect, it } from 'vitest';

import App from './App.svelte';
import { createSessionController } from './lib/api/session.svelte';
import { jobLane, jobRun, sourceStatus, syncRunStatus } from './lib/jobs/fixtures.test-support';

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

it('refreshes the shell dot after Settings Contacts CardDAV Sync now', async () => {
  window.history.replaceState(null, '', '/?workspace=settings');
  let synced = false;
  const status = () => ({
    configured: true, available: true, credential_configured: true,
    enabled: true, scheduled: false, schedule: '',
    ...(synced ? { latest: {
      id: 2, trigger: 'manual', full: false, state: 'failed',
      started_at: '2026-08-30T11:00:00Z', finished_at: '2026-08-30T11:00:04Z',
      books: 0, created: 0, updated: 0, removed: 0,
      error_code: 'sync_failed', error_message: 'Contacts unavailable',
    } } : {}),
  });
  const session = sessionFor((request, path) => {
    if (path === '/api/v1/operations/status') return Response.json({ lanes: [jobLane('carddav_sync', {
      latest: synced ? jobRun('carddav_sync') : undefined,
    })] });
    if (path === '/api/v1/carddav/connections') return Response.json({ connections: [{ connection: 'default', status: status() }] });
    if (path === '/api/v1/carddav/status') return Response.json(status());
    if (path === '/api/v1/carddav/books') return Response.json({ books: [] });
    if (path === '/api/v1/carddav/runs') return Response.json({ runs: [] });
    if (path === '/api/v1/carddav/conflicts') return Response.json({ conflicts: [] });
    if (request.method === 'POST' && path === '/api/v1/carddav/sync') {
      synced = true;
      return Response.json({ status: 'failed', connections: [{ connection: 'default', status: 'failed' }] });
    }
  });
  render(App, { session });
  await screen.findByRole('button', { name: 'Sync status: Idle' });
  await fireEvent.click(await screen.findByRole('button', { name: 'Contacts' }));
  const sync = await screen.findByRole('button', { name: 'Sync now' });
  await waitFor(() => expect(sync.matches(':disabled')).toBe(false));
  await fireEvent.click(sync);
  await screen.findByText('Contacts unavailable');
  await waitFor(() => expect(screen.getByRole('button', { name: /Sync status:/ }).getAttribute('aria-label')).toBe('Sync status: Needs attention'));
});

it('refreshes the shell dot after Sources Sync now', async () => {
  window.history.replaceState(null, '', '/?workspace=sources');
  let started = false;
  const session = sessionFor(async (request, path) => {
    if (request.method === 'POST' && path === '/api/v1/sync/archive%40example.com') {
      started = true;
      return Response.json({ message: 'accepted' }, { status: 202 });
    }
    if (path === '/api/v1/sources/status') {
      const snapshot = Response.json({ sources: [sourceStatus({
        latest_sync: syncRunStatus(),
        active_sync: started ? syncRunStatus({ id: 2, status: 'running', completed_at: null }) : null,
        can_sync: !started,
        sync_unavailable_reason: started ? 'sync_already_running' : undefined,
      })] });
      return snapshot;
    }
  });
  render(App, { session });
  await screen.findByRole('button', { name: 'Sync status: Idle' });
  await screen.findByRole('button', { name: 'Sync now Example archive' });
  await fireEvent.click(screen.getByRole('button', { name: 'Sync now Example archive' }));
  await within(screen.getByRole('main', { name: 'Sources' })).findByText('Syncing');
  await screen.findByRole('button', { name: 'Sync status: Running' });
});

it('refreshes the shell dot after Settings completes Build index', async () => {
  window.history.replaceState(null, '', '/?workspace=settings');
  let built = false;
  const session = sessionFor(async (request, path) => {
    if (path === '/api/v1/operations/status') {
      const snapshot = Response.json({ lanes: [jobLane('message_embedding', {
        latest: built ? { ...jobRun('message_embedding'), state: 'succeeded', error: undefined, counters: [{ name: 'attempted', unit: 'messages', value: 1 }, { name: 'succeeded', unit: 'messages', value: 1 }, { name: 'failed', unit: 'messages', value: 0 }] } : jobRun('message_embedding'),
      })] });
      return snapshot;
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
  await screen.findByRole('button', { name: 'Sync status: Needs attention' });
  const build = await screen.findByRole('button', { name: 'Build index, Message search' });
  await fireEvent.click(build);
  await fireEvent.click(screen.getByRole('button', { name: 'Confirm full rebuild' }));
  await waitFor(() => expect(built).toBe(true));
  await within(screen.getByText('Message search').closest('li')!).findByText('On');
  await waitFor(() => expect(screen.getByRole('button', { name: /Sync status:/ }).getAttribute('aria-label')).toBe('Sync status: Idle'), { timeout: 3000 });
});

it('refreshes the shell dot after Messages fails Build index', async () => {
  window.history.replaceState(null, '', '/?workspace=everything&mode=hybrid');
  let built = false;
  const session = sessionFor(async (request, path) => {
    if (path === '/api/v1/operations/status') {
      const snapshot = Response.json({ lanes: [jobLane('message_embedding', {
        latest: built ? jobRun('message_embedding') : undefined,
      })] });
      return snapshot;
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
  await fireEvent.click(build);
  await fireEvent.click(screen.getByRole('button', { name: 'Confirm full rebuild' }));
  await waitFor(() => expect(built).toBe(true));
  await waitFor(() => expect(screen.getByRole('button', { name: /Sync status:/ }).getAttribute('aria-label')).toBe('Sync status: Needs attention'), { timeout: 3000 });
});
