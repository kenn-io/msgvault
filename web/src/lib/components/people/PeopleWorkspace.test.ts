import { openTypeahead } from '../../../test/kit-ui';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it, vi, onTestFinished } from 'vitest';
import { createAPIClient } from '../../api/client';
import { RelationshipsController } from '../../relationships/controller.svelte';
import { DirectoryController } from '../../directory/controller.svelte';
import { ExploreState } from '../../explore/state.svelte';
import { meetingFixtureResponse } from '../../meetings/fixtures.test-support';
import PeopleWorkspace from './PeopleWorkspace.svelte';
import { emptyCalendar } from './fixtures.test-support';

const when = '2026-07-19T10:00:00Z';
function clientFor(routes: Record<string, (request: Request) => Promise<Response> | Response>) {
  return createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    return routes[path]?.(request) ?? meetingFixtureResponse(path) ?? Response.json({ rows: [] });
  });
}
const profile = (id: number, revision: number, participant_ids: number[]) => ({ id, revision, participant_ids, display_name: id === 7 ? 'Synthetic One' : 'Synthetic Two', created_at: when, updated_at: when, vcard_uid: `synthetic-${id}` });

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
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: state.current, target: 'cluster:1', filesOpen: false, predicate,
    onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    if (kind === 'missing') {
      expect(await screen.findByText('Person or domain not found')).toBeDefined();
      expect(screen.queryByRole('region', { name: 'Relationship activity calendar' })).toBeNull();
    } else {
      expect(await screen.findByRole('heading', { name: 'Synthetic contact' })).toBeDefined();
      expect(Boolean(screen.queryByRole('region', { name: 'Relationship activity calendar' }))).toBe(kind === 'filtered');
      await fireEvent.click(screen.getByRole('radio', { name: 'Details' }));
      expect(await screen.findByText(kind === 'draft' ? 'No sent or received messages with this contact yet.' : '0 items')).toBeDefined();
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
      expect(screen.getByRole('radio', { name: 'Details' }).getAttribute('aria-checked')).toBe('true');
    }
    await fireEvent.click(screen.getByRole('radio', { name: 'Messages' }));
    expect(screen.queryByRole('button', { name: 'Clear date' })).not.toBeNull();
    await fireEvent.click(screen.getByRole('radio', { name: 'Files' }));
    await view.rerender({ filesOpen: true });
    expect((await screen.findByRole('searchbox', { name: 'Filter filename' }) as HTMLInputElement).value).toBe('report');
    expect((screen.getByRole('checkbox', { name: 'PDFs' }) as HTMLInputElement).checked).toBe(false);
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
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: state.current, target: 'cluster:1', filesOpen: false, predicate: {}, onNavigate, onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
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

it.each(['is cancelled', 'clears the stale warning', 'merges'])('handles the next pair when it %s', async (outcome) => {
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
      return calls === 1 || outcome === 'clears the stale warning' ? Response.json({ identity_revision: 5, cache_state: calls === 1 && outcome === 'clears the stale warning' ? 'stale' : 'ready' }) : Response.json({ error: 'person_merge_required', message: 'Choose a survivor', profiles: [{ etag: '"person-7-r4"', person: profile(7, 4, [1, 2]) }, { etag: '"person-9-r2"', person: profile(9, 2, [3]) }] }, { status: 409 });
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
    } else {
      await waitFor(() => expect(screen.queryByRole('button', { name: 'Clear selection' })).toBeNull());
      expect(screen.queryByRole('button', { name: 'Retry identity cache' })).toBeNull();
    }
    if (outcome === 'is cancelled') expect(screen.getByRole('status').textContent).toContain('1 of 2');
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

it("passes the saved person's durable ID to Files when the first binding is draft-only", async () => {
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
    if (path === '/api/v1/participants/1') return Response.json({ error: 'not_found' }, { status: 404 });
    if (path === '/api/v1/relationships/1/calendar') return Response.json(emptyCalendar(2026));
    if (path === '/api/v1/participants/1/files/search') return Response.json({ files: [first], total_count: 1, cache_revision: 'cache-1', search_provenance: {} });
    if (path === '/api/v1/people/91/files/search') return Response.json({ files: [second], total_count: 1, cache_revision: 'cache-1', search_provenance: {} });
    return meetingFixtureResponse(path) ?? Response.json({ rows: [] });
  });
  const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true, directoryPersonID: 91, relationshipTarget: 'cluster:1', relationshipFiles: true });
  const directory = new DirectoryController(client);
  const controller = new RelationshipsController(client, () => 'UTC');
  await controller.openTarget('cluster:1', {});
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: state.current, target: 'cluster:1', filesOpen: true, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
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
  const one = { ...profile(7, 1, [1]), organizations: [], categories: [], contact_state: 'active', last_contact_at: '2019-07-18T10:00:00Z' };
  const two = { ...profile(9, 1, [4]), organizations: [], categories: [], contact_state: 'active', last_contact_at: '2020-07-18T10:00:00Z' };
  const client = clientFor({
    '/api/v1/people/directory': request => {
      sorts.push(new URL(request.url).searchParams.get('sort'));
      return Response.json({ people: linked ? [{ ...one, participant_ids: [1, 2], last_contact_at: '2021-07-18T10:00:00Z' }, two] : [two, one] });
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
  const props = { client, controller, directory, urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: (patch: Parameters<ExploreState['commitNavigation']>[0]) => state.commitNavigation(patch), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() };
  let view = render(PeopleWorkspace, { props });
  try {
    const list = within(await screen.findByRole('grid', { name: 'People results' }));
    await list.findByText('2019');
    expect(list.getAllByRole('row').map(row => row.textContent)).toEqual([expect.stringMatching(/Synthetic Two.*2020/), expect.stringMatching(/Synthetic One.*2019/)]);
    await fireEvent.click(screen.getByRole('button', { name: 'Remove Saved' }));
    await view.rerender({ urlState: state.current });
    for (const name of batch === 'partial' ? ['Synthetic One', 'Bob Example', 'Cara Example'] : ['Synthetic One', 'Bob Example']) await fireEvent.click(await screen.findByRole('checkbox', { name: `Select ${name}` }));
    await fireEvent.click(screen.getByRole('button', { name: 'Same person' }));
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
      await screen.findByText(batch === 'partial' ? '1 of 2 linked. Synthetic link failed' : 'People are now treated as the same person.');
      await fireEvent.click(screen.getByRole('button', { name: /Filter/ }));
      await fireEvent.click(await screen.findByText('Saved', { exact: true }));
      await view.rerender({ urlState: state.current });
    }
    expect(linked).toBe(true);
    await waitFor(() => expect(within(screen.getByRole('grid', { name: 'People results' })).getAllByRole('row').map(row => row.textContent)).toEqual([expect.stringMatching(/Synthetic One.*2021/), expect.stringMatching(/Synthetic Two.*2020/)]));
    expect(sorts).toEqual(['last_contact_desc', 'last_contact_desc']);
    expect(pairs).toEqual(batch === 'partial' ? [{ participant_a: 1, participant_b: 2 }, { participant_a: 1, participant_b: 3 }] : [{ participant_a: 1, participant_b: 2 }]);
  } finally { releaseLink?.(); view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it.each(['link', 'merge'])('refreshes the Saved list after a %s from Details', async (operation) => {
  let changed = false;
  let merged = false;
  const one = { ...profile(7, 4, [1]), organizations: [], categories: [], contact_state: 'active', last_contact_at: '2020-07-18T10:00:00Z' };
  const two = { ...profile(9, 2, [2]), organizations: [], categories: [], contact_state: 'active' };
  const client = clientFor({
    '/api/v1/people/directory': () => Response.json({ people: [{ ...one, last_contact_at: changed ? '2021-07-18T10:00:00Z' : one.last_contact_at }, ...(!merged && operation === 'merge' ? [two] : [])] }),
    '/api/v1/people/7': () => Response.json(one),
    '/api/v1/participants/1': () => Response.json({ id: 1, profile: { id: 7 }, display_label: 'Synthetic One', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }),
    '/api/v1/relationships/1/calendar': async request => Response.json({ ...emptyCalendar((await request.json()).year), identity_revision: changed ? 8 : 1 }),
    '/api/v1/participants/search': () => Response.json({ rows: [{ id: 2, display_label: 'Bob Example', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }], total_count: 1, cache_revision: 'cache-1', search_provenance: {} }),
    '/api/v1/identity/links': () => {
      if (operation === 'link' || merged) { changed = true; return Response.json({ identity_revision: 8, cache_state: 'ready' }); }
      return Response.json({ error: 'person_merge_required', message: 'Choose a survivor', profiles: [{ etag: '"person-7-r4"', person: one }, { etag: '"person-9-r2"', person: two }] }, { status: 409 });
    },
    '/api/v1/people/7/merge': () => { merged = true; changed = true; return Response.json({ cache_state: 'ready', identity_revision: 8, review_candidates: [], person: { ...one, revision: 5, participant_ids: [1, 2] }, merge: { id: 41, survivor_person_id: 7, absorbed_person_id: 9, current_person_id: 7, survivor_vcard_uid: 'synthetic-7', absorbed_vcard_uid: 'synthetic-9', survivor_revision_before: 4, absorbed_revision_before: 2, survivor_revision_after: 5, actor: 'web', snapshot_version: 1, snapshot_sha256: 'synthetic-digest', created_at: when } }, { headers: { ETag: '"person-7-r5"' } }); },
  });
  const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true, directoryPersonID: 7, relationshipTarget: 'cluster:1' });
  const directory = new DirectoryController(client);
  const controller = new RelationshipsController(client, () => 'UTC');
  await controller.openTarget('cluster:1', {});
  const onOpenDirectoryPerson = vi.fn();
  const props = { client, controller, directory, urlState: state.current, target: 'cluster:1', filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn(), onOpenDirectoryPerson };
  let view = render(PeopleWorkspace, { props });
  try {
    const list = within(await screen.findByRole('grid', { name: 'People results' }));
    await list.findByText('2020');
    if (operation === 'merge') await list.findByText('Synthetic Two');
    await fireEvent.click(await screen.findByRole('radio', { name: 'Details' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Same person…' }));
    const input = await openTypeahead('Search people to link');
    await fireEvent.input(input, { target: { value: 'Bob' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Bob Example/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'These are the same person' }));
    if (operation === 'merge') {
      await screen.findByRole('dialog', { name: 'Resolve person merge' });
      await fireEvent.click(screen.getByRole('radio', { name: 'Synthetic One (Person 7)' }));
      await fireEvent.click(screen.getByRole('checkbox', { name: /I understand this consolidates both profiles/ }));
      await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
      await waitFor(() => expect(merged).toBe(true));
      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Resolve person merge' })).toBeNull());
      expect(onOpenDirectoryPerson).toHaveBeenCalledWith(7);
    }
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Link another identity for Synthetic One' })).toBeNull());
    expect(changed).toBe(true);
    await list.findByText('2021');
    view.unmount();
    // Back returns to People with the same saved-list URL and shell-owned controllers.
    view = render(PeopleWorkspace, { props });
    await within(await screen.findByRole('grid', { name: 'People results' })).findByText('2021');
    if (operation === 'merge') expect(within(screen.getByRole('grid', { name: 'People results' })).queryByText('Synthetic Two')).toBeNull();
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it.each([false, true])('offers Retry without the previous person page when a Directory selection fails to load, remount: %s', async (remount) => {
  const client = clientFor({
    '/api/v1/people/directory': () => Response.json({ people: [{ ...profile(7, 1, [1]), organizations: [], categories: [], contact_state: 'active' }] }),
    '/api/v1/people/7': () => Response.json(profile(7, 1, [1])),
    '/api/v1/people/9': () => { throw new Error('Could not load selected person.'); },
    '/api/v1/participants/1': () => Response.json({ id: 1, display_label: 'Alice Example', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }),
    '/api/v1/relationships/1/calendar': async request => Response.json(emptyCalendar((await request.json()).year)),
  });
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSaved: true, relationshipTarget: 'cluster:1' });
  await controller.openTarget('cluster:1', {});
  const onNavigate = vi.fn(patch => state.commitNavigation(patch));
  const props = { client, controller, directory, urlState: state.current, target: 'cluster:1', filesOpen: false, predicate: {}, onNavigate, onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() };
  let view = render(PeopleWorkspace, { props });
  try {
    await fireEvent.click(await screen.findByText('Synthetic One'));
    await waitFor(() => expect(onNavigate).toHaveBeenCalledWith({ directoryPersonID: 7, relationshipTarget: 'cluster:1', relationshipFiles: false }));
    await view.rerender({ urlState: state.current });
    await screen.findByRole('heading', { name: 'Alice Example' });
    if (remount) { state.commitNavigation({ workspace: 'directory' }); view.unmount(); }
    state.commitNavigation({ directoryPersonID: 9 });
    state.commitNavigation({ workspace: 'relationships' });
    if (remount) view = render(PeopleWorkspace, { props: { ...props, urlState: state.current } }); else await view.rerender({ urlState: state.current });
    await screen.findByText('Could not load selected person.');
    expect.soft(screen.queryByRole('button', { name: 'Retry' })).not.toBeNull();
    expect.soft(screen.queryByRole('group', { name: 'Person page' })).toBeNull();
    expect.soft(screen.queryByRole('heading', { name: 'Alice Example' })).toBeNull();
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('loads a saved person deep link before showing its person page', async () => {
  let release!: (response: Response) => void;
  const pending = new Promise<Response>(resolve => release = resolve);
  const client = clientFor({
    '/api/v1/people/7': () => pending,
    '/api/v1/participants/1': () => Response.json({ id: 1, display_label: 'Alice Example', identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] }),
  });
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', directoryPersonID: 7, relationshipTarget: 'cluster:1' });
  await controller.openTarget('cluster:1', {});
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: state.current, target: 'cluster:1', filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await screen.findByText('Loading person…');
    expect(screen.queryByRole('group', { name: 'Person page' })).toBeNull();
    release(Response.json(profile(7, 1, [1])));
    await screen.findByRole('heading', { name: 'Alice Example' });
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
    expect(onNavigate.mock.calls).toEqual([[{ relationshipSaved: false }], [{ directoryPersonID: null, relationshipTarget: 'cluster:1', relationshipFiles: false }]]);
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('preserves the unfinished bulk link and selection after retrying a stale identity cache', async () => {
  const pairs: Array<{ participant_a: number; participant_b: number }> = [];
  const names = ['Alice Example', 'Bob Example', 'Cara Example'];
  let cacheReady = false;
  const client = clientFor({
    '/api/v1/relationships': () => Response.json({ rows: names.map((display_label, index) => ({ canonical_id: index + 1, display_label, last_at: when, member_ids: cacheReady && index === 0 ? [1, 2] : [index + 1], score: 2, signals: { last_interaction_at: when, meeting_count: 0, meetings_together: 0, modalities: 2, received_from_them: 1, sent_count: 3, sent_to_them: 1 } })).filter(row => !cacheReady || row.canonical_id !== 2) }),
    '/api/v1/identity/links': async request => {
      pairs.push(await request.json());
      if (pairs.length === 2) return Response.json({ error: 'internal_error', message: 'Cara link failed' }, { status: 500 });
      cacheReady = pairs.length > 1;
      return Response.json({ identity_revision: 2, cache_state: cacheReady ? 'ready' : 'stale' });
    },
  });
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: state.current, target: null, filesOpen: false, predicate: { filters: [], presentation: 'table' }, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    for (const name of names) await fireEvent.click(await screen.findByRole('checkbox', { name: `Select ${name}` }));
    await fireEvent.click(screen.getByRole('button', { name: 'Same person' }));
    await waitFor(() => expect(screen.getByRole('status').textContent).toContain('1 of 2 linked.'));
    expect(pairs).toEqual([{ participant_a: 1, participant_b: 2 }, { participant_a: 1, participant_b: 3 }]);
    await fireEvent.click(screen.getByRole('button', { name: 'Retry identity cache' }));
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Retry identity cache' })).toBeNull());
    expect(pairs).toEqual([{ participant_a: 1, participant_b: 2 }, { participant_a: 1, participant_b: 3 }, { participant_a: 1, participant_b: 2 }]);
    expect.soft(screen.queryByRole('checkbox', { name: /select Bob Example/i })).toBeNull();
    for (const name of ['Alice Example', 'Cara Example']) expect.soft(screen.queryByRole('checkbox', { name: `Unselect ${name}` })).not.toBeNull();
    expect.soft(screen.queryByRole('button', { name: 'Clear selection' })).not.toBeNull();
    expect.soft(screen.getByRole('status').textContent).toContain('1 of 2 linked.');
    expect.soft(screen.getByRole('status').textContent).not.toContain('People are now treated as the same person.');
  } finally { view.unmount(); controller.destroy(); directory.destroy(); state.destroy(); }
});

it('blocks a second bulk link while an identity cache retry is in flight', async () => {
  const pairs: unknown[] = [];
  let resolveRetry: ((response: Response) => void) | undefined;
  const client = clientFor({
    '/api/v1/participants/search': () => Response.json({ rows: ['Alice Example', 'Bob Example'].map((display_label, index) => ({ id: index + 1, display_label, identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] })), total_count: 2, cache_revision: 'cache-1', search_provenance: {} }),
    '/api/v1/identity/links': async request => {
      pairs.push(await request.json());
      if (pairs.length === 2) return new Promise<Response>(resolve => { resolveRetry = resolve; });
      return Response.json({ identity_revision: 2, cache_state: 'stale' });
    },
  });
  const state = new ExploreState(null);
  state.commitNavigation({ workspace: 'relationships', relationshipSort: 'name' });
  const directory = new DirectoryController(client);
  const controller = new RelationshipsController(client, () => 'UTC');
  const view = render(PeopleWorkspace, { props: { client, controller, directory, urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    for (const name of ['Alice Example', 'Bob Example']) await fireEvent.click(await screen.findByRole('checkbox', { name: `Select ${name}` }));
    await fireEvent.click(screen.getByRole('button', { name: 'Same person' }));
    const retry = await screen.findByRole('button', { name: 'Retry identity cache' });
    for (const name of ['Alice Example', 'Bob Example']) await fireEvent.click(await screen.findByRole('checkbox', { name: `Select ${name}` }));
    await fireEvent.click(retry);
    await waitFor(() => expect(pairs).toHaveLength(2));
    await fireEvent.click(screen.getByRole('button', { name: 'Same person' }));
    expect(pairs).toHaveLength(2);
  } finally {
    resolveRetry?.(Response.json({ identity_revision: 2, cache_state: 'ready' }));
    view.unmount(); controller.destroy(); directory.destroy(); state.destroy();
  }
});
