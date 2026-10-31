import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
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

it('keeps the batch locked while a profile merge reconciles, then finishes the queue', async () => {
  let listRequests = 0; let releaseReconcile: (() => void) | undefined;
  const links: Array<{ participant_a: number; participant_b: number }> = [];
  const client = clientFor({
    '/api/v1/relationships': async () => {
      if (++listRequests === 2) await new Promise<void>(resolve => { releaseReconcile = resolve; });
      return Response.json({ rows: ['Alice Example', 'Bob Example', 'Cara Example'].map((display_label, index) => ({ canonical_id: index + 1, display_label, last_at: when, member_ids: [index + 1], score: 2, signals: { last_interaction_at: when, meeting_count: 0, meetings_together: 0, modalities: 2, received_from_them: 1, sent_count: 3, sent_to_them: 1 } })) });
    },
    '/api/v1/identity/links': async request => {
      links.push(await request.json());
      return links.length === 1 ? Response.json({ error: 'person_merge_required', message: 'Choose a survivor', profiles: [{ etag: '"person-7-r4"', person: profile(7, 4, [1]) }, { etag: '"person-9-r2"', person: profile(9, 2, [2]) }] }, { status: 409 }) : Response.json({ identity_revision: 5, cache_state: 'ready' });
    },
    '/api/v1/people/7/merge': () => Response.json({ cache_state: 'ready', identity_revision: 8, review_candidates: [], person: profile(7, 5, [1, 2]), merge: { id: 41, survivor_person_id: 7, absorbed_person_id: 9, current_person_id: 7, survivor_vcard_uid: 'synthetic-7', absorbed_vcard_uid: 'synthetic-9', survivor_revision_before: 4, absorbed_revision_before: 2, survivor_revision_after: 5, actor: 'web', snapshot_version: 1, snapshot_sha256: 'synthetic-digest', created_at: when } }, { headers: { ETag: '"person-7-r5"' } })
  });
  const controller = new RelationshipsController(client, () => 'UTC'); const directory = new DirectoryController(client); const state = new ExploreState(null);
  render(PeopleWorkspace, { props: { client, controller, directory, urlState: state.current, target: null, filesOpen: false, predicate: { filters: [], presentation: 'table' }, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  onTestFinished(() => { controller.destroy(); directory.destroy(); state.destroy(); });
  for (const name of ['Alice Example', 'Bob Example', 'Cara Example']) await fireEvent.click(await screen.findByRole('checkbox', { name: `Select ${name}` }));
  await fireEvent.click(screen.getByRole('button', { name: 'Same person' }));
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
});
