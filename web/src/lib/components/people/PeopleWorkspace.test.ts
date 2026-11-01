import { openTypeahead } from '../../../test/kit-ui';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it, vi, onTestFinished } from 'vitest';
import { createAPIClient } from '../../api/client';
import { RelationshipsController } from '../../relationships/controller.svelte';
import { DirectoryController } from '../../directory/controller.svelte';
import { ExploreState } from '../../explore/state.svelte';
import { meetingFixtureResponse } from '../../meetings/fixtures.test-support';
import PeopleWorkspace from './PeopleWorkspace.svelte';
import PersonPage from './PersonPage.svelte';

vi.mock('./PersonPage.svelte', { spy: true });

const when = '2026-07-19T10:00:00Z';
function clientFor(routes: Record<string, (request: Request) => Promise<Response> | Response>, requests?: string[]) {
  return createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    const personID = Number(path.match(/\/people\/(\d+)\//)?.[1]);
    requests?.push(path);
    return routes[path]?.(request) ?? (path.endsWith('/contact-state') ? Response.json({ person_id: personID, cadence_status: 'unknown', interaction_count: 0 }) : undefined) ?? (path.endsWith('/network') ? Response.json({ root_person_id: personID, depth: 1, nodes: [], edges: [], truncated: false }) : undefined) ?? meetingFixtureResponse(path) ?? Response.json({ rows: [] });
  });
}
const profile = (id: number, revision: number, participant_ids: number[]) => ({ id, revision, participant_ids, display_name: id === 7 ? 'Synthetic One' : 'Synthetic Two', created_at: when, updated_at: when, vcard_uid: `synthetic-${id}` });

it('retains the day and file filters between tabs and keeps Details selected when resized', async () => {
  const observers = new Map<Element, ResizeObserverCallback>();
  vi.stubGlobal('ResizeObserver', class {
    constructor(private callback: ResizeObserverCallback) {}
    observe(target: Element) { observers.set(target, this.callback); }
    unobserve(target: Element) { observers.delete(target); }
    disconnect() {}
  });
  onTestFinished(() => { vi.unstubAllGlobals(); });
  const client = clientFor({
    '/api/v1/participants/1': () => Response.json({ id: 1, display_label: 'Alice Example', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [], cache_revision: 'cache-1' }),
    '/api/v1/relationships/1/calendar': async request => Response.json({ participant_id: 1, canonical_id: 1, year: (await request.json()).year, timezone: 'UTC', current: { temperature: 50, rank: 1, population: 1, raw_score: 1, signals: { sent_signal: 1, received_volume: 0, meeting_signal: 0, modalities: 1 } }, peak_temperature: 50, peak_year: 2026, scoring_timezone: 'UTC', score_version: 1, effective_date: '2026-07-19', annual: [], identity_revision: 1, cache_revision: 'cache-1', days: [{ date: '2026-07-19', total: 1, email: 1, chat: 0, meetings: 0, sent: 1, received: 0, modality_mask: 1, level: 'FIRST_QUARTILE' }] }),
    '/api/v1/participants/1/files/search': () => Response.json({ files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} }),
  });
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipTarget: 'cluster:1' });
  await controller.openTarget('cluster:1', {}, false);
  const props = { client, controller, directory, urlState: state.current, target: 'cluster:1', filesOpen: true, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() };
  const view = render(PeopleWorkspace, { props });
  try {
    await fireEvent.input(await screen.findByRole('searchbox', { name: 'Filter filename' }), { target: { value: 'report' } });
    await fireEvent.click(screen.getByRole('checkbox', { name: 'PDFs' }));
    await fireEvent.click(screen.getByRole('radio', { name: 'Messages' }));
    await view.rerender({ filesOpen: false });
    await fireEvent.click((await screen.findAllByRole('button', { name: '1 message on Jul 19, 2026' }))[0]);
    await screen.findByRole('button', { name: 'Clear date' });
    await fireEvent.click(screen.getByRole('radio', { name: 'Details' }));
    const root = screen.getByRole('main', { name: 'People' });
    for (const width of [640, 1200]) {
      await act(() => observers.get(root)?.([{ target: root, contentRect: { width } } as unknown as ResizeObserverEntry], {} as ResizeObserver));
      expect.soft(screen.getByRole('radio', { name: 'Details' }).getAttribute('aria-checked')).toBe('true');
    }
    await fireEvent.click(screen.getByRole('radio', { name: 'Messages' }));
    expect.soft(screen.queryByRole('button', { name: 'Clear date' })).not.toBeNull();
    await fireEvent.click(screen.getByRole('radio', { name: 'Files' }));
    await view.rerender({ filesOpen: true });
    expect.soft((await screen.findByRole('searchbox', { name: 'Filter filename' }) as HTMLInputElement).value).toBe('report');
    expect.soft((screen.getByRole('checkbox', { name: 'PDFs' }) as HTMLInputElement).checked).toBe(false);
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it.each(['sort', 'filter'])('Escape dismisses the People %s before leaving the open person', async (control) => {
  const client = clientFor({
    '/api/v1/relationships': () => Response.json({ rows: [] }),
    '/api/v1/participants/1': () => Response.json({ id: 1, display_label: 'Alice Example', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }),
    '/api/v1/relationships/1/calendar': async request => Response.json({ year: (await request.json()).year, days: [] }),
  });
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipTarget: 'cluster:1' });
  await controller.openTarget('cluster:1', {}, false);
  const onNavigate = vi.fn();
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: state.current, target: 'cluster:1', filesOpen: false, predicate: {}, onNavigate, onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    const trigger = control === 'sort' ? await screen.findByRole('combobox', { name: 'Sort: Closest' }) : within(screen.getByRole('complementary', { name: 'People' })).getByRole('button', { name: 'Filter' });
    await fireEvent.click(trigger);
    if (control === 'sort') await screen.findByRole('listbox');
    else await screen.findByPlaceholderText('Find a filter');
    await fireEvent.keyDown(trigger, { key: 'Escape' });
    expect(onNavigate).not.toHaveBeenCalled();
    await waitFor(() => expect(screen.queryByRole('listbox')).toBeNull());
    await waitFor(() => expect(screen.queryByPlaceholderText('Find a filter')).toBeNull());
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it.each(['fails', 'is cancelled', 'refreshes the cache', 'merges'])('reports the result when the next pair %s', async (outcome) => {
  let calls = 0; let listRequests = 0; let releaseReconcile: (() => void) | undefined;
  const links: Array<{ participant_a: number; participant_b: number }> = [];
  const client = clientFor({
    '/api/v1/relationships': async () => {
      if (++listRequests === 2 && outcome === 'merges') await new Promise<void>(resolve => { releaseReconcile = resolve; });
      return Response.json({ rows: ['Alice Example', 'Bob Example', 'Cara Example'].map((display_label, index) => ({ canonical_id: index + 1, display_label, last_at: when, member_ids: [index + 1], score: 2, signals: { last_interaction_at: when, meeting_count: 0, meetings_together: 0, modalities: 2, received_from_them: 1, sent_count: 3, sent_to_them: 1 } })) });
    },
    '/api/v1/identity/links': async request => {
      links.push(await request.json()); calls += 1;
      if (outcome === 'merges') return calls === 1 ? Response.json({ error: 'person_merge_required', message: 'Choose a survivor', profiles: [{ etag: '"person-7-r4"', person: profile(7, 4, [1]) }, { etag: '"person-9-r2"', person: profile(9, 2, [2]) }] }, { status: 409 }) : Response.json({ identity_revision: 5, cache_state: 'ready' });
      return calls === 1 || outcome === 'refreshes the cache' ? Response.json({ identity_revision: 5, cache_state: calls === 1 && outcome === 'refreshes the cache' ? 'stale' : 'ready' }) : outcome === 'fails' ? Response.json({ error: 'internal_error', message: 'Synthetic link failed' }, { status: 500 }) : Response.json({ error: 'person_merge_required', message: 'Choose a survivor', profiles: [{ etag: '"person-7-r4"', person: profile(7, 4, [1, 2]) }, { etag: '"person-9-r2"', person: profile(9, 2, [3]) }] }, { status: 409 });
    },
    '/api/v1/people/7/merge': () => Response.json({ cache_state: 'ready', identity_revision: 8, review_candidates: [], person: profile(7, 5, [1, 2]), merge: { id: 41, survivor_person_id: 7, absorbed_person_id: 9, current_person_id: 7, survivor_vcard_uid: 'synthetic-7', absorbed_vcard_uid: 'synthetic-9', survivor_revision_before: 4, absorbed_revision_before: 2, survivor_revision_after: 5, actor: 'web', snapshot_version: 1, snapshot_sha256: 'synthetic-digest', created_at: when } }, { headers: { ETag: '"person-7-r5"' } })
  });
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: state.current, target: null, filesOpen: false, predicate: { filters: [], presentation: 'table' }, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    for (const name of ['Alice Example', 'Bob Example', 'Cara Example']) await fireEvent.click(await screen.findByRole('checkbox', { name: `Select ${name}` }));
    await fireEvent.click(screen.getByRole('button', { name: 'Same person' }));
    if (outcome === 'merges') {
      await screen.findByRole('dialog', { name: 'Resolve person merge' });
      await fireEvent.click(screen.getByRole('radio', { name: 'Synthetic One (Person 7)' }));
      await fireEvent.click(screen.getByRole('checkbox', { name: /I understand this consolidates both profiles/i }));
      await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
      await waitFor(() => expect(releaseReconcile).toBeDefined());
      expect(screen.queryByRole('dialog', { name: 'Resolve person merge' })).toBeNull();
      expect(screen.getByRole('checkbox', { name: 'Unselect Bob Example' }).closest('.list-content')).toHaveProperty('inert', true);
      await fireEvent.click(screen.getByRole('button', { name: 'Same person' }));
      expect(links).toHaveLength(1);
      releaseReconcile?.();
      expect(await screen.findByText('People are now treated as the same person.')).toBeDefined();
      expect(links).toEqual([{ participant_a: 1, participant_b: 2 }, { participant_a: 1, participant_b: 2 }, { participant_a: 1, participant_b: 3 }]);
    } else if (outcome === 'is cancelled') {
      await screen.findByRole('dialog', { name: 'Resolve person merge' });
      await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    } else if (outcome === 'fails') await screen.findByText(/Synthetic link failed/);
    else {
      await waitFor(() => expect(screen.queryByRole('button', { name: 'Clear selection' })).toBeNull());
      expect(screen.queryByRole('button', { name: 'Retry identity cache' })).toBeNull();
    }
    if (outcome === 'fails' || outcome === 'is cancelled') expect(screen.getByRole('status').textContent).toContain('1 of 2');
    expect(calls).toBe(outcome === 'merges' ? 3 : 2);
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it.each(['Category', 'Organization'])('forwards typed %s profile filters as drafts', async (field) => {
  const client = clientFor({});
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  const onNavigate = vi.fn(); const onDraftNavigation = vi.fn();
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: { ...state.current, directoryContactState: 'active' }, target: null, filesOpen: false, predicate: {}, onNavigate, onReplaceNavigation: vi.fn(), onDraftNavigation, onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: /Filter/ }));
    await fireEvent.click(await screen.findByText('Profile filters'));
    const input = await screen.findByLabelText(`${field} filter`);
    for (const value of ['a', 'ac', 'acme']) await fireEvent.input(input, { target: { value } });
    expect(onNavigate).not.toHaveBeenCalled();
    expect(onDraftNavigation).toHaveBeenLastCalledWith({ [field === 'Category' ? 'directoryCategory' : 'directoryOrganization']: 'acme' });
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('resetting People filters commits one complete navigation', async () => {
  const client = clientFor({});
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  const onNavigate = vi.fn(); const onShowAllChange = vi.fn();
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: { ...state.current, relationshipSaved: true, relationshipShowAll: true, directoryCategory: 'acme' }, target: null, filesOpen: false, predicate: {}, onNavigate, onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange, onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: /Filter/ }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Clear filters' }));
    expect(onNavigate.mock.calls.length + onShowAllChange.mock.calls.length).toBe(1);
    expect(onNavigate).toHaveBeenCalledExactlyOnceWith({ relationshipSaved: false, relationshipShowAll: false, directoryContactState: '', directoryCategory: '', directoryOrganization: '', directoryPrimaryChannel: '', directoryLastContactAfter: '', directoryLastContactBefore: '' });
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('reads a saved profile once and loads its fields when Details opens', async () => {
  const paths: string[] = [];
  const profile = { id: 91, display_name: 'Saved Example', participant_ids: [1], organizations: [], categories: [], contact_state: 'active', revision: 1, vcard_uid: 'example-91', created_at: '2026-07-18T12:00:00Z', updated_at: '2026-07-18T12:00:00Z' };
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    paths.push(path);
    if (path === '/api/v1/people/directory') return Response.json({ people: [profile] });
    if (path === '/api/v1/participants/1') return Response.json({ id: 1, display_label: 'Saved Example', profile, identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] });
    if (path === '/api/v1/relationships/1/calendar') return Response.json({ year: 2026, days: [] });
    if (path === '/api/v1/people/91') return Response.json(profile);
    if (path === '/api/v1/people/91/contact-state') return Response.json({ person_id: 91, cadence_status: 'overdue', interaction_count: 3 });
    if (path.endsWith('/network')) return Response.json({ root_person_id: 91, depth: 1, nodes: [], edges: [], truncated: false });
    return meetingFixtureResponse(path) ?? Response.json({ rows: [] });
  });
  const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true, relationshipTarget: 'cluster:1' });
  const directory = new DirectoryController(client);
  const controller = new RelationshipsController(client, () => 'UTC');
  await controller.openTarget('cluster:1', {}, false);
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: state.current, target: 'cluster:1', filesOpen: false, predicate: {}, onNavigate: patch => state.commitNavigation(patch), onReplaceNavigation: patch => state.replaceCommittedNavigation(patch), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await screen.findByRole('heading', { name: 'Saved Example' });
    await waitFor(() => expect(directory.detailLoading).toBe(false));
    expect(paths.filter(path => path === '/api/v1/people/91')).toHaveLength(1);
    expect(paths).toContain('/api/v1/people/91/profile');
    expect(paths.filter(path => /people-inference|attribute-definitions|kata|agenda|brief/.test(path))).toEqual([]);
    await fireEvent.click(screen.getByRole('radio', { name: 'Details' }));
    expect(await screen.findByRole('button', { name: 'Add phone' })).toBeDefined();
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('names an empty Domains list for the selected facet', async () => {
  const client = clientFor({});
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: { ...state.current, relationshipFacet: 'domains' }, target: null, filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try { expect(await screen.findByText('No domains yet')).toBeDefined(); }
  finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('opens the checkbox-clicked person when Enter follows the selection', async () => {
  const client = clientFor({ '/api/v1/relationships': () => Response.json({ rows: ['Alice Example', 'Bob Example'].map((display_label, index) => ({ canonical_id: index + 1, display_label, last_at: when, member_ids: [index + 1], score: 2, signals: { last_interaction_at: when, meeting_count: 0, meetings_together: 0, modalities: 2, received_from_them: 1, sent_count: 3, sent_to_them: 1 } })) }) });
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  const onNavigate = vi.fn();
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate, onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    const checkbox = await screen.findByRole('checkbox', { name: 'Select Bob Example' });
    await fireEvent.pointerDown(checkbox);
    await fireEvent.click(checkbox);
    await fireEvent.keyDown(screen.getByRole('grid', { name: 'People results' }), { key: 'Enter' });
    expect(onNavigate).toHaveBeenCalledWith({ directoryPersonID: null, relationshipTarget: 'cluster:2', relationshipFiles: false });
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it.each([false, true])('keeps files from every bound cluster when the first saved binding is draft-only: %s', async (draftOnly) => {
  const at = '2026-07-19T10:00:00Z';
  const profile = { id: 91, display_name: 'Saved Example', participant_ids: [1, 2], organizations: [], categories: [], contact_state: 'active', revision: 1, vcard_uid: 'example-91', created_at: at, updated_at: at };
  const file = (id: number, filename: string) => ({ id, key: `file:${id}`, entry_key: `message:${id}`, message_id: id, conversation_id: id, occurred_at: at, source_id: 1, source_type: 'gmail', source_identifier: 'owner@example.com', containing_title: 'Archived message', filename, mime_type: 'application/pdf', mime_family: 'pdf', size_bytes: 123, participant_labels: ['Saved Example'], participant_domains: ['example.com'], content_state: 'metadata_only', content_available: false, person_provenance: { participant_ids: [id], roles: ['from'], directions: ['from_person'] } });
  const first = file(1, 'first-address.pdf');
  const second = file(2, 'second-address.pdf');
  const requests: string[] = [];
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    requests.push(path);
    if (path === '/api/v1/people/directory') return Response.json({ people: [profile] });
    if (path === '/api/v1/people/91') return Response.json(profile);
    if (path === '/api/v1/participants/1' && draftOnly) return Response.json({ error: 'not_found' }, { status: 404 });
    if (path === '/api/v1/participants/1') return Response.json({ id: 1, display_label: 'Saved Example', identifiers: [], first_at: at, last_at: at, activity_count: 1, file_count: 1, source_counts: [] });
    if (path === '/api/v1/relationships/1/calendar') return Response.json({ year: 2026, days: [] });
    if (path === '/api/v1/participants/1/files/search') return Response.json({ files: [first], total_count: 1, cache_revision: 'cache-1', search_provenance: {} });
    if (path === '/api/v1/people/91/files/search') return Response.json({ files: draftOnly ? [second] : [first, second], total_count: draftOnly ? 1 : 2, cache_revision: 'cache-1', search_provenance: {} });
    if (path.endsWith('/network')) return Response.json({ root_person_id: 91, depth: 1, nodes: [], edges: [], truncated: false });
    return meetingFixtureResponse(path) ?? Response.json({ rows: [] });
  });
  const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true, directoryPersonID: 91, relationshipTarget: 'cluster:1', relationshipFiles: true });
  const directory = new DirectoryController(client);
  const controller = new RelationshipsController(client, () => 'UTC');
  await controller.openTarget('cluster:1', {}, false);
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: state.current, target: 'cluster:1', filesOpen: true, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    if (!draftOnly) await screen.findByText('first-address.pdf');
    await waitFor(() => expect(screen.queryByText('second-address.pdf'), requests.join(', ')).not.toBeNull());
    expect(requests).toContain('/api/v1/people/91/files/search');
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('refreshes the selected survivor and Saved list after merging', async () => {
  let merged = false;
  const one = { ...profile(7, 4, [1]), organizations: [], categories: [], contact_state: 'active' };
  const two = { ...profile(9, 2, [2]), organizations: [], categories: [], contact_state: 'active' };
  const client = clientFor({
    '/api/v1/people/directory': () => Response.json({ people: merged ? [one] : [one, two] }),
    '/api/v1/people/7': () => Response.json(one),
    '/api/v1/people/7/profile': () => Response.json({ person: one, names: [], addresses: [], dates: [], categories: [], media: [], contact_points: merged ? [{ person_id: 7, address_kind: 'email', original_value: 'absorbed@example.com', normalized_value: 'absorbed@example.com', envelope: { id: 11, ordinal: 0, source: 'user', vcard: {}, created_at: when, updated_at: when } }] : [] }),
    '/api/v1/participants/1': () => Response.json({ id: 1, profile: one, display_label: 'Synthetic One', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }),
    '/api/v1/participants/search': () => Response.json({ rows: [{ id: 2, display_label: 'Bob Example', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }], total_count: 1, cache_revision: 'cache-1', search_provenance: {} }),
    '/api/v1/identity/links': () => merged ? Response.json({ identity_revision: 8, cache_state: 'ready' }) : Response.json({ error: 'person_merge_required', message: 'Choose a survivor', profiles: [{ etag: '"person-7-r4"', person: one }, { etag: '"person-9-r2"', person: two }] }, { status: 409 }),
    '/api/v1/people/7/merge': () => { merged = true; return Response.json({ cache_state: 'ready', identity_revision: 8, review_candidates: [], person: { ...one, revision: 5, participant_ids: [1, 2] }, merge: { id: 41, survivor_person_id: 7, absorbed_person_id: 9, current_person_id: 7, survivor_vcard_uid: 'synthetic-7', absorbed_vcard_uid: 'synthetic-9', survivor_revision_before: 4, absorbed_revision_before: 2, survivor_revision_after: 5, actor: 'web', snapshot_version: 1, snapshot_sha256: 'synthetic-digest', created_at: when } }, { headers: { ETag: '"person-7-r5"' } }); },
  });
  const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true, relationshipTarget: 'cluster:1' });
  const directory = new DirectoryController(client);
  const controller = new RelationshipsController(client, () => 'UTC');
  await controller.openTarget('cluster:1', {}, false);
  const onOpenDirectoryPerson = vi.fn();
  const props = { client, controller, directory, urlState: state.current, target: 'cluster:1', filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn(), onOpenDirectoryPerson };
  const view = render(PeopleWorkspace, { props });
  try {
    await within(await screen.findByRole('grid', { name: 'People results' })).findByText('Synthetic Two');
    await waitFor(() => expect(directory.detailLoading).toBe(false));
    await fireEvent.click(await screen.findByRole('radio', { name: 'Details' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Same person…' }));
    const input = await openTypeahead('Search people to link');
    await fireEvent.input(input, { target: { value: 'Bob' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Bob Example/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'These are the same person' }));
    await screen.findByRole('dialog', { name: 'Resolve person merge' });
    await fireEvent.click(screen.getByRole('radio', { name: 'Synthetic One (Person 7)' }));
    await fireEvent.click(screen.getByRole('checkbox', { name: /I understand this consolidates both profiles/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
    await waitFor(() => expect(merged).toBe(true));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Resolve person merge' })).toBeNull());
    expect(onOpenDirectoryPerson).toHaveBeenCalledWith(7);
    await screen.findByText('absorbed@example.com');
    await waitFor(() => expect(within(screen.getByRole('grid', { name: 'People results' })).queryByText('Synthetic Two')).toBeNull());
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('loads a saved person deep link inside its person page', async () => {
  let release!: (response: Response) => void;
  const pending = new Promise<Response>(resolve => release = resolve);
  const client = clientFor({
    '/api/v1/people/7': () => pending,
  });
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', directoryPersonID: 7 });
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await screen.findByText('Loading details…');
    expect(screen.getByRole('group', { name: 'Person page' })).toBeDefined();
    release(Response.json(profile(7, 1, [])));
    await screen.findByRole('heading', { name: 'Synthetic One' });
    expect(await screen.findByRole('heading', { name: 'Meeting activity and follow-ups' })).toBeDefined();
    expect(screen.getByRole('group', { name: 'Person page' })).toBeDefined();
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('ignores a completed saved profile lookup after an observed person supersedes it', async () => {
  const saved = { ...profile(92, 1, []), display_name: 'Saved Contact', organizations: [], categories: [], contact_state: 'active' };
  let release!: (response: Response) => void;
  const pending = new Promise<Response>(resolve => release = resolve);
  const client = clientFor({
    '/api/v1/people/directory': () => Response.json({ people: [saved] }),
    '/api/v1/people/92': () => pending,
    '/api/v1/relationships': () => Response.json({ rows: [{ canonical_id: 1, display_label: 'Alice Example', last_at: when, member_ids: [1], score: 2, signals: { last_interaction_at: when, meeting_count: 0, meetings_together: 0, modalities: 2, received_from_them: 1, sent_count: 3, sent_to_them: 1 } }] }),
  });
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true });
  const onNavigate = vi.fn((patch) => state.commitNavigation(patch));
  const props = () => ({ client, controller, directory, urlState: state.current, target: state.current.relationshipTarget, filesOpen: false, predicate: {}, onNavigate, onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() });
  const view = render(PeopleWorkspace, { props: props() });
  try {
    await fireEvent.click(await screen.findByText('Saved Contact'));
    await fireEvent.click(screen.getByRole('button', { name: 'Remove Saved' }));
    await view.rerender(props());
    await fireEvent.click(await screen.findByText('Alice Example'));
    await view.rerender(props());
    await act(async () => { release(Response.json(saved)); await pending; await new Promise(resolve => setTimeout(resolve, 0)); });
    expect(state.current).toMatchObject({ directoryPersonID: null, relationshipTarget: 'cluster:1' });
    expect(onNavigate.mock.calls).toEqual([[{ directoryPersonID: 92, relationshipTarget: null, relationshipFiles: false }], [{ relationshipSaved: false }], [{ directoryPersonID: null, relationshipTarget: 'cluster:1', relationshipFiles: false }]]);
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it.each([
  [{ identityQuery: 'after' }, 'q', 'after'],
  [{ relationshipSort: 'recent' as const }, 'sort', 'last_contact_desc'],
] as const)('replaces Saved results when its inputs change: %s', async (patch, parameter, value) => {
  const reads: URL[] = [];
  const client = clientFor({ '/api/v1/people/directory': request => {
    const url = new URL(request.url); reads.push(url);
    return Response.json({ people: [{ ...profile(7, 1, []), display_name: url.searchParams.get(parameter) === value ? 'After' : 'Before', organizations: [], categories: [], contact_state: 'active' }] });
  } });
  const state = new ExploreState(null); state.commitNavigation({ workspace: 'relationships', relationshipSaved: true });
  const directory = new DirectoryController(client); const controller = new RelationshipsController(client, () => 'UTC');
  const props = { client, controller, directory, urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() };
  const view = render(PeopleWorkspace, { props });
  try {
    await screen.findByText('Before');
    await view.rerender({ ...props, urlState: { ...state.current, ...patch } });
    await screen.findByText('After');
    expect(screen.queryByText('Before')).toBeNull();
    expect(reads.at(-1)?.searchParams.get(parameter)).toBe(value);
  } finally { view.unmount(); directory.destroy(); controller.destroy(); state.destroy(); }
});


it.each([false, undefined])('forwards sweep configuration and settings navigation to the person page: %s', async sweepConfigured => {
  vi.mocked(PersonPage).mockClear();
  const client = clientFor({ '/api/v1/people/7': () => Response.json(profile(7, 1, [])) });
  const state = new ExploreState(null); state.commitNavigation({ workspace: 'relationships', directoryPersonID: 7 });
  const directory = new DirectoryController(client); const controller = new RelationshipsController(client, () => 'UTC');
  const onOpenSettings = vi.fn();
  const view = render(PeopleWorkspace, { props: { client, controller, directory, sweepConfigured, onOpenSettings, urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await waitFor(() => expect(PersonPage).toHaveBeenCalledWith(expect.anything(), expect.objectContaining({ sweepConfigured, onOpenSettings })));
  } finally { view.unmount(); directory.destroy(); controller.destroy(); state.destroy(); }
});

it('keeps Files and its filters when a saved person finishes initial hydration', async () => {
  let releaseProfile!: (response: Response) => void;
  const pendingProfile = new Promise<Response>(resolve => releaseProfile = resolve);
  const saved = profile(7, 1, [1]);
  const client = clientFor({
    '/api/v1/people/7': () => pendingProfile,
    '/api/v1/people/7/profile': () => Response.json({ person: saved, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] }),
    '/api/v1/people/7/attributes': () => Response.json({ person_id: 7, attributes: [] }),
    '/api/v1/people/7/files/search': () => Response.json({ files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} }),
    '/api/v1/participants/1': () => Response.json({ id: 1, display_label: 'Synthetic One', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [], profile: saved }),
    '/api/v1/relationships/1/calendar': () => Response.json({ year: 2026, days: [] }),
  });
  const directory = new DirectoryController(client); const controller = new RelationshipsController(client, () => 'UTC'); const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', directoryPersonID: 7 });
  const sync = () => view.rerender({ urlState: state.current, target: state.current.relationshipTarget, filesOpen: state.current.relationshipFiles });
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: vi.fn(),
    onReplaceNavigation: patch => { state.replaceCommittedNavigation(patch); void sync(); if (patch.relationshipTarget) void controller.openTarget(patch.relationshipTarget, {}, false); },
    onFilesToggle: value => { state.commitNavigation({ relationshipFiles: value }); void sync(); }, onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await fireEvent.click(await screen.findByRole('radio', { name: /^Files$/ }));
    await fireEvent.input(await screen.findByRole('searchbox', { name: 'Filter filename' }), { target: { value: 'report' } });
    await fireEvent.click(screen.getByRole('checkbox', { name: 'PDFs' }));
    await act(() => releaseProfile(Response.json(saved)));
    await waitFor(() => expect(state.current.relationshipTarget).toBe('cluster:1'));
    expect(state.current.relationshipFiles).toBe(true);
    await waitFor(() => expect(within(screen.getByRole('radiogroup', { name: 'Person tabs' })).getByRole('radio', { name: /^Files$/ }).getAttribute('aria-checked')).toBe('true'));
    expect(screen.getByRole('searchbox', { name: 'Filter filename' })).toHaveProperty('value', 'report');
    expect(screen.getByRole('checkbox', { name: 'PDFs' })).toHaveProperty('checked', false);
  } finally { releaseProfile(Response.json(saved)); view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('keeps Messages mounted across a profile reload', async () => {
  let releaseProfile!: (value: Response) => void; let personReads = 0;
  const pendingProfile = new Promise<Response>(resolve => releaseProfile = resolve);
  const saved = profile(7, 1, [1]);
  const client = clientFor({
    '/api/v1/people/7': () => ++personReads === 1 ? Response.json(saved) : pendingProfile,
    '/api/v1/participants/1': () => Response.json({ id: 1, display_label: 'Synthetic One', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [], profile: saved }),
    '/api/v1/relationships/1/calendar': () => Response.json({ year: 2026, days: [] }),
  });
  const directory = new DirectoryController(client); const controller = new RelationshipsController(client, () => 'UTC'); const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipTarget: 'cluster:1' });
  await controller.openTarget('cluster:1', {}, false);
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: state.current, target: 'cluster:1', filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    const messages = await screen.findByRole('radio', { name: 'Messages' });
    await waitFor(() => expect(directory.detailLoading).toBe(false));
    let reload!: Promise<void>;
    await act(() => { reload = directory.loadSelection(7); });
    expect(messages.isConnected).toBe(true);
    expect(screen.queryByRole('button', { name: 'Save' })).toBeNull();
    expect(screen.getByRole('radio', { name: 'Messages' })).toBe(messages);
    await act(async () => { releaseProfile(Response.json(saved)); await reload; });
    expect(screen.getByRole('radio', { name: 'Messages' })).toBe(messages);
  } finally { releaseProfile(Response.json(saved)); view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('routes reconciliation Reload to the directory controller', async () => {
  const directoryRequests: Request[] = [];
  const saved = profile(7, 2, []);
  const client = clientFor({
    '/api/v1/people/directory': request => {
      directoryRequests.push(request);
      return directoryRequests.length === 2
        ? Response.json({ message: 'Directory reconciliation unavailable.' }, { status: 503 })
        : Response.json({ people: [{ ...saved, contact_state: 'active', categories: [], organizations: ['Example Org'], primary_channel: 'email' }] });
    },
    '/api/v1/people/7': () => Response.json(saved),
    '/api/v1/people/7/profile': request => Response.json({ person: saved, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] }, { headers: { ETag: request.method === 'PATCH' ? '"person-7-r3"' : '"person-7-r2"' } }),
  });
  const controller = new DirectoryController(client);
  const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', identityQuery: 'synthetic', relationshipSaved: true, directoryContactState: 'active', directoryCategory: 'STRASSE', directoryOrganization: 'Example Org', directoryPrimaryChannel: 'email', directoryPersonID: 7 });
  const relationships = new RelationshipsController(client, () => 'UTC');
  const view = render(PeopleWorkspace, { props: { client, directory: controller, controller: relationships, urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await screen.findByRole('button', { name: 'Add phone' });
    await controller.profile?.patchProfile({ categories: { add: [{ original_value: 'Straße', envelope: { source: 'user' } }] } });
    const reload = vi.spyOn(controller, 'reloadFirstPage');
    await fireEvent.click(await screen.findByRole('button', { name: 'Reload' }));
    await waitFor(() => expect(reload).toHaveBeenCalledOnce());
  } finally { view.unmount(); relationships.destroy(); controller.destroy(); state.destroy(); }
});

it.each([['participant', false], ['profile', false], ['missing', false], ['network', false], ['network', true]] as const)('retains a saved person and its recovery when the %s lookup fails (remount=%s)', async (failure, remount) => {
  let failed = true;
  const saved = profile(failure === 'network' ? 9 : 7, 1, [1]);
  const client = clientFor({
    '/api/v1/people/7': () => failure === 'network' ? Response.json(profile(7, 1, [1])) : failure !== 'participant' && failed ? Response.json({ message: failure === 'missing' ? 'Person not found' : 'Saved profile unavailable' }, { status: failure === 'missing' ? 404 : 503 }) : Response.json(saved),
    '/api/v1/people/9': () => { if (failed) throw new Error('Could not load selected person.'); return Response.json(saved); },
    '/api/v1/participants/1': () => failure === 'participant' && failed ? Response.json({ message: 'Participant unavailable' }, { status: 503 }) : Response.json({ id: 1, display_label: 'Synthetic One', profile: saved, identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }),
    '/api/v1/relationships/1/calendar': () => Response.json({ year: 2026, days: [] }),
  });
  const directory = new DirectoryController(client); const controller = new RelationshipsController(client, () => 'UTC'); const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipTarget: 'cluster:1', directoryPersonID: failure !== 'profile' ? 7 : null });
  await controller.openTarget('cluster:1', {}, false);
  const props = { client, directory, controller, urlState: state.current, target: 'cluster:1', filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() };
  let view = render(PeopleWorkspace, { props });
  try {
    if (failure === 'network') {
      await screen.findByRole('heading', { name: 'Synthetic One' });
      if (remount) { state.commitNavigation({ workspace: 'directory' }); view.unmount(); }
      state.commitNavigation({ workspace: 'relationships', directoryPersonID: 9, relationshipTarget: null });
      if (remount) view = render(PeopleWorkspace, { props: { ...props, urlState: state.current, target: null } });
      else await view.rerender({ urlState: state.current, target: null });
    }
    await waitFor(() => expect(directory.detailLoading).toBe(false));
    if (failure === 'profile') {
      expect(screen.getByRole('radio', { name: 'Messages' })).toBeDefined();
      await fireEvent.click(screen.getByRole('radio', { name: 'Details' }));
    }
    expect(await screen.findByText(new RegExp(failure === 'network' ? 'Could not load selected person' : failure === 'profile' ? 'Saved profile unavailable' : failure === 'missing' ? 'Person not found' : 'Participant unavailable'))).toBeDefined();
    expect(screen.queryByText('Loading person…')).toBeNull();
    if (failure === 'network') expect(screen.queryByRole('heading', { name: 'Synthetic One' })).toBeNull();
    failed = false;
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(screen.queryByText(/Saved profile unavailable|Participant unavailable|Person not found|Could not load selected person/)).toBeNull());
    if (failure === 'network') expect(await screen.findByRole('heading', { name: 'Synthetic Two' })).toBeDefined();
    else expect(await screen.findByRole('radio', { name: 'Messages' })).toBeDefined();
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it.each([false, true])('refreshes the selected profile after bulk links (partial failure=%s)', async partial => {
  let links = 0; let revision = 1; const writes: Request[] = [];
  const saved = () => profile(7, revision, revision === 1 ? [1] : [1, 2]);
  const client = clientFor({
    '/api/v1/relationships': () => Response.json({ rows: ['Alice Example', 'Bob Example', 'Cara Example'].map((display_label, i) => ({ canonical_id: i + 1, display_label, member_ids: [i + 1], last_at: when, score: 2, signals: { last_interaction_at: when, meeting_count: 0, meetings_together: 0, modalities: 2, received_from_them: 1, sent_count: 3, sent_to_them: 1 } })) }),
    '/api/v1/participants/1': () => Response.json({ id: 1, display_label: 'Synthetic One', profile: saved(), identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }),
    '/api/v1/relationships/1/calendar': () => Response.json({ year: 2026, days: [] }),
    '/api/v1/people/7': () => Response.json(saved(), { headers: { ETag: `"person-7-r${revision}"` } }),
    '/api/v1/people/7/profile': request => {
      if (request.method === 'PATCH') { writes.push(request); return Response.json({ person: saved(), names: [], addresses: [], dates: [], categories: [], media: [], contact_points: [] }, { headers: { ETag: `"person-7-r${revision + 1}"` } }); }
      return Response.json({ person: saved(), names: [], addresses: [], dates: [], categories: [], media: [], contact_points: [] }, { headers: { ETag: `"person-7-r${revision}"` } });
    },
    '/api/v1/identity/links': () => { if (++links === 2 && partial) return Response.json({ error: 'internal_error', message: 'Synthetic pair failed' }, { status: 500 }); revision += 1; return Response.json({ identity_revision: revision, cache_state: 'ready' }); },
  });
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipTarget: 'cluster:1' });
  await controller.openTarget('cluster:1', {}, false);
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: state.current, target: 'cluster:1', filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await fireEvent.click(await screen.findByRole('radio', { name: 'Details' }));
    await screen.findByRole('button', { name: 'Add phone' });
    for (const name of ['Alice Example', 'Bob Example', 'Cara Example']) await fireEvent.click(await screen.findByRole('checkbox', { name: `Select ${name}` }));
    await fireEvent.click(screen.getByRole('button', { name: 'Same person' }));
    await screen.findByText(partial ? /Synthetic pair failed/ : 'People are now treated as the same person.');
    await fireEvent.click(screen.getByRole('button', { name: 'Add phone' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Contact value' }), { target: { value: '+12025550123' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save contact point' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]!.headers.get('If-Match')).toBe(`"person-7-r${revision}"`);
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('keeps person B selected when an earlier deletion of person A completes', async () => {
  let finish!: (response: Response) => void;
  const pending = new Promise<Response>(resolve => { finish = resolve; });
  const routes: Record<string, (request: Request) => Response | Promise<Response>> = {};
  for (const id of [7, 9]) {
    routes[`/api/v1/people/${id}`] = request => request.method === 'DELETE' ? pending : Response.json(profile(id, 1, []), { headers: { ETag: `"person-${id}-r1"` } });
    routes[`/api/v1/people/${id}/profile`] = () => Response.json({ person: profile(id, 1, []), names: [], addresses: [], dates: [], categories: [], media: [], contact_points: [] }, { headers: { ETag: `"person-${id}-r1"` } });
  }
  const client = clientFor(routes); const directory = new DirectoryController(client); const controller = new RelationshipsController(client, () => 'UTC'); const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', directoryPersonID: 7 });
  const onNavigate = vi.fn();
  const view = render(PeopleWorkspace, { props: { client, directory, controller, urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate, onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: 'More actions' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Delete' }));
    const deletion = vi.spyOn(directory.profile!, 'deletePerson');
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete person' }));
    await waitFor(() => expect(directory.profile?.mutationPending).toBe(true));
    state.commitNavigation({ directoryPersonID: 9 });
    await view.rerender({ urlState: state.current });
    await screen.findByRole('heading', { name: 'Synthetic Two' });
    await act(async () => { finish(new Response(null, { status: 204 })); await deletion.mock.results[0]!.value; });
    expect(onNavigate).not.toHaveBeenCalled();
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it.each([true, false])('keeps the split receipt visible after its source loses the displayed binding (saved entry=%s)', async savedEntry => {
  let split = false;
  const saved = () => profile(7, split ? 5 : 4, split ? [] : [1]);
  const merge = { id: 41, survivor_person_id: 7, absorbed_person_id: 9, current_person_id: 7, survivor_vcard_uid: 'synthetic-7', absorbed_vcard_uid: 'synthetic-9', survivor_revision_before: 3, absorbed_revision_before: 2, survivor_revision_after: 4, actor: 'web', snapshot_version: 1, snapshot_sha256: 'synthetic-digest', created_at: when };
  const client = clientFor({
    '/api/v1/people/7': () => Response.json(saved(), { headers: { ETag: `"person-7-r${saved().revision}"` } }),
    '/api/v1/people/7/profile': () => Response.json({ person: saved(), names: [], addresses: [], dates: [], categories: [], media: [], contact_points: [] }, { headers: { ETag: `"person-7-r${saved().revision}"` } }),
    '/api/v1/people/7/profile/history': () => Response.json({ person: saved(), names: [], addresses: [], dates: [], categories: [], media: [], contact_points: [] }),
    '/api/v1/people/7/merges': () => Response.json({ merges: [{ merge, participant_count: 1, row_count: 0, pending_candidate_count: 0, split_count: 0, row_action_counts: {} }] }),
    '/api/v1/person-merges/41': () => Response.json({ merge, participants: [{ merge_id: 41, participant_id: 1, origin_side: 'absorbed' }], rows: [], splits: [], review_candidates: [] }),
    '/api/v1/people/7/split': () => { split = true; return Response.json({ source_person: saved(), new_person: profile(9, 1, [1]), split: { id: 55, merge_id: 41, source_person_id: 7, new_person_id: 9, new_person_uid: 'synthetic-9', source_revision_before: 4, source_revision_after: 5, exact_reversal: false, actor: 'web', created_at: when }, exact_reversal: false, ambiguous_rows: [{ table_name: 'person_names', original_row_key: 'synthetic-row', action: 'ambiguous' }], unrestored_rows: [], uid_alias_disposition: 'restored', identity_revision: 2, cache_state: 'ready' }); },
    '/api/v1/participants/1': () => Response.json({ id: 1, display_label: 'Synthetic One', profile: split ? profile(9, 1, [1]) : saved(), identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }),
    '/api/v1/relationships/1/calendar': () => Response.json({ year: 2026, days: [] }),
  });
  const directory = new DirectoryController(client); const controller = new RelationshipsController(client, () => 'UTC'); const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', directoryPersonID: savedEntry ? 7 : null, relationshipTarget: 'cluster:1' });
  await controller.openTarget('cluster:1', {}, false);
  const view = render(PeopleWorkspace, { props: { client, directory, controller, urlState: state.current, target: 'cluster:1', filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: patch => { state.replaceCommittedNavigation(patch); void Promise.resolve().then(() => view.rerender({ urlState: state.current, target: state.current.relationshipTarget })); }, onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: 'More actions' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: 'History' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Inspect merge 41' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Split merged profile' }));
    await fireEvent.click(await screen.findByRole('checkbox', { name: 'Participant 1' }));
    await fireEvent.click(screen.getByRole('checkbox', { name: /I confirm splitting/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Create restored person' }));
    await waitFor(() => expect(controller.detail).toMatchObject({ profile: { id: 9 } }));
    expect(await screen.findByRole('heading', { name: 'Split completed' })).toBeDefined();
    expect(state.current.relationshipTarget).toBeNull();
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('leaving People aborts its pending Save', async () => {
  let pending: Request | undefined;
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input); const path = new URL(request.url).pathname;
    if (path === '/api/v1/people' && request.method === 'POST') { pending = request; return new Promise<Response>(resolve => request.signal.addEventListener('abort', () => resolve(Response.json({ id: 42, revision: 1 }, { status: 201 })), { once: true })); }
    if (path === '/api/v1/participants/11') return Response.json({ id: 11, display_label: 'Synthetic Candidate', identifiers: [], activity_count: 1, file_count: 0, source_counts: [], first_at: '2026-07-19T10:00:00Z', last_at: '2026-07-19T10:00:00Z' });
    return meetingFixtureResponse(path) ?? Response.json({ rows: [] });
  });
  const state = new ExploreState(null); const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client);
  state.commitNavigation({ workspace: 'relationships', relationshipTarget: 'cluster:11' });
  await controller.openTarget('cluster:11', {}, false);
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: state.current, target: 'cluster:11', filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: 'Save' }));
    await waitFor(() => expect(pending).toBeDefined());
    view.unmount();
    expect(pending!.signal.aborted).toBe(true);
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});
