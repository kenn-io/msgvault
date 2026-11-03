import { act, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { openTypeahead } from '../../../test/kit-ui';
import { createAPIClient } from '../../api/client';
import { DirectoryController } from '../../directory/controller.svelte';
import { DirectoryReviewController } from '../../directory/review-controller.svelte';
import { ExploreState } from '../../explore/state.svelte';
import { meetingFixtureResponse } from '../../meetings/fixtures.test-support';
import { RelationshipsController } from '../../relationships/controller.svelte';
import PeopleWorkspace from './PeopleWorkspace.svelte';
import { emptyCalendar } from './fixtures.test-support';

it('Back opens the survivor after Details absorbs the open saved profile', async () => {
  const when = '2026-07-19T10:00:00Z';
  const one = { id: 7, revision: 4, display_name: 'Synthetic One', participant_ids: [1], vcard_uid: 'synthetic-7', created_at: when, updated_at: when };
  const two = { ...one, id: 9, revision: 2, display_name: 'Synthetic Two', participant_ids: [2], vcard_uid: 'synthetic-9' };
  const survivor = { ...two, revision: 3, participant_ids: [1, 2] };
  let merged = false;
  const deletedReads: string[] = [];
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    const personID = /^\/api\/v1\/people\/(7|9)(?:\/|$)/.exec(path)?.[1];
    if (personID) {
      if (personID === '7' && merged) {
        deletedReads.push(path);
        return Response.json({ error: 'person_not_found' }, { status: 404 });
      }
      const person = personID === '7' ? one : merged ? survivor : two;
      if (path.endsWith('/profile')) return Response.json({ person, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] });
      if (path === `/api/v1/people/${personID}`) return Response.json(person);
    }
    if (path === '/api/v1/people/9/merge') {
      merged = true;
      return Response.json({ cache_state: 'ready', identity_revision: 8, review_candidates: [], person: survivor, merge: {
        id: 41, survivor_person_id: 9, absorbed_person_id: 7, current_person_id: 9,
        survivor_vcard_uid: 'synthetic-9', absorbed_vcard_uid: 'synthetic-7',
        survivor_revision_before: 2, absorbed_revision_before: 4, survivor_revision_after: 3,
        actor: 'web', snapshot_version: 1, snapshot_sha256: 'synthetic-digest', created_at: when,
      } }, { headers: { ETag: '"person-9-r3"' } });
    }
    if (path === '/api/v1/identity/links') return Response.json({ error: 'person_merge_required', message: 'Choose a survivor', profiles: [
      { etag: '"person-7-r4"', person: one }, { etag: '"person-9-r2"', person: two },
    ] }, { status: 409 });
    const participant = (id: number) => ({ id, display_label: id === 1 && !merged ? 'Synthetic One' : 'Synthetic Two', profile: merged ? survivor : id === 1 ? one : two,
      identifiers: [], first_at: when, last_at: when, activity_count: 1, file_count: 0, source_counts: [] });
    if (path === '/api/v1/participants/1') return Response.json(participant(1));
    if (path === '/api/v1/participants/search') return Response.json({ rows: [participant(2)], total_count: 1, cache_revision: 'cache-1', search_provenance: {} });
    if (path === '/api/v1/relationships/1/calendar') return Response.json(emptyCalendar((await request.json()).year));
    return meetingFixtureResponse(path) ?? Response.json({ rows: [] });
  });
  window.history.replaceState(null, '', '/');
  const state = new ExploreState();
  state.commitNavigation({ workspace: 'relationships', directoryPersonID: 7, relationshipTarget: 'cluster:1' });
  const controller = new RelationshipsController(client, () => 'UTC'), directory = new DirectoryController(client);
  await controller.openTarget('cluster:1', {});
  const onOpenDirectoryPerson = vi.fn((id: number) => state.commitNavigation({ workspace: 'relationships', directoryPersonID: id, relationshipTarget: null, relationshipFiles: false }));
  const props = () => ({ client, controller, directory, reviews: new DirectoryReviewController(client), onDuplicatesChanged: vi.fn(), urlState: state.current, target: state.current.relationshipTarget, filesOpen: false, predicate: {},
    onNavigate: vi.fn(), onReplaceNavigation: (patch: Parameters<typeof state.replaceCommittedNavigation>[0]) => {
      state.replaceCommittedNavigation(patch);
      void Promise.resolve().then(() => view.rerender({ urlState: state.current, target: state.current.relationshipTarget }));
    }, onOpenDirectoryPerson,
    onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() });
  let view = render(PeopleWorkspace, { props: props() });
  try {
    await screen.findByRole('heading', { name: 'Synthetic One' });
    await fireEvent.click(screen.getByRole('radio', { name: 'Details' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge with…' }));
    await fireEvent.input(await openTypeahead('Search people'), { target: { value: 'Synthetic Two' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Synthetic Two/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }));
    await screen.findByRole('dialog', { name: 'Resolve person merge' });
    await fireEvent.click(screen.getByRole('radio', { name: 'Synthetic Two (Person 9)' }));
    await fireEvent.click(screen.getByRole('checkbox', { name: /I understand this consolidates both profiles/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Merge into selected survivor' }));
    await waitFor(() => expect(onOpenDirectoryPerson).toHaveBeenCalledWith(9));
    view.unmount();
    await act(async () => {
      const popped = new Promise<void>(resolve => window.addEventListener('popstate', () => resolve(), { once: true }));
      window.history.back();
      await popped;
    });
    view = render(PeopleWorkspace, { props: props() });
    expect(state.current.directoryPersonID).toBe(9);
    expect(await screen.findByRole('heading', { name: 'Synthetic Two' })).toBeDefined();
    expect(screen.queryByText('Saved person not found.')).toBeNull();
    expect(deletedReads).toEqual([]);
  } finally {
    view.unmount(); controller.destroy(); directory.destroy(); state.destroy();
    window.history.replaceState(null, '', '/');
  }
});
