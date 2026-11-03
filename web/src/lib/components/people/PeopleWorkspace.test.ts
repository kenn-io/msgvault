import { directoryPerson, mergeResult, person as profile } from '../../directory/entity-fixtures.test-support';
import { DirectoryReviewController } from '../../directory/review-controller.svelte';
import { emptyProfile } from '../../directory/profile-fixtures.test-support';
import { openTypeahead } from '../../../test/kit-ui';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it, vi, onTestFinished } from 'vitest';
import { createAPIClient } from '../../api/client';
import { RelationshipsController } from '../../relationships/controller.svelte';
import { DirectoryController } from '../../directory/controller.svelte';
import { ExploreState } from '../../explore/state.svelte';
import { meetingFixtureResponse } from '../../meetings/fixtures.test-support';
import PeopleWorkspace from './PeopleWorkspace.svelte';
import { employment, organizationProfile } from '../../directory/entity-fixtures.test-support';
import { emptyCalendar } from './fixtures.test-support';

const when = '2026-07-19T10:00:00Z';
function clientFor(routes: Record<string, (request: Request) => Promise<Response> | Response>) {
  return createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    const personID = Number(path.match(/\/people\/(\d+)\//)?.[1]);
    return routes[path]?.(request) ?? (path.endsWith('/profile') ? Response.json(emptyProfile(profile(personID, 1, []))) : undefined) ?? (path.endsWith('/attributes') ? Response.json({ person_id: personID, attributes: [] }) : undefined) ?? (path.endsWith('/attribute-definitions') ? Response.json({ definitions: [] }) : undefined) ?? (path.endsWith('/contact-state') ? Response.json({ person_id: personID, cadence_status: 'unknown', interaction_count: 0, computed_at: when, stale: false }) : undefined) ?? (path.endsWith('/network') ? Response.json({ root_person_id: personID, depth: 1, nodes: [], edges: [], truncated: false }) : undefined) ?? meetingFixtureResponse(path) ?? Response.json({ rows: [] });
  });
}

it.each(['missing', 'draft', 'filtered'])('preserves the %s person state in the redesigned page', async (kind) => {
  const client = clientFor({
    '/api/v1/participants/1': () => kind === 'missing'
      ? Response.json({ error: 'participant_not_found' }, { status: 404 })
      : Response.json({ id: 1, display_label: 'Synthetic contact', identifiers: [], activity_count: kind === 'draft' ? 0 : 1,
        ...(kind === 'draft' ? {} : { first_at: when, last_at: when }), file_count: 0, source_counts: [], cache_revision: 'cache-1' }),
    '/api/v1/participants/1/summary': () => Response.json({ error: 'No matching activity' }, { status: 404 }),
    '/api/v1/relationships/1/calendar': async request => Response.json(emptyCalendar((await request.json()).year)),
  });
  const controller = new RelationshipsController(client, () => 'UTC');
  const directory = new DirectoryController(client);
  const state = new ExploreState(null);
  const predicate = kind === 'filtered' ? { filters: [{ dimension: 'source' as const, values: ['2'] }] } : {};
  state.commitNavigation({ workspace: 'relationships', relationshipTarget: 'cluster:1' });
  await controller.openTarget('cluster:1', predicate);
  const view = render(PeopleWorkspace, { props: { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: 'cluster:1', filesOpen: false, predicate,
    onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    if (kind === 'missing') {
      expect(await screen.findByText('Person or domain not found')).toBeDefined();
      expect(screen.queryByRole('region', { name: 'Relationship activity calendar' })).toBeNull();
    } else {
      expect(await screen.findByRole('heading', { name: 'Synthetic contact' })).toBeDefined();
      expect(Boolean(screen.queryByRole('region', { name: 'Relationship activity calendar' }))).toBe(kind === 'filtered');
      await fireEvent.click(screen.getByRole('radio', { name: 'Details' }));
      if (kind === 'draft') expect(await screen.findByText('No sent or received messages with this contact yet.')).toBeDefined();
      else {
        expect(await screen.findByRole('heading', { name: 'Addresses' })).toBeDefined();
        expect(screen.queryByText('No sent or received messages with this contact yet.')).toBeNull();
      }
    }
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('keeps the day across Details and the file filter across Files', async () => {
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
  await controller.openTarget('cluster:1', {});
  const props = { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: 'cluster:1', filesOpen: true, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() };
  const view = render(PeopleWorkspace, { props });
  try {
    await fireEvent.input(await screen.findByRole('searchbox', { name: 'Filter filename' }), { target: { value: 'report' } });
    await fireEvent.click(screen.getByRole('radio', { name: 'Messages' }));
    await view.rerender({ filesOpen: false });
    await fireEvent.click((await screen.findAllByRole('button', { name: '1 message on Jul 19, 2026' }))[0]);
    await screen.findByRole('button', { name: 'Clear date' });
    await fireEvent.click(screen.getByRole('radio', { name: 'Details' }));
    const root = screen.getByRole('main', { name: 'People' });
    for (const width of [640, 1200]) {
      await act(() => observers.get(root)?.([{ target: root, contentRect: { width } } as unknown as ResizeObserverEntry], {} as ResizeObserver));
      expect(screen.getByRole('radio', { name: 'Details' }).getAttribute('aria-checked')).toBe('true');
    }
    await fireEvent.click(screen.getByRole('radio', { name: 'Messages' }));
    expect(screen.queryByRole('button', { name: 'Clear date' })).not.toBeNull();
    await fireEvent.click(screen.getByRole('radio', { name: 'Files' }));
    await view.rerender({ filesOpen: true });
    expect((await screen.findByRole('searchbox', { name: 'Filter filename' }) as HTMLInputElement).value).toBe('report');
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('Escape from People results closes the reader before leaving the person', async () => {
  const client = clientFor({
    '/api/v1/relationships': () => Response.json({ rows: [] }),
    '/api/v1/participants/1': () => Response.json({ id: 1, display_label: 'Alice Example', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }),
    '/api/v1/relationships/1/calendar': async request => Response.json(emptyCalendar((await request.json()).year)),
    '/api/v1/participants/1/timeline': () => Response.json({ rows: [{ key: 'message:2', kind: 'email', title: 'Saved message', occurred_at: when, first_at: when, message_type: 'email', conversation_type: 'email', source_id: 1, source_type: 'gmail', source_identifier: 'owner@example.com', participant_labels: [], participant_ids: [], attachment_count: 0, attachment_size: 0, message_count: 1, deleted_from_source: false, conversation_id: 2, anchor_message_id: 2, match: {} }], total_count: 1, cache_revision: 'cache-1', search_provenance: {} }),
    '/api/v1/conversations/2': () => Response.json({ id: 2, messages: [{ id: 2, conversation_id: 2, subject: 'Saved message', sent_at: when, message_type: 'email', from: 'Alice Example', to: [], body: 'Reader body', snippet: 'Reader body', attachments: [] }], anchor_id: 2, total: 1, has_before: false, has_after: false }),
    '/api/v1/cli/accounts': () => Response.json({ accounts: [] }),
  });
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipTarget: 'cluster:1' });
  await controller.openTarget('cluster:1', {});
  const onNavigate = vi.fn(patch => state.commitNavigation(patch));
  const view = render(PeopleWorkspace, { props: { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: 'cluster:1', filesOpen: false, predicate: {}, onNavigate, onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    const messages = await screen.findByRole('grid', { name: 'Messages results' });
    await fireEvent.click(await within(messages).findByText('Saved message'));
    await screen.findByText('Reader body');
    const people = screen.getByRole('grid', { name: 'People results' });
    people.focus();
    await fireEvent.keyDown(people, { key: 'Escape' });
    expect(state.current.relationshipTarget).toBe('cluster:1');
    await waitFor(() => expect(screen.queryByText('Reader body')).toBeNull());
    expect(onNavigate).not.toHaveBeenCalled();
    people.focus();
    await fireEvent.keyDown(people, { key: 'Escape' });
    expect(state.current.relationshipTarget).toBeNull();
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); window.history.replaceState(null, '', '/'); }
});

it('Escape in Possible duplicates preserves the selected person on returning to People', async () => {
  const client = clientFor({
    '/api/v1/people/directory': () => Response.json({ people: [directoryPerson(7)] }),
    '/api/v1/people/7': () => Response.json(profile(7, 4, []), { headers: { ETag: '"person-7-r4"' } }),
  });
  const controller = new RelationshipsController(client, () => 'UTC'), directory = new DirectoryController(client), state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true, directoryPersonID: 7 });
  const view = render(PeopleWorkspace, { props: { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: patch => { state.commitNavigation(patch); void view.rerender({ urlState: state.current }); }, onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await screen.findByRole('heading', { name: 'Synthetic One' });
    await fireEvent.click(within(screen.getByRole('complementary', { name: 'People' })).getByRole('button', { name: 'Filter' }));
    await fireEvent.click(await screen.findByText('Possible duplicates', { exact: true }));
    await screen.findByRole('heading', { name: 'Possible duplicates' });
    const back = screen.getByRole('button', { name: 'Back to People' });
    back.focus();
    await fireEvent.keyDown(back, { key: 'Escape' });
    await fireEvent.click(back);
    await screen.findByRole('heading', { name: 'Synthetic One' });
    expect(state.current.directoryPersonID).toBe(7);
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it.each(['sort', 'filter'])('Escape dismisses the People %s before leaving the open person', async (control) => {
  const client = clientFor({
    '/api/v1/relationships': () => Response.json({ rows: [] }),
    '/api/v1/participants/1': () => Response.json({ id: 1, display_label: 'Alice Example', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }),
    '/api/v1/relationships/1/calendar': async request => Response.json(emptyCalendar((await request.json()).year)),
  });
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipTarget: 'cluster:1' });
  await controller.openTarget('cluster:1', {});
  const onNavigate = vi.fn();
  const view = render(PeopleWorkspace, { props: { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: 'cluster:1', filesOpen: false, predicate: {}, onNavigate, onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
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

it.each([
  { outcome: 'is cancelled', selectedID: null },
  { outcome: 'merges', selectedID: null },
  { outcome: 'merges', selectedID: 7 },
  { outcome: 'merges', selectedID: 42 },
])('reports the result when the next pair $outcome (selected=$selectedID)', async ({ outcome, selectedID }) => {
  let calls = 0; let listRequests = 0; let releaseReconcile: (() => void) | undefined;
  let merged = false;
  const one = profile(7, 4, [1]), two = profile(9, 2, [2]);
  const unrelated = { ...profile(42, 1, []), display_name: 'Unrelated person' };
  const survivor = { ...two, revision: 3, participant_ids: [1, 2] };
  const structured = (person: ReturnType<typeof profile>, alias: string) => ({ ...emptyProfile(person), names: [{ person_id: person.id, name_kind: 'nickname', formatted: alias, original_value: alias, is_derived: false, envelope: { id: person.id, ordinal: 0, source: 'user', created_at: when, updated_at: when, vcard: {} } }] });
  const missing = () => Response.json({ error: 'person_not_found', message: 'Person not found.' }, { status: 404 });
  const links: Array<{ participant_a: number; participant_b: number }> = [];
  const client = clientFor({
    '/api/v1/relationships': async () => {
      if (++listRequests === 2 && outcome === 'merges') await new Promise<void>(resolve => { releaseReconcile = resolve; });
      return Response.json({ rows: ['Alice Example', 'Bob Example', 'Cara Example'].map((display_label, index) => ({ canonical_id: index + 1, display_label, last_at: when, member_ids: [index + 1], score: 2, signals: { last_interaction_at: when, meeting_count: 0, meetings_together: 0, modalities: 2, received_from_them: 1, sent_count: 3, sent_to_them: 1 } })) });
    },
    '/api/v1/people/directory': () => Response.json({ people: merged ? [directoryPerson(9, { revision: 3 }), directoryPerson(42, { display_name: 'Unrelated person' })] : [directoryPerson(7), directoryPerson(9), directoryPerson(42, { display_name: 'Unrelated person' })] }),
    '/api/v1/people/7': () => merged ? missing() : Response.json(one, { headers: { ETag: '"person-7-r4"' } }),
    '/api/v1/people/7/profile': () => merged ? missing() : Response.json(structured(one, 'Absorbed alias'), { headers: { ETag: '"person-7-r4"' } }),
    '/api/v1/people/9': () => Response.json(merged ? survivor : two, { headers: { ETag: merged ? '"person-9-r3"' : '"person-9-r2"' } }),
    '/api/v1/people/9/profile': () => Response.json(structured(merged ? survivor : two, 'Survivor alias'), { headers: { ETag: merged ? '"person-9-r3"' : '"person-9-r2"' } }),
    '/api/v1/people/42': () => Response.json(unrelated, { headers: { ETag: '"person-42-r1"' } }),
    '/api/v1/people/42/profile': () => Response.json(structured(unrelated, 'Unrelated alias'), { headers: { ETag: '"person-42-r1"' } }),
    '/api/v1/participants/1': () => Response.json({ id: 1, profile: merged ? survivor : one, display_label: merged ? 'Synthetic Two' : 'Synthetic One', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }),
    '/api/v1/relationships/1/calendar': () => Response.json(emptyCalendar(2026)),
    '/api/v1/identity/links': async request => {
      links.push(await request.json()); calls += 1;
      if (outcome === 'merges') return calls === 1 ? Response.json({ error: 'person_merge_required', message: 'Choose a survivor', profiles: [{ etag: '"person-7-r4"', person: profile(7, 4, [1]) }, { etag: '"person-9-r2"', person: profile(9, 2, [2]) }] }, { status: 409 }) : Response.json({ identity_revision: 5, cache_state: 'ready' });
      return calls === 1 ? Response.json({ identity_revision: 5, cache_state: 'ready' }) : Response.json({ error: 'person_merge_required', message: 'Choose a survivor', profiles: [{ etag: '"person-7-r4"', person: profile(7, 4, [1, 2]) }, { etag: '"person-9-r2"', person: profile(9, 2, [3]) }] }, { status: 409 });
    },
    '/api/v1/people/9/merge': () => { merged = true; return Response.json(mergeResult(survivor), { headers: { ETag: '"person-9-r3"' } }); },
  });
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  if (selectedID !== null) state.commitNavigation({ workspace: 'relationships', directoryPersonID: selectedID });
  const sync = () => view.rerender({ urlState: state.current, target: state.current.relationshipTarget });
  const view = render(PeopleWorkspace, { props: { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: null, filesOpen: false, predicate: { filters: [], presentation: 'table' }, onNavigate: vi.fn(), onReplaceNavigation: patch => { state.replaceCommittedNavigation(patch); void sync(); if (patch.relationshipTarget) void controller.openTarget(patch.relationshipTarget, {}); }, onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    if (selectedID !== null) {
      await fireEvent.click(await screen.findByRole('radio', { name: 'Details' }));
      expect(await screen.findByText(selectedID === 7 ? 'Absorbed alias' : 'Unrelated alias')).toBeDefined();
    }
    for (const name of ['Alice Example', 'Bob Example', 'Cara Example']) await fireEvent.click(await screen.findByRole('checkbox', { name: `Select ${name}` }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    if (outcome === 'merges') {
      await screen.findByRole('dialog', { name: 'Resolve person merge' });
      await fireEvent.click(screen.getByRole('radio', { name: 'Synthetic Two (Person 9)' }));
      await fireEvent.click(screen.getByRole('checkbox', { name: /I understand this consolidates both profiles/i }));
      await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
      await waitFor(() => expect(releaseReconcile).toBeDefined());
      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Resolve person merge' })).toBeNull());
      if (selectedID === null) {
        expect(screen.getByRole('checkbox', { name: 'Unselect Bob Example' }).closest('.list-content')).toHaveProperty('inert', true);
        await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
        expect(links).toHaveLength(1);
      }
      releaseReconcile?.();
      expect(await screen.findByText('People are now treated as the same person.')).toBeDefined();
      expect(links).toEqual([{ participant_a: 1, participant_b: 2 }, { participant_a: 1, participant_b: 2 }, { participant_a: 1, participant_b: 3 }]);
      if (selectedID !== null) {
        const expectedID = selectedID === 7 ? 9 : 42;
        await waitFor(() => expect(directory.selectedPersonID).toBe(expectedID));
        expect(state.current.directoryPersonID).toBe(expectedID);
        await fireEvent.click(screen.getByRole('radio', { name: 'Details' }));
        expect(await screen.findByText(selectedID === 7 ? 'Survivor alias' : 'Unrelated alias')).toBeDefined();
        expect(screen.queryByText('Absorbed alias')).toBeNull();
      }
    } else if (outcome === 'is cancelled') {
      await screen.findByRole('dialog', { name: 'Resolve person merge' });
      await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    }
    if (outcome === 'is cancelled') expect(screen.getByText(/^1 of 2/).getAttribute('role')).toBe('status');
    expect(calls).toBe(outcome === 'merges' ? 3 : 2);
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it.each(['Category', 'Organization'])('forwards typed %s profile filters as drafts', async (field) => {
  const client = clientFor({});
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  const onNavigate = vi.fn(); const onDraftNavigation = vi.fn();
  const view = render(PeopleWorkspace, { props: { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: { ...state.current, directoryContactState: 'active' }, target: null, filesOpen: false, predicate: {}, onNavigate, onReplaceNavigation: vi.fn(), onDraftNavigation, onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
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
  const view = render(PeopleWorkspace, { props: { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: { ...state.current, relationshipSaved: true, relationshipShowAll: true, directoryCategory: 'acme' }, target: null, filesOpen: false, predicate: {}, onNavigate, onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange, onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: /Filter/ }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Clear filters' }));
    expect(onNavigate.mock.calls.length + onShowAllChange.mock.calls.length).toBe(1);
    expect(onNavigate).toHaveBeenCalledExactlyOnceWith({ relationshipSaved: false, relationshipShowAll: false, directoryContactState: '', directoryCategory: '', directoryOrganization: '', directoryPrimaryChannel: '', directoryLastContactAfter: '', directoryLastContactBefore: '' });
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it.each(['list', 'deep link'])('opens a saved person from %s in its person page', async entry => {
  let release!: (response: Response) => void;
  const pending = new Promise<Response>(resolve => release = resolve);
  const saved = { ...profile(7, 1, [1]), display_name: 'Saved Person' };
  const client = clientFor({
    '/api/v1/people/directory': () => Response.json({ people: [directoryPerson(saved.id, { revision: saved.revision, display_name: saved.display_name })] }),
    '/api/v1/people/7': () => entry === 'deep link' ? pending : Response.json(saved, { headers: { ETag: '"person-7-r1"' } }),
    '/api/v1/people/7/profile': () => Response.json(emptyProfile(saved), { headers: { ETag: '"person-7-r1"' } }),
    '/api/v1/participants/1': () => Response.json({ id: 1, profile: saved, display_label: 'Saved Person', identifiers: [], first_at: when, last_at: when, activity_count: 50, file_count: 0, source_counts: [] }),
    '/api/v1/people/7/contact-state': () => Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 50, computed_at: when, stale: false }),
    '/api/v1/people/7/employments': () => Response.json({ employments: [employment()] }),
    '/api/v1/organizations/21': () => Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } }),
    '/api/v1/relationships/1/calendar': () => Response.json(emptyCalendar(2026)),
  });
  const requests = vi.spyOn(client, 'fetch');
  const controller = new RelationshipsController(client, () => 'UTC'), directory = new DirectoryController(client), state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true, directoryPersonID: entry === 'deep link' ? 7 : null });
  const navigate = (patch: Parameters<typeof state.commitNavigation>[0]) => {
    state.commitNavigation(patch);
    void view.rerender({ urlState: state.current, target: state.current.relationshipTarget });
    if (patch.relationshipTarget) void controller.openTarget(patch.relationshipTarget, {});
  };
  const view = render(PeopleWorkspace, { props: { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: patch => void navigate(patch), onReplaceNavigation: patch => void navigate(patch), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    if (entry === 'list') await fireEvent.click(await within(screen.getByRole('grid', { name: 'People results' })).findByText('Saved Person'));
    else {
      await screen.findByText('Loading details…');
      expect(screen.getByRole('group', { name: 'Person page' })).toBeDefined();
      release(Response.json(saved, { headers: { ETag: '"person-7-r1"' } }));
    }
    await screen.findByRole('heading', { name: 'Saved Person' });
    expect(screen.getByRole('group', { name: 'Person page' })).toBeDefined();
    if (entry === 'list') {
      await waitFor(() => expect(directory.detailLoading).toBe(false));
      const paths = requests.mock.calls.map(([input]) => new URL(input instanceof Request ? input.url : String(input), document.baseURI).pathname);
      expect(paths.filter(path => /attribute-definitions|agenda|brief|\/people\/7\/(employments|relationships)|relationship-types|\/organizations\//.test(path))).toEqual([]);
      await fireEvent.click(screen.getByRole('radio', { name: 'Details' }));
      expect(await screen.findByRole('button', { name: 'Synthetic Org' })).toBeDefined();
    }
  } finally { release(Response.json(saved)); view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('names an empty Domains list for the selected facet', async () => {
  const client = clientFor({});
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  const view = render(PeopleWorkspace, { props: { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: { ...state.current, relationshipFacet: 'domains' }, target: null, filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try { expect(await screen.findByText('No domains yet')).toBeDefined(); }
  finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('opens the checkbox-clicked person when Enter follows the selection', async () => {
  const client = clientFor({ '/api/v1/relationships': () => Response.json({ rows: ['Alice Example', 'Bob Example'].map((display_label, index) => ({ canonical_id: index + 1, display_label, last_at: when, member_ids: [index + 1], score: 2, signals: { last_interaction_at: when, meeting_count: 0, meetings_together: 0, modalities: 2, received_from_them: 1, sent_count: 3, sent_to_them: 1 } })) }) });
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  const onNavigate = vi.fn();
  const view = render(PeopleWorkspace, { props: { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate, onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    const checkbox = await screen.findByRole('checkbox', { name: 'Select Bob Example' });
    await fireEvent.pointerDown(checkbox);
    await fireEvent.click(checkbox);
    await fireEvent.keyDown(screen.getByRole('grid', { name: 'People results' }), { key: 'Enter' });
    expect(onNavigate).toHaveBeenCalledWith({ directoryPersonID: null, relationshipTarget: 'cluster:2', relationshipFiles: false });
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it("passes the saved person's durable ID to Files when the first binding is draft-only", async () => {
  const at = '2026-07-19T10:00:00Z';
  const saved = { ...profile(91, 1, [1, 2]), display_name: 'Saved Example', vcard_uid: 'example-91', created_at: at, updated_at: at };
  const file = (id: number, filename: string) => ({ id, key: `file:${id}`, entry_key: `message:${id}`, message_id: id, conversation_id: id, occurred_at: at, source_id: 1, source_type: 'gmail', source_identifier: 'owner@example.com', containing_title: 'Archived message', filename, mime_type: 'application/pdf', mime_family: 'pdf', size_bytes: 123, participant_labels: ['Saved Example'], participant_domains: ['example.com'], content_state: 'metadata_only', content_available: false, person_provenance: { participant_ids: [id], roles: ['from'], directions: ['from_person'] } });
  const first = file(1, 'first-address.pdf');
  const second = file(2, 'second-address.pdf');
  const requests: string[] = [];
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    requests.push(path);
    if (path === '/api/v1/people/directory') return Response.json({ people: [directoryPerson(saved.id, { revision: saved.revision, display_name: saved.display_name })] });
    if (path === '/api/v1/people/91') return Response.json(saved);
    if (path === '/api/v1/participants/1') return Response.json({ error: 'not_found' }, { status: 404 });
    if (path === '/api/v1/relationships/1/calendar') return Response.json(emptyCalendar(2026));
    if (path === '/api/v1/participants/1/files/search') return Response.json({ files: [first], total_count: 1, cache_revision: 'cache-1', search_provenance: {} });
    if (path === '/api/v1/people/91/files/search') return Response.json({ files: [second], total_count: 1, cache_revision: 'cache-1', search_provenance: {} });
    if (path.endsWith('/network')) return Response.json({ root_person_id: 91, depth: 1, nodes: [], edges: [], truncated: false });
    return meetingFixtureResponse(path) ?? Response.json({ rows: [] });
  });
  const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true, directoryPersonID: 91, relationshipTarget: 'cluster:1', relationshipFiles: true });
  const directory = new DirectoryController(client);
  const controller = new RelationshipsController(client, () => 'UTC');
  await controller.openTarget('cluster:1', {});
  const view = render(PeopleWorkspace, { props: { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: 'cluster:1', filesOpen: true, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await waitFor(() => expect(screen.queryByText('second-address.pdf'), requests.join(', ')).not.toBeNull());
    expect(requests).toContain('/api/v1/people/91/files/search');
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it.each([
  ['complete', false], ['partial', false], ['complete', true], ['partial', true],
] as const)('refreshes cached Saved dates and Most recent order after a %s bulk link, navigating away: %s', async (batch, navigateAway) => {
  let linked = false;
  let releaseLink: (() => void) | undefined;
  const pendingLink = new Promise<void>(resolve => { releaseLink = resolve; });
  const sorts: (string | null)[] = [];
  const pairs: unknown[] = [];
  const one = directoryPerson(7, { revision: 1, last_contact_at: '2019-07-18T10:00:00Z' });
  const two = directoryPerson(9, { revision: 1, last_contact_at: '2020-07-18T10:00:00Z' });
  const client = clientFor({
    '/api/v1/people/directory': request => {
      sorts.push(new URL(request.url).searchParams.get('sort'));
      return Response.json({ people: linked ? [{ ...one, last_contact_at: '2021-07-18T10:00:00Z' }, two] : [two, one] });
    },
    '/api/v1/participants/search': () => Response.json({ rows: ['Synthetic One', 'Bob Example', 'Cara Example'].map((display_label, index) => ({ id: index + 1, display_label, identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [], ...(index === 0 ? { profile: { id: 7 } } : {}) })), total_count: 3, cache_revision: 'cache-1', search_provenance: {} }),
    '/api/v1/identity/links': async request => {
      pairs.push(await request.json());
      if (linked) return Response.json({ error: 'internal_error', message: 'Synthetic link failed' }, { status: 500 });
      await pendingLink;
      linked = true;
      return Response.json({ identity_revision: 2, cache_state: 'ready' });
    },
  });
  const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true, relationshipSort: 'recent' });
  const directory = new DirectoryController(client);
  const controller = new RelationshipsController(client, () => 'UTC');
  const props = { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: (patch: Parameters<ExploreState['commitNavigation']>[0]) => state.commitNavigation(patch), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() };
  let view = render(PeopleWorkspace, { props });
  try {
    const list = within(await screen.findByRole('grid', { name: 'People results' }));
    await list.findByText('2019');
    expect(list.getAllByRole('row').map(row => row.textContent)).toEqual([expect.stringMatching(/Synthetic Two.*2020/), expect.stringMatching(/Synthetic One.*2019/)]);
    await fireEvent.click(screen.getByRole('button', { name: 'Remove Saved' }));
    await view.rerender({ urlState: state.current });
    for (const name of batch === 'partial' ? ['Synthetic One', 'Bob Example', 'Cara Example'] : ['Synthetic One', 'Bob Example']) await fireEvent.click(await screen.findByRole('checkbox', { name: `Select ${name}` }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    await waitFor(() => expect(pairs).toHaveLength(1));
    if (navigateAway) {
      state.commitNavigation({ workspace: 'sources' });
      view.unmount();
    }
    releaseLink!();
    if (navigateAway) {
      await waitFor(() => expect(directory.rows.map(person => [person.id, person.last_contact_at])).toEqual([[7, '2021-07-18T10:00:00Z'], [9, '2020-07-18T10:00:00Z']]));
      state.commitNavigation({ workspace: 'relationships', relationshipSaved: true });
      view = render(PeopleWorkspace, { props: { ...props, urlState: state.current } });
    } else {
      await waitFor(() => expect(sorts).toHaveLength(2));
      await fireEvent.click(screen.getByRole('button', { name: /Filter/ }));
      await fireEvent.click(await screen.findByText('Saved', { exact: true }));
      await view.rerender({ urlState: state.current });
    }
    expect(linked).toBe(true);
    await waitFor(() => expect(within(screen.getByRole('grid', { name: 'People results' })).getAllByRole('row').map(row => row.textContent)).toEqual([expect.stringMatching(/Synthetic One.*2021/), expect.stringMatching(/Synthetic Two.*2020/)]));
    expect(sorts).toEqual(Array(navigateAway ? 3 : 2).fill('last_contact_desc'));
  } finally { releaseLink?.(); view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it.each(['link', 'merge'])('keeps Details current and its draft mounted after a %s', async operation => {
  let changed = false;
  let merged = false;
  const one = { ...profile(7, 4, [1]), last_contact_at: '2020-07-18T10:00:00Z' };
  const two = profile(9, 2, [2]);
  const survivor = { ...one, revision: 5, participant_ids: [1, 2] };
  const client = clientFor({
    '/api/v1/people/directory': () => Response.json({ people: [directoryPerson(7, { revision: changed ? 5 : 4, last_contact_at: changed ? '2021-07-18T10:00:00Z' : one.last_contact_at }), ...(!merged && operation === 'merge' ? [directoryPerson(9)] : [])] }),
    '/api/v1/people/7': () => Response.json(changed ? survivor : one, { headers: { ETag: changed ? '"person-7-r5"' : '"person-7-r4"' } }),
    '/api/v1/people/7/profile': () => Response.json({ person: changed ? survivor : one, names: [], addresses: [], dates: [], categories: [], media: [], contact_points: merged ? [{ person_id: 7, address_kind: 'email', original_value: 'absorbed@example.com', normalized_value: 'absorbed@example.com', envelope: { id: 11, ordinal: 0, source: 'user', vcard: {}, created_at: when, updated_at: when } }] : [] }, { headers: { ETag: changed ? '"person-7-r5"' : '"person-7-r4"' } }),
    '/api/v1/participants/1': () => Response.json({ id: 1, profile: changed ? survivor : one, display_label: 'Synthetic One', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }),
    '/api/v1/participants/search': () => Response.json({ rows: [{ id: 2, display_label: 'Bob Example', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }], total_count: 1, cache_revision: 'cache-1', search_provenance: {} }),
    '/api/v1/relationships/1/calendar': async request => Response.json({ ...emptyCalendar((await request.json()).year), identity_revision: changed ? 8 : 1 }),
    '/api/v1/identity/links': () => {
      if (operation === 'link' || merged) { changed = true; return Response.json({ identity_revision: 8, cache_state: 'ready' }); }
      return Response.json({ error: 'person_merge_required', message: 'Choose a survivor', profiles: [{ etag: '"person-7-r4"', person: one }, { etag: '"person-9-r2"', person: two }] }, { status: 409 });
    },
    '/api/v1/people/7/brief-enrollment': () => Response.json({ person_id: 7, enrolled: merged, enabled_at: merged ? when : null, actor: 'api' }),
    '/api/v1/people/7/brief': () => Response.json({ error: 'person_brief_not_found', message: 'Person brief not found' }, { status: 404 }),
    '/api/v1/people/7/merge': () => { merged = true; changed = true; return Response.json(mergeResult(survivor), { headers: { ETag: '"person-7-r5"' } }); },
  });
  const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true, relationshipTarget: 'cluster:1' });
  const directory = new DirectoryController(client);
  const controller = new RelationshipsController(client, () => 'UTC');
  await controller.openTarget('cluster:1', {});
  const sync = () => Promise.resolve().then(() => view.rerender({ urlState: state.current, target: state.current.relationshipTarget }));
  const onOpenDirectoryPerson = vi.fn((id: number) => { state.commitNavigation({ workspace: 'relationships', directoryPersonID: id, relationshipTarget: null, relationshipFiles: false }); void sync(); });
  const props = { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: 'cluster:1', filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: (patch: Parameters<typeof state.replaceCommittedNavigation>[0]) => { state.replaceCommittedNavigation(patch); void sync(); if (patch.relationshipTarget) void controller.openTarget(patch.relationshipTarget, {}); }, onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn(), onOpenDirectoryPerson };
  let view = render(PeopleWorkspace, { props });
  let sawLoadingPerson = false;
  const observer = new MutationObserver(records => {
    if (records.some(record => [...record.addedNodes].some(node => node.textContent?.includes('Loading person…')))) sawLoadingPerson = true;
  });
  try {
    await waitFor(() => expect(directory.detailLoading).toBe(false));
    await fireEvent.click(await screen.findByRole('radio', { name: 'Details' }));
    await waitFor(() => expect((screen.getByRole('switch', { name: 'Enroll this person in briefs' }) as HTMLInputElement).checked).toBe(false));
    await fireEvent.click(screen.getByRole('button', { name: 'Add phone' }));
    const draft = screen.getByRole('textbox', { name: 'Contact value' });
    await fireEvent.input(draft, { target: { value: '+12025550123' } });
    observer.observe(view.container, { childList: true, subtree: true });
    await fireEvent.click(screen.getByRole('button', { name: 'Merge with…' }));
    const input = await openTypeahead('Search people');
    await fireEvent.input(input, { target: { value: 'Bob' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Bob Example/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    if (operation === 'merge') {
      await screen.findByRole('dialog', { name: 'Resolve person merge' });
      await fireEvent.click(screen.getByRole('radio', { name: 'Synthetic One (Person 7)' }));
      await fireEvent.click(screen.getByRole('checkbox', { name: /I understand this consolidates both profiles/ }));
      await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Resolve person merge' })).toBeNull());
      expect(onOpenDirectoryPerson).toHaveBeenCalledWith(7);
    }
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Merge Synthetic One' })).toBeNull());
    expect(changed).toBe(true);
    expect(screen.getByRole('radio', { name: 'Details' }).getAttribute('aria-checked')).toBe('true');
    expect(screen.getByRole('textbox', { name: 'Contact value' })).toBe(draft);
    expect(draft).toHaveProperty('value', '+12025550123');
    if (operation === 'merge') await waitFor(() => expect((screen.getByRole('switch', { name: 'Enroll this person in briefs' }) as HTMLInputElement).checked).toBe(true));
    expect(sawLoadingPerson).toBe(false);
  } finally { observer.disconnect(); view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('clears the saved selection when opening an observed person', async () => {
  const saved = { ...profile(92, 1, []), display_name: 'Saved Contact' };
  let release!: (response: Response) => void;
  const pending = new Promise<Response>(resolve => release = resolve);
  const client = clientFor({
    '/api/v1/people/directory': () => Response.json({ people: [directoryPerson(saved.id, { revision: saved.revision, display_name: saved.display_name })] }),
    '/api/v1/people/92': () => pending,
    '/api/v1/relationships': () => Response.json({ rows: [{ canonical_id: 1, display_label: 'Alice Example', last_at: when, member_ids: [1], score: 2, signals: { last_interaction_at: when, meeting_count: 0, meetings_together: 0, modalities: 2, received_from_them: 1, sent_count: 3, sent_to_them: 1 } }] }),
  });
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true });
  const onNavigate = vi.fn((patch) => state.commitNavigation(patch));
  const onReplaceNavigation = vi.fn();
  const props = () => ({ client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: state.current.relationshipTarget, filesOpen: false, predicate: {}, onNavigate, onReplaceNavigation, onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() });
  const view = render(PeopleWorkspace, { props: props() });
  try {
    await fireEvent.click(await screen.findByText('Saved Contact'));
    await fireEvent.click(screen.getByRole('button', { name: 'Remove Saved' }));
    await view.rerender(props());
    await fireEvent.click(await screen.findByText('Alice Example'));
    await view.rerender(props());
    await act(async () => { release(Response.json(saved)); await pending; await new Promise(resolve => setTimeout(resolve, 0)); });
    expect(onReplaceNavigation).not.toHaveBeenCalled();
    expect(directory.selectedPersonID).toBeNull();
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it.each([
  [{ identityQuery: 'after' }, 'q', 'after'],
  [{ relationshipSort: 'recent' as const }, 'sort', 'last_contact_desc'],
] as const)('maps Saved query and sort inputs to directory requests: %s', async (patch, parameter, value) => {
  const reads: URL[] = [];
  const client = clientFor({ '/api/v1/people/directory': request => {
    const url = new URL(request.url); reads.push(url);
    return Response.json({ people: [] });
  } });
  const state = new ExploreState(null); state.commitNavigation({ workspace: 'relationships', relationshipSaved: true });
  const directory = new DirectoryController(client); const controller = new RelationshipsController(client, () => 'UTC');
  const props = { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() };
  const view = render(PeopleWorkspace, { props });
  try {
    await waitFor(() => expect(reads).toHaveLength(1));
    await view.rerender({ ...props, urlState: { ...state.current, ...patch } });
    await waitFor(() => expect(reads.at(-1)?.searchParams.get(parameter)).toBe(value));
  } finally { view.unmount(); directory.destroy(); controller.destroy(); state.destroy(); }
});



it('keeps Files and its filename filter when a saved person finishes initial hydration', async () => {
  let releaseProfile!: (response: Response) => void;
  const pendingProfile = new Promise<Response>(resolve => releaseProfile = resolve);
  const saved = profile(7, 1, [1]);
  const client = clientFor({
    '/api/v1/people/7': () => pendingProfile,
    '/api/v1/people/7/profile': () => Response.json(emptyProfile(saved)),
    '/api/v1/people/7/attributes': () => Response.json({ person_id: 7, attributes: [] }),
    '/api/v1/people/7/files/search': () => Response.json({ files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} }),
    '/api/v1/participants/1': () => Response.json({ id: 1, display_label: 'Synthetic One', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [], profile: saved }),
    '/api/v1/relationships/1/calendar': () => Response.json(emptyCalendar(2026)),
  });
  const directory = new DirectoryController(client); const controller = new RelationshipsController(client, () => 'UTC'); const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', directoryPersonID: 7 });
  const sync = () => view.rerender({ urlState: state.current, target: state.current.relationshipTarget, filesOpen: state.current.relationshipFiles });
  const view = render(PeopleWorkspace, { props: { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: vi.fn(),
    onReplaceNavigation: patch => { state.replaceCommittedNavigation(patch); void sync(); if (patch.relationshipTarget) void controller.openTarget(patch.relationshipTarget, {}); },
    onFilesToggle: value => { state.commitNavigation({ relationshipFiles: value }); void sync(); }, onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await fireEvent.click(await screen.findByRole('radio', { name: /^Files$/ }));
    await fireEvent.input(await screen.findByRole('searchbox', { name: 'Filter filename' }), { target: { value: 'report' } });
    await act(() => releaseProfile(Response.json(saved)));
    await waitFor(() => expect(state.current.relationshipTarget).toBe('cluster:1'));
    expect(state.current.relationshipFiles).toBe(true);
    await waitFor(() => expect(within(screen.getByRole('radiogroup', { name: 'Person tabs' })).getByRole('radio', { name: /^Files$/ }).getAttribute('aria-checked')).toBe('true'));
    expect(screen.getByRole('searchbox', { name: 'Filter filename' })).toHaveProperty('value', 'report');
  } finally { releaseProfile(Response.json(saved)); view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});


it('routes reconciliation Reload to the directory controller', async () => {
  const client = clientFor({ '/api/v1/people/directory': () => Response.json({ people: [directoryPerson(7, { revision: 2 })] }) });
  const controller = new DirectoryController(client);
  const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true });
  const relationships = new RelationshipsController(client, () => 'UTC');
  const view = render(PeopleWorkspace, { props: { client, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), directory: controller, controller: relationships, urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await act(async () => {
      await controller.loadFirstPage();
      controller.pageError = 'Directory reconciliation unavailable.';
      controller.pageRecovery = 'reload';
    });
    const reload = vi.spyOn(controller, 'reloadFirstPage');
    await fireEvent.click(await screen.findByRole('button', { name: 'Reload' }));
    await waitFor(() => expect(reload).toHaveBeenCalledOnce());
  } finally { view.unmount(); relationships.destroy(); controller.destroy(); state.destroy(); }
});

it.each([['participant', false], ['second person', false], ['second person', true]] as const)('retains a saved person and its recovery when the %s lookup fails (remount=%s)', async (failure, remount) => {
  let failed = true;
  const saved = profile(failure === 'second person' ? 9 : 7, 1, failure === 'second person' ? [2] : [1]);
  const client = clientFor({
    '/api/v1/people/7': () => failure === 'second person' ? Response.json(profile(7, 1, [1])) : Response.json(saved, { headers: { ETag: `"person-${saved.id}-r1"` } }),
    ...Object.fromEntries([7, 9].flatMap(id => [
      [`/api/v1/people/${id}/profile`, () => Response.json(emptyProfile(profile(id, 1, [id === 9 ? 2 : 1])))],
      [`/api/v1/people/${id}/attributes`, () => Response.json({ person_id: id, attributes: [] })]
    ])),
    '/api/v1/attribute-definitions': () => Response.json({ definitions: [] }),
    '/api/v1/people/9': () => { if (failed) throw new Error('Could not load selected person.'); return Response.json(saved, { headers: { ETag: `"person-${saved.id}-r1"` } }); },
    '/api/v1/participants/1': () => failure === 'participant' && failed ? Response.json({ message: 'Participant unavailable' }, { status: 503 }) : Response.json({ id: 1, display_label: 'Synthetic One', profile: failure === 'second person' ? profile(7, 1, [1]) : saved, identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }),
    '/api/v1/participants/2': () => Response.json({ id: 2, display_label: 'Synthetic Two', profile: saved, identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }),
    '/api/v1/relationships/1/calendar': () => Response.json(emptyCalendar(2026)),
  });
  const directory = new DirectoryController(client); const controller = new RelationshipsController(client, () => 'UTC'); const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipTarget: 'cluster:1', directoryPersonID: 7 });
  await controller.openTarget('cluster:1', {});
  const props = { client, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), directory, controller, urlState: state.current, target: 'cluster:1', filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() };
  let view = render(PeopleWorkspace, { props });
  try {
    if (failure === 'second person') {
      await screen.findByRole('heading', { name: 'Synthetic One' });
      if (remount) { state.commitNavigation({ workspace: 'everything' }); view.unmount(); }
      state.commitNavigation({ workspace: 'relationships', directoryPersonID: 9, relationshipTarget: null });
      if (remount) view = render(PeopleWorkspace, { props: { ...props, urlState: state.current, target: null } });
      else await view.rerender({ urlState: state.current, target: null });
    }
    await waitFor(() => expect(directory.detailLoading).toBe(false));
    expect(await screen.findByText(new RegExp(failure === 'second person' ? 'Could not load selected person' : 'Participant unavailable'))).toBeDefined();
    expect(screen.queryByText('Loading person…')).toBeNull();
    if (failure === 'second person') expect(screen.queryByRole('heading', { name: 'Synthetic One' })).toBeNull();
    failed = false;
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(screen.queryByText(/Participant unavailable|Could not load selected person/)).toBeNull());
    if (failure === 'second person') expect(await screen.findByRole('heading', { name: 'Synthetic Two' })).toBeDefined();
    else expect(await screen.findByRole('radio', { name: 'Messages' })).toBeDefined();
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
  const client = clientFor(routes); const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client, undefined, () => void controller.loadList({})); const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', directoryPersonID: 7 });
  const onNavigate = vi.fn(), onDuplicatesChanged = vi.fn();
  const view = render(PeopleWorkspace, { props: { client, reviews: new DirectoryReviewController(client), onDuplicatesChanged, directory, controller, urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate, onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
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

it.each([false, true])('refreshes duplicates and keeps the split receipt after source binding changes: saved=%s', async savedEntry => {
  let split = false;
  const saved = () => profile(7, split ? 5 : 4, split ? [] : [1]);
  const merge = mergeResult(saved()).merge;
  const client = clientFor({
    '/api/v1/people/7': () => Response.json(saved(), { headers: { ETag: `"person-7-r${saved().revision}"` } }),
    '/api/v1/people/7/profile': () => Response.json(emptyProfile(saved()), { headers: { ETag: `"person-7-r${saved().revision}"` } }),
    '/api/v1/people/7/profile/history': () => Response.json({ ...emptyProfile(saved()), observations: [] }),
    '/api/v1/people/7/brief-enrollment': () => Response.json({ person_id: 7, enrolled: !split, enabled_at: split ? null : when, actor: 'api' }),
    '/api/v1/people/7/brief': () => Response.json({ error: 'person_brief_not_found', message: 'Person brief not found' }, { status: 404 }),
    '/api/v1/people/7/merges': () => Response.json({ merges: [{ merge, participant_count: 1, row_count: 0, pending_candidate_count: 0, split_count: 0, row_action_counts: {} }] }),
    '/api/v1/person-merges/41': () => Response.json({ merge, participants: [{ merge_id: 41, participant_id: 1, origin_side: 'absorbed' }], rows: [], splits: [], review_candidates: [] }),
    '/api/v1/people/7/split': () => { split = true; return Response.json({ source_person: saved(), new_person: profile(9, 1, [1]), split: { id: 55, merge_id: 41, source_person_id: 7, new_person_id: 9, new_person_uid: 'synthetic-9', source_revision_before: 4, source_revision_after: 5, exact_reversal: false, actor: 'web', created_at: when }, exact_reversal: false, ambiguous_rows: [{ table_name: 'person_names', original_row_key: 'synthetic-row', action: 'ambiguous' }], unrestored_rows: [], uid_alias_disposition: 'restored', identity_revision: 2, cache_state: 'ready' }); },
    '/api/v1/participants/1': () => Response.json({ id: 1, display_label: 'Synthetic One', profile: split ? profile(9, 1, [1]) : saved(), identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }),
    '/api/v1/relationships/1/calendar': () => Response.json(emptyCalendar(2026)),
  });
  const directory = new DirectoryController(client); const controller = new RelationshipsController(client, () => 'UTC'); const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', directoryPersonID: savedEntry ? 7 : null, relationshipTarget: 'cluster:1' });
  await controller.openTarget('cluster:1', {});
  const onDuplicatesChanged = vi.fn();
  const view = render(PeopleWorkspace, { props: { client, reviews: new DirectoryReviewController(client), onDuplicatesChanged, directory, controller, urlState: state.current, target: 'cluster:1', filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: patch => { state.replaceCommittedNavigation(patch); void Promise.resolve().then(() => view.rerender({ urlState: state.current, target: state.current.relationshipTarget })); }, onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await fireEvent.click(await screen.findByRole('radio', { name: 'Details' }));
    const enrollment = await screen.findByRole('switch', { name: 'Enroll this person in briefs' }) as HTMLInputElement;
    await waitFor(() => expect(enrollment.checked).toBe(true));
    await fireEvent.click(await screen.findByRole('button', { name: 'More actions' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: 'History' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Inspect merge 41' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Split merged profile' }));
    await fireEvent.click(await screen.findByRole('checkbox', { name: 'Participant 1' }));
    await fireEvent.click(screen.getByRole('checkbox', { name: /I confirm splitting/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Create restored person' }));
    await waitFor(() => expect(controller.detail).toMatchObject({ profile: { id: 9 } }));
    expect(await screen.findByRole('heading', { name: 'Split completed' })).toBeDefined();
    expect(onDuplicatesChanged).toHaveBeenCalled();
    expect(state.current.relationshipTarget).toBeNull();
    await waitFor(() => expect((screen.getByLabelText('Enroll this person in briefs') as HTMLInputElement).checked).toBe(false));
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
  onTestFinished(() => { controller.destroy(); directory.destroy(); state.destroy(); });
  state.commitNavigation({ workspace: 'relationships', relationshipTarget: 'cluster:11' });
  await controller.openTarget('cluster:11', {});
  const view = render(PeopleWorkspace, { props: { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: 'cluster:11', filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: 'Save' }));
    await waitFor(() => expect(pending).toBeDefined());
  } finally { view.unmount(); }
  expect(pending!.signal.aborted).toBe(true);
});

it.each(['settled', 'pending', 'all pairs link'] as const)('preserves bulk-link work with stale-cache outcome: %s', async phase => {
  const pairs: Array<{ participant_a: number; participant_b: number }> = [];
  const names = phase !== 'pending' ? ['Alice Example', 'Bob Example', 'Cara Example'] : ['Alice Example', 'Bob Example'];
  let cacheReady = false;
  let resolveRetry: ((response: Response) => void) | undefined;
  const client = clientFor({
    '/api/v1/relationships': () => Response.json({ rows: names.map((display_label, index) => ({ canonical_id: index + 1, display_label, last_at: when, member_ids: cacheReady && index === 0 ? [1, 2] : [index + 1], score: 2, signals: { last_interaction_at: when, meeting_count: 0, meetings_together: 0, modalities: 2, received_from_them: 1, sent_count: 3, sent_to_them: 1 } })).filter(row => !cacheReady || row.canonical_id !== 2) }),
    '/api/v1/identity/links': async request => {
      pairs.push(await request.json());
      if (phase === 'pending' && pairs.length === 2) return new Promise<Response>(resolve => { resolveRetry = resolve; });
      if (phase === 'settled' && pairs.length === 2) return Response.json({ error: 'internal_error', message: 'Cara link failed' }, { status: 500 });
      cacheReady = pairs.length > 1;
      return Response.json({ identity_revision: 2, cache_state: cacheReady ? 'ready' : 'stale' });
    },
  });
  const controller = new RelationshipsController(client, () => 'UTC'), directory = new DirectoryController(client), state = new ExploreState(null);
  const view = render(PeopleWorkspace, { props: { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: null, filesOpen: false, predicate: { filters: [], presentation: 'table' }, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    for (const name of names) await fireEvent.click(await screen.findByRole('checkbox', { name: `Select ${name}` }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    if (phase === 'all pairs link') {
      await waitFor(() => expect(pairs).toHaveLength(2));
      await waitFor(() => expect(screen.queryByRole('button', { name: 'Clear selection' })).toBeNull());
      expect(screen.queryByRole('button', { name: 'Retry identity cache' })).toBeNull();
      return;
    }
    const retry = await screen.findByRole('button', { name: 'Retry identity cache' });
    if (phase === 'pending') for (const name of names) await fireEvent.click(await screen.findByRole('checkbox', { name: `Select ${name}` }));
    else {
      await waitFor(() => expect(screen.getByRole('status').textContent).toContain('1 of 2 linked.'));
      expect(pairs).toEqual([{ participant_a: 1, participant_b: 2 }, { participant_a: 1, participant_b: 3 }]);
    }
    await fireEvent.click(retry);
    if (phase === 'pending') {
      await waitFor(() => expect(pairs).toHaveLength(2));
      await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
      expect(pairs).toHaveLength(2);
    } else {
      await waitFor(() => expect(screen.queryByRole('button', { name: 'Retry identity cache' })).toBeNull());
      expect(pairs).toEqual([{ participant_a: 1, participant_b: 2 }, { participant_a: 1, participant_b: 3 }, { participant_a: 1, participant_b: 2 }]);
      expect.soft(screen.queryByRole('checkbox', { name: /select Bob Example/i })).toBeNull();
      for (const name of ['Alice Example', 'Cara Example']) expect.soft(screen.queryByRole('checkbox', { name: `Unselect ${name}` })).not.toBeNull();
      expect.soft(screen.queryByRole('button', { name: 'Clear selection' })).not.toBeNull();
      expect.soft(screen.getByRole('status').textContent).toContain('1 of 2 linked.');
      expect.soft(screen.getByRole('status').textContent).not.toContain('People are now treated as the same person.');
    }
  } finally {
    resolveRetry?.(Response.json({ identity_revision: 2, cache_state: 'ready' }));
    view.unmount(); controller.destroy(); directory.destroy(); state.destroy();
  }
});

it.each(['remount', 'restoration epoch'])('refreshes retained Saved rows after %s', async entry => {
  let synced = false;
  const client = clientFor({ '/api/v1/people/directory': () => Response.json({ people: [directoryPerson(7, { display_name: synced ? 'After sync' : 'Before sync' })] }) });
  const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true });
  const directory = new DirectoryController(client), controller = new RelationshipsController(client, () => 'UTC');
  const props = { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() };
  let view = render(PeopleWorkspace, { props });
  try {
    await within(await screen.findByRole('grid', { name: 'People results' })).findByText('Before sync');
    if (entry === 'remount') view.unmount();
    synced = true;
    if (entry === 'remount') view = render(PeopleWorkspace, { props });
    else await view.rerender({ ...props, urlState: { ...state.current }, restorationEpoch: 1 });
    const grid = await screen.findByRole('grid', { name: 'People results' });
    await within(grid).findByText('After sync');
    expect(within(grid).queryByText('Before sync')).toBeNull();
  } finally { view.unmount(); directory.destroy(); controller.destroy(); state.destroy(); }
});

it('keeps the survivor choice open after promoting the observed person during Merge', async () => {
  let promoted = false;
  let finishHydration!: () => void;
  const hydration = new Promise<void>(resolve => { finishHydration = resolve; });
  let createdReads = 0;
  const hydrationStarted = vi.fn();
  const saved = profile(7, 4, []), created = profile(9, 1, [1]);
  const client = clientFor({
    '/api/v1/participants/1': () => Response.json({ id: 1, display_label: 'Observed One', identifiers: [], activity_count: 1, file_count: 0, first_at: when, last_at: when, source_counts: [], ...(promoted ? { profile: created } : {}) }),
    '/api/v1/people/directory': () => Response.json({ people: [directoryPerson(7)] }),
    '/api/v1/participants/search': () => Response.json({ rows: [] }),
    '/api/v1/people': async request => { expect(await request.json()).toEqual({ participant_id: 1 }); promoted = true; return Response.json(created); },
    '/api/v1/people/7': () => Response.json(saved, { headers: { ETag: '"person-7-r4"' } }),
    '/api/v1/people/9': async () => {
      if (++createdReads === 1) return Response.json(created, { headers: { ETag: '"person-9-r1"' } });
      hydrationStarted();
      await hydration;
      return Response.json(created, { headers: { ETag: '"person-9-r1"' } });
    },
  });
  const controller = new RelationshipsController(client, () => 'UTC'), directory = new DirectoryController(client), state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipTarget: 'cluster:1' });
  await controller.openTarget('cluster:1', {});
  const view = render(PeopleWorkspace, { props: { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: 'cluster:1', filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: 'Merge with…' }));
    await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'Synthetic' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Synthetic One/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    await waitFor(() => expect(hydrationStarted).toHaveBeenCalled());
    await act(async () => finishHydration());
    await waitFor(() => expect(directory.detailLoading).toBe(false));
    expect(await screen.findByRole('dialog', { name: 'Resolve person merge' })).toBeDefined();
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('Escape dismisses bulk Delete while retaining the selected saved person', async () => {
  const client = clientFor({
    '/api/v1/people/directory': () => Response.json({ people: [directoryPerson(7)] }),
    '/api/v1/people/7': () => Response.json(profile(7, 4, []), { headers: { ETag: '"person-7-r4"' } }),
  });
  const controller = new RelationshipsController(client, () => 'UTC'), directory = new DirectoryController(client), state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true, directoryPersonID: 7 });
  const view = render(PeopleWorkspace, { props: { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: patch => { state.commitNavigation(patch); void view.rerender({ urlState: state.current }); }, onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await screen.findByRole('heading', { name: 'Synthetic One' });
    await fireEvent.click(await screen.findByRole('checkbox', { name: 'Select Synthetic One' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    const confirm = await screen.findByRole('button', { name: 'Confirm delete' });
    confirm.focus();
    await fireEvent.keyDown(document.activeElement!, { key: 'Escape' });
    expect.soft(state.current.directoryPersonID).toBe(7);
    expect.soft(screen.queryByRole('heading', { name: 'Synthetic One' })).not.toBeNull();
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Delete people' })).toBeNull());
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it.each(['Synthetic One', ''])('identifies a failed bulk deletion with display name %j', async displayName => {
  const deletes: Request[] = [];
  const client = clientFor({
    '/api/v1/people/directory': () => Response.json({ people: [directoryPerson(7, { display_name: displayName })] }),
    '/api/v1/people/7': request => { deletes.push(request); throw new Error('Connection lost'); },
  });
  const controller = new RelationshipsController(client, () => 'UTC'), directory = new DirectoryController(client), state = new ExploreState(null);
  const reviews = new DirectoryReviewController(client);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true });
  const view = render(PeopleWorkspace, { props: { client, controller, directory, reviews, onDuplicatesChanged: vi.fn(), urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await fireEvent.click(await screen.findByRole('checkbox', { name: `Select ${displayName || 'Person 7'}` }));
    await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    const dialog = await screen.findByRole('dialog', { name: 'Delete people' });
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Confirm delete' }));
    expect(await screen.findByText(`0 deleted. ${displayName || 'Person 7'}: Connection lost`)).toBeDefined();
    expect(deletes).toHaveLength(1);
    expect(deletes[0]!.method).toBe('DELETE');
    expect(deletes[0]!.headers.get('If-Match')).toBe('"person-7-r4"');
  } finally { view.unmount(); reviews.destroy(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('refreshes duplicates after Details unlink and deletion of only loaded saved profiles', async () => {
  const saved = profile(9, 2, [1, 2]);
  let unlinked = false;
  const deletes: number[] = [];
  const onDuplicatesChanged = vi.fn();
  const client = clientFor({
    '/api/v1/people/directory': request => Response.json({ people: [directoryPerson(new URL(request.url).searchParams.has('cursor') ? 9 : 7)], ...(new URL(request.url).searchParams.has('cursor') ? {} : { next_cursor: 'second' }) }),
    '/api/v1/people/7': request => { expect(request.method).toBe('DELETE'); deletes.push(7); return new Response(null, { status: 204 }); },
    '/api/v1/people/9': request => { if (request.method === 'DELETE') { deletes.push(9); return new Response(null, { status: 204 }); } return Response.json(saved, { headers: { ETag: '"person-9-r2"' } }); },
    '/api/v1/people/9/profile': () => Response.json(emptyProfile(saved), { headers: { ETag: '"person-9-r2"' } }),
    '/api/v1/participants/1': () => Response.json({ id: 1, display_label: 'Synthetic Two', profile: saved, identifiers: [{ participant_id: 2, type: 'email', value: 'other@example.com', is_primary: false, provenance: 'participant_identifiers' }], cluster: { member_ids: [1, 2], members: [], edges: unlinked ? [] : [{ participant_a: 1, participant_b: 2 }] }, first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }),
    '/api/v1/relationships/1/calendar': async request => Response.json(emptyCalendar((await request.json()).year)),
    '/api/v1/identity/unlinks': async request => { expect(await request.json()).toEqual({ participant_a: 1, participant_b: 2 }); unlinked = true; return Response.json({ identity_revision: 2, cache_state: 'ready' }); },
  });
  const directory = new DirectoryController(client), controller = new RelationshipsController(client, () => 'UTC'), state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true });
  const navigate = (patch: Parameters<typeof state.commitNavigation>[0]) => { state.commitNavigation(patch); void view.rerender({ urlState: state.current, target: state.current.relationshipTarget }); if (patch.relationshipTarget) void controller.openTarget(patch.relationshipTarget, {}); };
  const view = render(PeopleWorkspace, { props: { client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged, urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: navigate, onReplaceNavigation: navigate, onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await screen.findByRole('checkbox', { name: 'Select Synthetic One' });
    await directory.loadNextPage();
    await fireEvent.click(within(screen.getByRole('grid', { name: 'People results' })).getByText('Synthetic Two'));
    await fireEvent.click(await screen.findByRole('radio', { name: 'Details' }));
    await fireEvent.click(await screen.findByRole('checkbox', { name: 'Select Synthetic Two' }));
    expect(screen.getByRole('button', { name: 'Delete' })).toBeDefined();
    await fireEvent.click(await screen.findByRole('button', { name: 'Unlink other@example.com' }));
    await fireEvent.click(within(screen.getByRole('group', { name: 'Confirm unlinking other@example.com' })).getByRole('button', { name: 'Unlink' }));
    await waitFor(() => expect(unlinked).toBe(true));
    await waitFor(() => expect(directory.rows.map(row => row.id)).toEqual([7]));
    expect(onDuplicatesChanged).toHaveBeenCalledOnce();
    expect(screen.queryByRole('button', { name: 'Delete' })).toBeNull();
    expect(deletes).toEqual([]);
    expect(screen.getByRole('button', { name: 'Clear selection' })).toBeDefined();
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Select Synthetic One' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    const dialog = await screen.findByRole('dialog', { name: 'Delete people' });
    expect(dialog.textContent).toContain('This permanently deletes 1 saved profiles');
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Confirm delete' }));
    await waitFor(() => expect(deletes).toEqual([7]));
    await waitFor(() => expect(onDuplicatesChanged).toHaveBeenCalledTimes(2));
  } finally { view.unmount(); directory.destroy(); controller.destroy(); state.destroy(); }
});

it.each([true, false])('keeps the reopened People selection when an earlier merge refresh finishes, Saved=%s', async saved => {
  let merged = false, refreshStarted = false;
  let finishRefresh!: () => void;
  const pending = new Promise<void>(resolve => { finishRefresh = resolve; });
  const one = profile(7, 4, []), two = profile(9, 2, []), unrelated = profile(42, 1, [], 'Unrelated person');
  const survivor = profile(7, 5, []);
  const client = clientFor({
    '/api/v1/people/directory': async request => {
      if (merged && !refreshStarted) {
        refreshStarted = true;
        await new Promise<void>((resolve, reject) => {
          request.signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true });
          void pending.then(resolve);
        });
      }
      return Response.json({ people: [directoryPerson(7, { revision: merged ? 5 : 4 }), ...(!merged ? [directoryPerson(9)] : []), directoryPerson(42, { display_name: unrelated.display_name })] });
    },
    '/api/v1/people/7': () => Response.json(merged ? survivor : one, { headers: { ETag: merged ? '"person-7-r5"' : '"person-7-r4"' } }),
    '/api/v1/people/9': () => Response.json(two, { headers: { ETag: '"person-9-r2"' } }),
    '/api/v1/people/42': () => Response.json(unrelated, { headers: { ETag: '"person-42-r1"' } }),
    '/api/v1/people/7/merge': () => { merged = true; return Response.json(mergeResult(survivor), { headers: { ETag: '"person-7-r5"' } }); },
  });
  const controller = new RelationshipsController(client, () => 'UTC'), directory = new DirectoryController(client), state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: saved, directoryPersonID: 7 });
  const refresh = vi.spyOn(directory, 'reloadFirstPage');
  const props = () => ({ client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn(), onOpenDirectoryPerson: vi.fn() });
  let view = render(PeopleWorkspace, { props: props() });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: 'Merge with…' }));
    await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'Synthetic' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Synthetic Two/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    await fireEvent.click(await screen.findByRole('radio', { name: 'Synthetic One (Person 7)' }));
    await fireEvent.click(screen.getByRole('checkbox', { name: /I understand this consolidates both profiles/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
    await waitFor(() => expect(refreshStarted).toBe(true));
    const oldRefresh = refresh.mock.results.at(-1)!.value;
    view.unmount();
    state.commitNavigation({ workspace: 'sources' });
    state.commitNavigation({ workspace: 'relationships', relationshipSaved: saved, directoryPersonID: 7 });
    view = render(PeopleWorkspace, { props: props() });
    await screen.findByRole('heading', { name: 'Synthetic One' });
    state.commitNavigation({ directoryPersonID: 42 });
    await view.rerender({ urlState: state.current });
    await screen.findByRole('heading', { name: 'Unrelated person' });
    await act(async () => { finishRefresh(); await oldRefresh; });
    expect(directory.selectedPersonID).toBe(42);
    expect(state.current.directoryPersonID).toBe(42);
    expect(screen.getByRole('heading', { name: 'Unrelated person' })).toBeDefined();
    expect(screen.queryByText('Loading person…')).toBeNull();
    expect(directory.rows.find(person => person.id === 7)?.revision).toBe(5);
  } finally { finishRefresh(); view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});
