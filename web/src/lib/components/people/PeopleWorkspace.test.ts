import { openTypeahead } from '../../../test/kit-ui';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it, vi, onTestFinished } from 'vitest';
import { createAPIClient } from '../../api/client';
import { RelationshipsController } from '../../relationships/controller.svelte';
import { DirectoryController } from '../../directory/controller.svelte';
import { ExploreState } from '../../explore/state.svelte';
import { meetingFixtureResponse } from '../../meetings/fixtures.test-support';
import PeopleWorkspace from './PeopleWorkspace.svelte';

const when = '2026-07-19T10:00:00Z';
function clientFor(routes: Record<string, (request: Request) => Promise<Response> | Response>) {
  return createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    return routes[path]?.(request) ?? meetingFixtureResponse(path) ?? Response.json({ rows: [] });
  });
}
const profile = (id: number, revision: number, participant_ids: number[]) => ({ id, revision, participant_ids, display_name: id === 7 ? 'Synthetic One' : 'Synthetic Two', created_at: when, updated_at: when, vcard_uid: `synthetic-${id}` });

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

it('opening a saved person reads its profile once and leaves Directory sections lazy', async () => {
  const paths: string[] = [];
  const profile = { id: 91, display_name: 'Saved Example', participant_ids: [], organizations: [], categories: [], contact_state: 'active', revision: 1, vcard_uid: 'example-91', created_at: '2026-07-18T12:00:00Z', updated_at: '2026-07-18T12:00:00Z' };
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    paths.push(path);
    if (path === '/api/v1/people/directory') return Response.json({ people: [profile] });
    if (path === '/api/v1/people/91') return Response.json(profile);
    return meetingFixtureResponse(path) ?? Response.json({ rows: [] });
  });
  const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true });
  const directory = new DirectoryController(client);
  const controller = new RelationshipsController(client, () => 'UTC');
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: patch => state.commitNavigation(patch), onReplaceNavigation: patch => state.replaceCommittedNavigation(patch), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await fireEvent.click(await screen.findByText('Saved Example'));
    await screen.findByRole('heading', { name: 'Saved Example' });
    await waitFor(() => expect(directory.detailLoading).toBe(false));
    expect(paths.filter(path => path === '/api/v1/people/91')).toHaveLength(1);
    expect(paths.filter(path => path.startsWith('/api/v1/people/91/'))).toEqual([]);
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

it('refreshes the Saved list after merging profiles from Details', async () => {
  let merged = false;
  const one = { ...profile(7, 4, [1]), organizations: [], categories: [], contact_state: 'active' };
  const two = { ...profile(9, 2, [2]), organizations: [], categories: [], contact_state: 'active' };
  const client = clientFor({
    '/api/v1/people/directory': () => Response.json({ people: merged ? [one] : [one, two] }),
    '/api/v1/people/7': () => Response.json(one),
    '/api/v1/participants/1': () => Response.json({ id: 1, profile: { id: 7 }, display_label: 'Synthetic One', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }),
    '/api/v1/participants/search': () => Response.json({ rows: [{ id: 2, display_label: 'Bob Example', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }], total_count: 1, cache_revision: 'cache-1', search_provenance: {} }),
    '/api/v1/identity/links': () => merged ? Response.json({ identity_revision: 8, cache_state: 'ready' }) : Response.json({ error: 'person_merge_required', message: 'Choose a survivor', profiles: [{ etag: '"person-7-r4"', person: one }, { etag: '"person-9-r2"', person: two }] }, { status: 409 }),
    '/api/v1/people/7/merge': () => { merged = true; return Response.json({ cache_state: 'ready', identity_revision: 8, review_candidates: [], person: { ...one, revision: 5, participant_ids: [1, 2] }, merge: { id: 41, survivor_person_id: 7, absorbed_person_id: 9, current_person_id: 7, survivor_vcard_uid: 'synthetic-7', absorbed_vcard_uid: 'synthetic-9', survivor_revision_before: 4, absorbed_revision_before: 2, survivor_revision_after: 5, actor: 'web', snapshot_version: 1, snapshot_sha256: 'synthetic-digest', created_at: when } }, { headers: { ETag: '"person-7-r5"' } }); },
  });
  const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true, directoryPersonID: 7, relationshipTarget: 'cluster:1' });
  const directory = new DirectoryController(client);
  const controller = new RelationshipsController(client, () => 'UTC');
  await controller.openTarget('cluster:1', {}, false);
  const onOpenDirectoryPerson = vi.fn();
  const props = { client, controller, directory, urlState: state.current, target: 'cluster:1', filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn(), onOpenDirectoryPerson };
  let view = render(PeopleWorkspace, { props });
  try {
    await within(await screen.findByRole('grid', { name: 'People results' })).findByText('Synthetic Two');
    await fireEvent.click(screen.getByRole('radio', { name: 'Details' }));
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
    view.unmount();
    // Back returns to People with the same saved-list URL and shell-owned controllers.
    view = render(PeopleWorkspace, { props });
    await waitFor(() => expect(within(screen.getByRole('grid', { name: 'People results' })).queryByText('Synthetic Two')).toBeNull());
    await fireEvent.click(within(screen.getByRole('grid', { name: 'People results' })).getByText('Synthetic One'));
    await waitFor(() => expect(props.onNavigate).toHaveBeenCalledWith({ directoryPersonID: 7, relationshipTarget: 'cluster:1', relationshipFiles: false }));
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});
