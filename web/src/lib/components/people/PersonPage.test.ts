import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, expect, it, vi } from 'vitest';
import { DirectoryController } from '../../directory/controller.svelte';
import { createAPIClient } from '../../api/client';
import { RelationshipsController, type RelationshipsMergeContext } from '../../relationships/controller.svelte';
import type { RelationshipCalendar } from '../../relationships/models';
import { meetingFixtureResponse } from '../../meetings/fixtures.test-support';
import PersonPage from './PersonPage.svelte';
import { openTypeahead } from '../../../test/kit-ui';

const when = '2026-07-19T10:00:00Z';
afterEach(() => { window.history.replaceState(null, '', '/'); vi.unstubAllEnvs(); });

it('forwards visual search setup from files for a person', async () => {
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/participants/1') return Response.json({ id: 1, display_label: 'Alice Example', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] });
    if (path.endsWith('/files/search')) return Response.json({ files: [], total_count: 0, cache_revision: 'cache-rel', search_provenance: {} });
    return meetingFixtureResponse(path) ?? Response.json({ rows: [] });
  });
  const controller = new RelationshipsController(client, () => 'UTC');
  await controller.openTarget('cluster:1', {}, false);
  const onConfigureVisualSearch = vi.fn();
  const view = render(PersonPage, { props: { client, directory: new DirectoryController(client), controller, onReconcilePersonMerge: (context: RelationshipsMergeContext) => controller.reconcilePersonMerge(context), target: 'cluster:1', filesOpen: true, predicate: {}, onFilesToggle: vi.fn(), onFiltersChange: vi.fn(), onConfigureVisualSearch } });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: 'Set up' }));
    expect(onConfigureVisualSearch).toHaveBeenCalledTimes(1);
  } finally { view.unmount(); controller.destroy(); }
});

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
  const view = render(PersonPage, { props: { client, directory: new DirectoryController(client), controller, onReconcilePersonMerge: (context: RelationshipsMergeContext) => controller.reconcilePersonMerge(context), target: 'cluster:1', filesOpen: false, predicate, onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await screen.findByText('Original message');
    await controller.linkParticipants(1, 2);
    await screen.findByText('Linked message');
    linked = false;
    await controller.reconcilePersonMerge(controller.personMergeContextSnapshot());
    await screen.findByText('Original message');
  } finally { view.unmount(); controller.destroy(); }
});

it('opening Files drops the selected day from the conversation reader', async () => {
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
  const props = { client, directory: new DirectoryController(client), controller, onReconcilePersonMerge: (context: RelationshipsMergeContext) => controller.reconcilePersonMerge(context), target: 'cluster:1', filesOpen: false, predicate: {}, onFilesToggle: vi.fn(), onFiltersChange: vi.fn() };
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

it('shows Addresses only on the person Details tab', async () => {
  vi.stubEnv('TZ', 'UTC');
  const summary = { id: 1, display_label: 'Alice Example', identifiers: [], activity_count: 9, file_count: 2, first_at: '2026-06-01T12:00:00Z', last_at: when, source_counts: [], cache_revision: 'cache-detail' };
  const target = 'cluster:1';
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/participants/1') return Response.json(summary);
    if (path.endsWith('/files/search')) return Response.json({ files: [], total_count: 0, cache_revision: 'cache-detail', search_provenance: {} });
    return Response.json({ rows: [], total_count: 0, cache_revision: 'cache-detail', search_provenance: {} });
  });
  const controller = new RelationshipsController(client, () => 'UTC');
  const directory = new DirectoryController(client);
  await controller.openTarget(target, {}, false);
  const view = render(PersonPage, { props: { client, directory, controller, target, filesOpen: false, predicate: {}, onFilesToggle: vi.fn(), onFiltersChange: vi.fn(), onReconcilePersonMerge: context => controller.reconcilePersonMerge(context) } });
  try {
    expect(screen.queryByRole('heading', { name: 'Addresses' })).toBeNull();
    await fireEvent.click(screen.getByRole('radio', { name: 'Details' }));
    expect(screen.getByRole('heading', { name: 'Addresses' })).toBeDefined();
    await fireEvent.click(screen.getByRole('radio', { name: 'Files' }));
    await view.rerender({ filesOpen: true });
    expect(screen.queryByRole('heading', { name: 'Addresses' })).toBeNull();
    await fireEvent.click(screen.getByRole('radio', { name: 'Messages' }));
    await view.rerender({ filesOpen: false });
    expect(screen.queryByRole('heading', { name: 'Addresses' })).toBeNull();
  } finally { view.unmount(); controller.destroy(); directory.destroy(); }
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
  const view = render(PersonPage, { props: { client, directory: new DirectoryController(client), controller, onReconcilePersonMerge: (context: RelationshipsMergeContext) => controller.reconcilePersonMerge(context), target: 'cluster:1', filesOpen: false, predicate, onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await screen.findByRole('heading', { name: 'Alice Example' });
    await fireEvent.click(screen.getByRole('radio', { name: 'Details' }));
    expect(await screen.findByText('Filtered activity is unavailable.')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Retry' })).toBeDefined();
  } finally { view.unmount(); controller.destroy(); }
});

it('keeps Back to People available after an observed person fails to load', async () => {
  const client = createAPIClient(async () => Response.json({ error: 'internal_error', message: 'Could not load this person.' }, { status: 500 }));
  const controller = new RelationshipsController(client, () => 'UTC');
  await controller.openTarget('cluster:1', {}, false);
  const onBack = vi.fn();
  const view = render(PersonPage, { props: { client, directory: new DirectoryController(client), controller, target: 'cluster:1', filesOpen: false, predicate: {}, onFilesToggle: vi.fn(), onFiltersChange: vi.fn(), onReconcilePersonMerge: context => controller.reconcilePersonMerge(context), onBack } });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: 'Back to People' }));
    expect(onBack).toHaveBeenCalledOnce();
  } finally { view.unmount(); controller.destroy(); }
});

it('keeps an open field draft when the saved person activity finishes loading', async () => {
  const person = { id: 7, revision: 1, display_name: 'Saved Person', participant_ids: [1], vcard_uid: 'saved-7', created_at: when, updated_at: when };
  let release!: (response: Response) => void;
  const pending = new Promise<Response>(resolve => release = resolve);
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input); const path = new URL(request.url).pathname;
    if (path === '/api/v1/participants/1') return pending;
    if (path === '/api/v1/people/7') return Response.json(person, { headers: { ETag: '"person-7-r1"' } });
    if (path.endsWith('/profile')) return Response.json({ person, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] }, { headers: { ETag: '"person-7-r1"' } });
    if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
    if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
    if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: when, stale: false });
    if (path.endsWith('/employments')) return Response.json({ employments: [] });
    if (path.endsWith('/organizations')) return Response.json({ organizations: [], total: 0, limit: 50, offset: 0 });
    if (path.endsWith('/relationships')) return Response.json({ relationships: [] });
    if (path.endsWith('/network')) return Response.json({ root_person_id: 7, depth: 1, nodes: [], edges: [], truncated: false });
    return meetingFixtureResponse(path) ?? Response.json({ rows: [] });
  });
  const directory = new DirectoryController(client); const controller = new RelationshipsController(client, () => 'UTC');
  await directory.loadSelection(7);
  const opening = controller.openTarget('cluster:1', {}, false);
  const view = render(PersonPage, { props: { client, directory, controller, durablePersonID: 7, target: 'cluster:1', filesOpen: false, predicate: {}, onFilesToggle: vi.fn(), onFiltersChange: vi.fn(), onReconcilePersonMerge: context => controller.reconcilePersonMerge(context) } });
  try {
    const addPhone = await screen.findByRole('button', { name: 'Add phone' });
    expect(addPhone).toHaveProperty('disabled', false);
    await fireEvent.click(addPhone);
    await fireEvent.input(screen.getByRole('textbox', { name: 'Contact value' }), { target: { value: '+1 202 555 0199' } });
    await act(async () => { release(Response.json({ id: 1, display_label: 'Saved Person', identifiers: [], profile: person, first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] })); await opening; });
    expect(screen.getByRole('radio', { name: 'Details' }).getAttribute('aria-checked')).toBe('true');
    expect(screen.getByRole('textbox', { name: 'Contact value' })).toHaveProperty('value', '+1 202 555 0199');
  } finally { view.unmount(); controller.destroy(); directory.destroy(); }
});

it('keeps the current person retry pair when an earlier Retry finishes late', async () => {
  const writes: Array<{ participant_a: number; participant_b: number }> = [];
  let release!: (response: Response) => void;
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (/\/participants\/[13]$/.test(path)) {
      const id = Number(path.split('/').at(-1));
      return Response.json({ id, display_label: `Observed ${id}`, identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] });
    }
    if (path.endsWith('/people/directory')) return Response.json({ people: [] });
    if (path.endsWith('/participants/search')) return Response.json({ rows: [2, 4].map(id => ({ id, display_label: `Peer ${id}`, identifiers: [], activity_count: 1 })) });
    if (path.endsWith('/identity/links')) {
      writes.push(await request.json());
      if (writes.length === 2) return new Promise<Response>(resolve => { release = resolve; });
      return Response.json({ identity_revision: writes.at(-1)!.participant_a === 3 ? 3 : 2, cache_state: 'stale' });
    }
    return meetingFixtureResponse(path) ?? Response.json({ rows: [] });
  });
  const controller = new RelationshipsController(client, () => 'UTC'), directory = new DirectoryController(client);
  const link = vi.spyOn(controller, 'linkParticipants');
  await controller.openTarget('cluster:1', {}, false);
  const props = { client, directory, controller, target: 'cluster:1', filesOpen: false, predicate: {}, onFilesToggle: vi.fn(), onFiltersChange: vi.fn(), onReconcilePersonMerge: vi.fn(async () => undefined) };
  const view = render(PersonPage, { props });
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'Merge with…' }));
    await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'Peer' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Peer 2/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    await screen.findByText('Identity saved; the cache refresh failed.');
    await fireEvent.click(within(screen.getByText('Identity saved; the cache refresh failed.')).getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(writes).toHaveLength(2));
    await controller.openTarget('cluster:3', {}, false);
    await view.rerender({ ...props, target: 'cluster:3' });
    await screen.findByRole('heading', { name: 'Observed 3' });
    await fireEvent.click(screen.getByRole('button', { name: 'Merge with…' }));
    await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'Peer' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Peer 4/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    await screen.findByText('Identity saved; the cache refresh failed.');
    await act(async () => { release(Response.json({ identity_revision: 2, cache_state: 'ready' })); await link.mock.results[1]!.value; });
    expect(screen.queryByText('Identity saved; the cache refresh failed.')).not.toBeNull();
    await fireEvent.click(within(screen.getByText('Identity saved; the cache refresh failed.')).getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(writes).toHaveLength(4));
    expect(writes.at(-1)).toEqual({ participant_a: 3, participant_b: 4 });
  } finally { view.unmount(); controller.destroy(); directory.destroy(); }
});

it('keeps Messages and its filters when a saved person has no contextual activity', async () => {
  const person = { id: 7, revision: 1, display_name: 'Saved Person', participant_ids: [1], vcard_uid: 'saved-7', created_at: when, updated_at: when };
  const summary = { id: 1, display_label: 'Saved Person', identifiers: [], profile: person, first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [], cache_revision: 'cache-1' };
  const predicate = { filters: [{ dimension: 'source' as const, values: ['999'] }], presentation: 'table' as const };
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input); const path = new URL(request.url).pathname;
    if (path === '/api/v1/participants/1') return Response.json(summary);
    if (path.endsWith('/summary')) return Response.json({ summary: { ...summary, activity_count: 0, first_at: '', last_at: '' } });
    if (path === '/api/v1/relationships/1/calendar') return Response.json({ participant_id: 1, canonical_id: 1, year: (await request.json()).year, timezone: 'UTC', days: [], annual: [], current: { temperature: 50, rank: 1, population: 1, raw_score: 1, signals: { sent_signal: 1, received_volume: 0, meeting_signal: 0, modalities: 1 } }, peak_temperature: 50, peak_year: 2026, scoring_timezone: 'UTC', score_version: 1, effective_date: '2026-07-19', cache_revision: 'cache-1', identity_revision: 1 });
    if (path.endsWith('/timeline')) return Response.json({ rows: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} });
    if (path === '/api/v1/people/7') return Response.json(person, { headers: { ETag: '"person-7-r1"' } });
    if (path.endsWith('/profile')) return Response.json({ person, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] }, { headers: { ETag: '"person-7-r1"' } });
    if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
    if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
    if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 1, computed_at: when, stale: false });
    if (path.endsWith('/employments')) return Response.json({ employments: [] });
    if (path.endsWith('/relationships')) return Response.json({ relationships: [] });
    if (path.endsWith('/network')) return Response.json({ root_person_id: 7, depth: 1, nodes: [], edges: [], truncated: false });
    return meetingFixtureResponse(path) ?? Response.json({ rows: [] });
  });
  const directory = new DirectoryController(client), controller = new RelationshipsController(client, () => 'UTC');
  await directory.loadSelection(7); await controller.openTarget('cluster:1', predicate, false);
  const onFiltersChange = vi.fn();
  const view = render(PersonPage, { props: { client, directory, controller, durablePersonID: 7, target: 'cluster:1', filesOpen: false, predicate, onFilesToggle: vi.fn(), onFiltersChange, onReconcilePersonMerge: context => controller.reconcilePersonMerge(context) } });
  try {
    expect(controller.detail?.activity_count).toBe(0);
    expect(controller.firstContactAt).toBe(when);
    expect(screen.getByRole('radio', { name: 'Messages' }).getAttribute('aria-checked')).toBe('true');
    expect(screen.getByRole('region', { name: 'Relationship activity calendar' })).toBeDefined();
    expect(await screen.findByText('No items match this view')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Clear filters' }));
    await waitFor(() => expect(onFiltersChange).toHaveBeenCalledWith([]));
  } finally { view.unmount(); controller.destroy(); directory.destroy(); }
});
