import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, expect, it } from 'vitest';

import { tick } from 'svelte';
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
    if (path === '/api/v1/operations/status') return Response.json({ lanes: [] });
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

it('post-build refresh supersedes an older shell status request', async () => {
  window.history.replaceState(null, '', '/?workspace=settings');
  let built = false;
  let hold = false;
  let release!: () => void;
  let captured!: () => void;
  let oldSnapshot!: Response;
  const capturedStatus = new Promise<void>(resolve => { captured = resolve; });
  const gate = new Promise<void>(resolve => { release = resolve; });
  const session = sessionFor(async (request, path) => {
    if (path === '/api/v1/operations/status') {
      const snapshot = Response.json({ lanes: [jobLane('message_embedding', {
        latest: built ? { ...jobRun('message_embedding'), state: 'succeeded', error: undefined } : jobRun('message_embedding'),
      })] });
      if (hold) { hold = false; oldSnapshot = snapshot; captured(); await gate; }
      return snapshot;
    }
    if (path === '/api/v1/search/coverage') return Response.json({
      eligible_count: 10, embedded_count: 0, percentage: 0, cache_revision: 'cache-1',
      status: 'initializing', actions: built ? [] : ['build_index'],
    });
    if (request.method === 'POST' && path === '/api/v1/cli/run') {
      built = true;
      return new Response('{"type":"complete"}\n');
    }
  });
  render(App, { session });
  await screen.findByRole('button', { name: 'Sync status: Needs attention' });
  await screen.findByRole('button', { name: 'Build index, Message search' });
  hold = true;
  window.dispatchEvent(new Event('focus'));
  await capturedStatus;
  await fireEvent.click(screen.getByRole('button', { name: 'Build index, Message search' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Confirm full rebuild' }));
  await within(screen.getByText('Message search').closest('li')!).findByText('On');
  release();
  await waitFor(() => expect(oldSnapshot.bodyUsed).toBe(true));
  await new Promise(resolve => setTimeout(resolve, 0));
  await tick();
  await waitFor(() => expect(screen.getByRole('button', { name: /Sync status:/ }).getAttribute('aria-label')).toBe('Sync status: Idle'));
});

it('Sources Sync now stays running when an older shell poll completes', async () => {
  window.history.replaceState(null, '', '/?workspace=sources');
  let started = false;
  let hold = false;
  let release!: () => void;
  let captured!: () => void;
  let oldSnapshot!: Response;
  const capturedStatus = new Promise<void>(resolve => { captured = resolve; });
  const gate = new Promise<void>(resolve => { release = resolve; });
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
      })] });
      if (hold) { hold = false; oldSnapshot = snapshot; captured(); await gate; }
      return snapshot;
    }
  });
  render(App, { session });
  await screen.findByRole('button', { name: 'Sync status: Idle' });
  await screen.findByRole('button', { name: 'Sync now Example archive' });
  hold = true;
  window.dispatchEvent(new Event('focus'));
  await capturedStatus;
  await fireEvent.click(screen.getByRole('button', { name: 'Sync now Example archive' }));
  await within(screen.getByRole('main', { name: 'Sources' })).findByText('Syncing');
  await screen.findByRole('button', { name: 'Sync status: Running' });
  release();
  await waitFor(() => expect(oldSnapshot.bodyUsed).toBe(true));
  await new Promise(resolve => setTimeout(resolve, 0));
  await tick();
  expect(screen.getByRole('button', { name: /Sync status:/ }).getAttribute('aria-label')).toBe('Sync status: Running');
});

it.each(['running', 'failed'] as const)('refreshes the shell dot after Messages search Build index: %s', async outcome => {
  window.history.replaceState(null, '', '/?workspace=everything&mode=hybrid');
  let built = false;
  const session = sessionFor((request, path) => {
    if (path === '/api/v1/operations/status') return Response.json({ lanes: [jobLane('message_embedding', {
      latest: built || outcome === 'running' ? jobRun('message_embedding') : undefined,
      ...(built && outcome === 'running' ? { active: { ...jobRun('message_embedding'), state: 'running', finished_at: undefined, error: undefined } } : {}),
    })] });
    if (path === '/api/v1/search/coverage') return Response.json({
      eligible_count: 10, embedded_count: 0, percentage: 0, cache_revision: 'cache-1',
      status: 'initializing', actions: built ? [] : ['build_index'],
    });
    if (request.method === 'POST' && path === '/api/v1/cli/run') {
      built = true;
      return new Response(outcome === 'failed' ? '{"type":"complete","error":"Embedding endpoint failed"}\n' : '{"type":"complete"}\n');
    }
  });
  render(App, { session });
  await screen.findByRole('button', { name: outcome === 'failed' ? 'Sync status: Idle' : 'Sync status: Needs attention' });
  await fireEvent.click(await screen.findByRole('button', { name: 'Build index' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Confirm full rebuild' }));
  await waitFor(() => expect(built).toBe(true));
  await waitFor(() => expect(screen.queryByRole('button', { name: 'Build index' })).toBeNull());
  await waitFor(() => expect(screen.getByRole('button', { name: /Sync status:/ }).getAttribute('aria-label')).toBe(outcome === 'failed' ? 'Sync status: Needs attention' : 'Sync status: Running'), { timeout: 3000 });
  if (outcome === 'failed') await screen.findByText('Embedding endpoint failed');
});

it('keeps a newer shell observation when an older Sources read finishes', async () => {
  let holdSources = false;
  let running = false;
  let entered!: () => void;
  let release!: () => void;
  const captured = new Promise<void>(resolve => { entered = resolve; });
  const gate = new Promise<void>(resolve => { release = resolve; });
  const session = sessionFor(async (_request, path) => {
    if (path === '/api/v1/sources/status') {
      const snapshot = Response.json({ sources: [sourceStatus({
        latest_sync: syncRunStatus(),
        active_sync: running ? syncRunStatus({ id: 2, status: 'running', completed_at: null }) : null,
        sync_unavailable_reason: running ? 'sync_already_running' : undefined,
      })] });
      if (holdSources) { holdSources = false; entered(); await gate; }
      return snapshot;
    }
  });
  render(App, { session });
  await screen.findByRole('button', { name: 'Sync status: Idle' });
  holdSources = true;
  await fireEvent.click(screen.getByRole('button', { name: 'Sources' }));
  await captured;
  // A scheduled sync starts after Sources captured its response.
  running = true;
  window.dispatchEvent(new Event('focus'));
  await screen.findByRole('button', { name: 'Sync status: Running' });
  release();
  await within(screen.getByRole('main', { name: 'Sources' })).findByText('Completed');
  await tick();
  expect(screen.getByRole('button', { name: /Sync status:/ }).getAttribute('aria-label')).toBe('Sync status: Running');
});
