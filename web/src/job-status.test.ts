import { lanes } from './test/operations';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, expect, it } from 'vitest';

import App from './App.svelte';
import { createSessionController } from './lib/api/session.svelte';
import { jobLane, sourceStatus } from './lib/jobs/fixtures.test-support';

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

it.each([
  { workspace: 'Settings Contacts', url: '/?workspace=settings', action: 'Sync now', contacts: true, build: false },
  { workspace: 'Sources', url: '/?workspace=sources', action: 'Sync now Example archive', contacts: false, build: false },
  { workspace: 'Settings Overview', url: '/?workspace=settings', action: 'Build index, Message search', contacts: false, build: true },
  { workspace: 'Messages', url: '/?workspace=everything&mode=hybrid', action: 'Build index', contacts: false, build: true },
])('refreshes shell status after an action in $workspace', async ({ workspace, url, action, contacts, build }) => {
  window.history.replaceState(null, '', url);
  let completed = false;
  const cardDAVStatus = {
    configured: true, available: true, credential_configured: true,
    enabled: true, scheduled: false, schedule: '',
  };
  const session = sessionFor((request, path) => {
    if (path === '/api/v1/operations/status') {
      return Response.json({ lanes: completed ? lanes(jobLane('message_embedding')) : lanes() });
    }
    if (path === '/api/v1/sources/status') return Response.json({ sources: [sourceStatus()] });
    if (path === '/api/v1/carddav/connections') return Response.json({ connections: [{ connection: 'default', status: cardDAVStatus }] });
    if (path === '/api/v1/carddav/status') return Response.json(cardDAVStatus);
    if (path === '/api/v1/carddav/books') return Response.json({ books: [] });
    if (path === '/api/v1/carddav/runs') return Response.json({ runs: [] });
    if (path === '/api/v1/carddav/conflicts') return Response.json({ conflicts: [] });
    if (path === '/api/v1/search/coverage') return Response.json({
      eligible_count: 10, embedded_count: completed && workspace !== 'Messages' ? 10 : 0,
      percentage: completed && workspace !== 'Messages' ? 100 : 0, cache_revision: 'cache-1',
      status: completed && workspace !== 'Messages' ? 'ready' : 'initializing', actions: completed ? [] : ['build_index'],
    });
    if (request.method === 'POST' && path === '/api/v1/carddav/sync') {
      completed = true;
      return Response.json({ status: 'failed', connections: [{ connection: 'default', status: 'failed' }] });
    }
    if (request.method === 'POST' && path === '/api/v1/sync/archive%40example.com') {
      completed = true;
      return Response.json({ message: 'accepted' }, { status: 202 });
    }
    if (request.method === 'POST' && path === '/api/v1/cli/run') {
      completed = true;
      return new Response(workspace === 'Messages' ? '{"type":"complete","error":"Embedding endpoint failed"}\n' : '{"type":"complete"}\n');
    }
  });
  render(App, { session });
  await screen.findByRole('button', { name: 'Sync status: Idle' });
  if (contacts) await fireEvent.click(await screen.findByRole('button', { name: 'Contacts' }));
  const trigger = await screen.findByRole('button', { name: action });
  await waitFor(() => expect(trigger.matches(':disabled')).toBe(false));
  await fireEvent.click(trigger);
  if (build) await fireEvent.click(screen.getByRole('button', { name: 'Confirm full rebuild' }));
  await screen.findByRole('button', { name: 'Sync status: Needs attention' });
});
