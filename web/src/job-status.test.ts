import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, expect, it } from 'vitest';

import App from './App.svelte';
import { createSessionController } from './lib/api/session.svelte';
import { jobLane, jobRun, sourceStatus, syncRunStatus } from './lib/jobs/fixtures.test-support';

function sessionFor(handle: (request: Request, path: string) => Response | undefined) {
  return createSessionController(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    const response = handle(request, path);
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

it('updates the shell dot when Sources starts Sync now', async () => {
  window.history.replaceState(null, '', '/?workspace=sources');
  let started = false;
  const session = sessionFor((request, path) => {
    if (request.method === 'POST' && path === '/api/v1/sync/archive%40example.com') {
      started = true;
      return Response.json({ message: 'accepted' }, { status: 202 });
    }
    if (path === '/api/v1/sources/status') return Response.json({ sources: [sourceStatus({
      latest_sync: syncRunStatus(),
      active_sync: started ? syncRunStatus({ id: 2, status: 'running', completed_at: null }) : null,
      can_sync: !started,
    })] });
  });
  render(App, { session });
  await screen.findByRole('button', { name: 'Sync status: Idle' });
  await fireEvent.click(await screen.findByRole('button', { name: 'Sync now Example archive' }));
  await within(screen.getByRole('main', { name: 'Sources' })).findByText('Syncing');
  await waitFor(() => expect(screen.getByRole('button', { name: /Sync status:/ }).getAttribute('aria-label')).toBe('Sync status: Running'));
});

it.each([
  { outcome: 'completed', pageLabel: 'Completed', dotLabel: 'Idle' },
  { outcome: 'failed', pageLabel: 'Failed', dotLabel: 'Needs attention' },
] as const)('updates the shell dot when a Sources poll observes sync $outcome', async ({ outcome, pageLabel, dotLabel }) => {
  window.history.replaceState(null, '', '/?workspace=sources');
  let finished = false;
  const session = sessionFor((_request, path) => {
    if (path === '/api/v1/sources/status') return Response.json({ sources: [sourceStatus({
      active_sync: finished ? null : syncRunStatus({ id: 2, status: 'running', completed_at: null }),
      latest_sync: finished ? syncRunStatus({ id: 2, status: outcome, error_message: outcome === 'failed' ? 'Sync failed' : null }) : syncRunStatus(),
      can_sync: finished,
    })] });
  });
  render(App, { session });
  await screen.findByRole('button', { name: 'Sync status: Running' });
  finished = true;
  await within(screen.getByRole('main', { name: 'Sources' })).findByText(pageLabel, {}, { timeout: 3000 });
  await waitFor(() => expect(screen.getByRole('button', { name: /Sync status:/ }).getAttribute('aria-label')).toBe(`Sync status: ${dotLabel}`));
});

it('refreshes the shell dot after Settings Overview Build index', async () => {
  window.history.replaceState(null, '', '/?workspace=settings');
  let built = false;
  const session = sessionFor((request, path) => {
    if (path === '/api/v1/operations/status') return Response.json({ lanes: [jobLane('message_embedding', {
      latest: built ? { ...jobRun('message_embedding'), state: 'succeeded', error: undefined } : jobRun('message_embedding'),
    })] });
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
  await fireEvent.click(await screen.findByRole('button', { name: 'Build index, Message search' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Confirm full rebuild' }));
  const row = screen.getByText('Message search').closest('li')!;
  await within(row).findByText('On');
  await waitFor(() => expect(screen.getByRole('button', { name: /Sync status:/ }).getAttribute('aria-label')).toBe('Sync status: Idle'));
});

it('shows queued enrichment as Queued in the shell job menu', async () => {
  window.history.replaceState(null, '', '/?workspace=sources');
  const session = sessionFor((_request, path) => {
    if (path === '/api/v1/operations/status') return Response.json({ lanes: [jobLane('person_enrichment', {
      active: { ...jobRun('person_enrichment'), state: 'queued', finished_at: undefined, error: undefined },
      latest: undefined,
    })] });
  });
  render(App, { session });
  await fireEvent.click(await screen.findByRole('button', { name: 'Sync status: Running' }));
  await screen.findByText('Person enrichment');
  const job = screen.getByRole('menuitem', { name: /Person enrichment/ });
  expect(job.textContent).toContain('Queued');
  expect(within(job).queryByText('Running')).toBeNull();
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
