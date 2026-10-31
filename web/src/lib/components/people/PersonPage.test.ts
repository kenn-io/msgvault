import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { RelationshipsController, type RelationshipsMergeContext } from '../../relationships/controller.svelte';
import type { RelationshipCalendar } from '../../relationships/models';
import { meetingFixtureResponse } from '../../meetings/fixtures.test-support';
import PersonPage from './PersonPage.svelte';
import { openTypeahead } from '../../../test/kit-ui';

const when = '2026-07-19T10:00:00Z';
afterEach(() => { window.history.replaceState(null, '', '/'); vi.unstubAllEnvs(); });

it('refreshes the open person after linking and profile reconciliation', async () => {
  let linked = false;
  const routes: Record<string, (request: Request) => Response | Promise<Response>> = {
    '/api/v1/participants/1': () => Response.json({ id: 1, display_label: 'Alice Example', identifiers: [], first_at: '2025-01-01T00:00:00Z', last_at: when, activity_count: 2, file_count: 0, source_counts: [] }),
    '/api/v1/relationships/1/calendar': async request => Response.json({ participant_id: 1, canonical_id: 1, year: (await request.json()).year, timezone: 'UTC', current: { temperature: 50, rank: 1, population: 1, raw_score: 1, signals: { sent_signal: 1, received_volume: 0, meeting_signal: 0, modalities: 1 } }, peak_temperature: 50, peak_year: 2026, scoring_timezone: 'UTC', score_version: 1, effective_date: '2026-07-19', days: [], annual: [], identity_revision: linked ? 2 : 1, cache_revision: 'cache-rel' } satisfies RelationshipCalendar),
    '/api/v1/identity/links': () => { linked = true; return Response.json({ identity_revision: 2, cache_state: 'ready' }); },
    '/api/v1/participants/1/timeline': () => Response.json({ rows: [{ key: 'message:1', kind: 'email', match: {}, title: linked ? 'Linked message' : 'Original message', occurred_at: when, message_count: 1, participant_labels: [], participant_ids: [1], message_type: 'email', conversation_type: 'email', attachment_count: 0, attachment_size: 0, deleted_from_source: false, source_identifier: 'owner@example.com', source_type: 'gmail', source_id: 1 }], total_count: 1, cache_revision: 'cache-rel', search_provenance: {} })
  };
  const client = createAPIClient(async input => { const request = input instanceof Request ? input : new Request(input); const path = new URL(request.url).pathname; return routes[path]?.(request) ?? meetingFixtureResponse(path) ?? Response.json({ rows: [] }); });
  const controller = new RelationshipsController(client, () => 'UTC'); const predicate = { filters: [], presentation: 'table' as const };
  await controller.openTarget('cluster:1', predicate, false);
  const view = render(PersonPage, { props: { client, controller, onReconcilePersonMerge: (context: RelationshipsMergeContext) => controller.reconcilePersonMerge(context), target: 'cluster:1', filesOpen: false, predicate, onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await screen.findByText('Original message');
    await controller.linkParticipants(1, 2);
    await screen.findByText('Linked message');
    linked = false;
    await controller.reconcilePersonMerge(controller.personMergeContextSnapshot());
    await screen.findByText('Original message');
  } finally { view.unmount(); controller.destroy(); }
});

it('clearing a calendar day through Files clears the conversation reader bounds', async () => {
  const at = '2026-07-18T10:00:00Z';
  vi.stubEnv('TZ', 'UTC');
  const conversations: URL[] = [];
  const timelines: unknown[] = [];
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const url = new URL(request.url);
    if (url.pathname === '/api/v1/participants/1') return Response.json({ id: 1, display_label: 'Alice Example', identifiers: [], first_at: at, last_at: at, activity_count: 1, file_count: 0, source_counts: [], cache_revision: 'cache-1' });
    if (url.pathname === '/api/v1/relationships/1/calendar') return Response.json({ participant_id: 1, canonical_id: 1, year: 2026, timezone: 'UTC', current: { temperature: 50, rank: 1, population: 1, raw_score: 1, signals: { sent_signal: 1, received_volume: 0, meeting_signal: 0, modalities: 1 } }, peak_temperature: 50, peak_year: 2026, scoring_timezone: 'UTC', score_version: 1, effective_date: '2026-07-19', annual: [], identity_revision: 1, cache_revision: 'cache-1', days: [{ date: '2026-07-18', total: 1, email: 0, chat: 1, meetings: 0, sent: 0, received: 1, modality_mask: 2, level: 'FIRST_QUARTILE' }] });
    if (url.pathname === '/api/v1/participants/1/timeline') {
      timelines.push(await request.json());
      return Response.json({ rows: [{ key: 'conversation:2', kind: 'conversation', title: 'Saved conversation', occurred_at: at, first_at: at, message_type: 'slack', conversation_type: 'chat', source_id: 1, source_type: 'slack', source_identifier: 'owner@example.com', participant_labels: [], participant_ids: [], attachment_count: 0, attachment_size: 0, message_count: 1, deleted_from_source: false, conversation_id: 2, anchor_message_id: 2, match: {} }], total_count: 1, cache_revision: 'cache-1', search_provenance: {} });
    }
    if (url.pathname === '/api/v1/conversations/2') {
      conversations.push(url);
      return Response.json({ id: 2, messages: [{ id: 2, conversation_id: 2, subject: 'Saved conversation', sent_at: at, message_type: 'slack', from: 'Alice Example', to: [], body: 'Conversation body', snippet: 'Conversation body', attachments: [] }], anchor_id: 2, total: 1, has_before: false, has_after: false });
    }
    if (url.pathname === '/api/v1/cli/accounts') return Response.json({ accounts: [] });
    if (url.pathname.endsWith('/files/search')) return Response.json({ files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} });
    return meetingFixtureResponse(url.pathname) ?? Response.json({ rows: [] });
  });
  const controller = new RelationshipsController(client, () => 'UTC');
  await controller.openTarget('cluster:1', {}, false);
  const props = { client, controller, onReconcilePersonMerge: (context: RelationshipsMergeContext) => controller.reconcilePersonMerge(context), target: 'cluster:1', filesOpen: false, predicate: {}, onFilesToggle: vi.fn(), onFiltersChange: vi.fn() };
  const view = render(PersonPage, { props });
  try {
    await fireEvent.click((await screen.findAllByRole('button', { name: '1 message on Jul 18, 2026' }))[0]);
    const grid = await screen.findByRole('grid', { name: 'Messages results' });
    await waitFor(() => expect(grid.getAttribute('aria-busy')).toBe('false'));
    await fireEvent.click(await within(grid).findByText('Saved conversation'));
    await screen.findByText('Conversation body');
    expect(conversations.at(-1)?.searchParams.get('start')).toBe('2026-07-18T00:00:00.000Z');
    await fireEvent.click(screen.getByRole('radio', { name: 'Files' }));
    await view.rerender({ ...props, filesOpen: true });
    await screen.findByRole('grid', { name: 'Files results' });
    await fireEvent.click(screen.getByRole('radio', { name: 'Messages' }));
    await view.rerender(props);
    await screen.findByText('Conversation body');
    expect(conversations).toHaveLength(2);
    expect(screen.queryByRole('button', { name: 'Clear date' })).toBeNull();
    expect(timelines.at(-1)).toMatchObject({ filters: [] });
    expect(conversations.at(-1)?.searchParams.get('start')).toBeNull();
    expect(conversations.at(-1)?.searchParams.get('end')).toBeNull();
  } finally { view.unmount(); controller.destroy(); }
});

it('reports a filtered summary failure while retaining the loaded person', async () => {
  const predicate = { filters: [{ dimension: 'source' as const, values: ['2'] }] };
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/participants/1') return Response.json({ id: 1, display_label: 'Alice Example', identifiers: [], first_at: when, last_at: when, activity_count: 9, file_count: 0, source_counts: [] });
    if (path === '/api/v1/participants/1/summary') return Response.json({ error: 'analytical_cache_unavailable', readiness: 'stale', recovery_action: '', message: 'Filtered activity is unavailable.' }, { status: 503 });
    return meetingFixtureResponse(path) ?? Response.json({ rows: [] });
  });
  const controller = new RelationshipsController(client, () => 'UTC');
  await controller.openTarget('cluster:1', predicate, false);
  const view = render(PersonPage, { props: { client, controller, onReconcilePersonMerge: (context: RelationshipsMergeContext) => controller.reconcilePersonMerge(context), target: 'cluster:1', filesOpen: false, predicate, onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await screen.findByRole('heading', { name: 'Alice Example' });
    expect(controller.timelineError).toBe('Filtered activity is unavailable.');
    await fireEvent.click(screen.getByRole('radio', { name: 'Details' }));
    expect(await screen.findByText('Filtered activity is unavailable.')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Retry' })).toBeDefined();
  } finally { view.unmount(); controller.destroy(); }
});

it('keeps a stale identity mutation retry across Details and Messages', async () => {
  let links = 0;
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/participants/1') return Response.json({ id: 1, display_label: 'Alice Example', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] });
    if (path === '/api/v1/participants/search') return Response.json({ rows: [{ id: 2, display_label: 'Bob Example', identifiers: [], activity_count: 1, file_count: 0, source_counts: [], first_at: when, last_at: when }], total_count: 1, cache_revision: 'cache-1', search_provenance: {} });
    if (path === '/api/v1/identity/links') { links += 1; return Response.json({ identity_revision: 2, cache_state: 'stale' }); }
    return meetingFixtureResponse(path) ?? Response.json({ rows: [] });
  });
  const controller = new RelationshipsController(client, () => 'UTC');
  await controller.openTarget('cluster:1', {}, false);
  const view = render(PersonPage, { props: { client, controller, onReconcilePersonMerge: (context: RelationshipsMergeContext) => controller.reconcilePersonMerge(context), target: 'cluster:1', filesOpen: false, predicate: {}, onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await fireEvent.click(await screen.findByRole('radio', { name: 'Details' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Same person…' }));
    await fireEvent.input(await openTypeahead('Search people to link'), { target: { value: 'Bob' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Bob Example/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'These are the same person' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Identity saved; the cache refresh failed');
    await fireEvent.click(screen.getByRole('radio', { name: 'Messages' }));
    await fireEvent.click(screen.getByRole('radio', { name: 'Details' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Identity saved; the cache refresh failed');
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(links).toBe(2));
  } finally { view.unmount(); controller.destroy(); }
});
