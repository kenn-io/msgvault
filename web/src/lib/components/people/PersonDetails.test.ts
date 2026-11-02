import { render, screen, waitFor } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryController } from '../../directory/controller.svelte';
import PersonDetails from './PersonDetails.svelte';
import { meetingFixtureResponse } from '../../meetings/fixtures.test-support';
import MeetingPanel from '../meetings/MeetingPanel.svelte';

vi.mock('../meetings/MeetingPanel.svelte', { spy: true });

it('shows one alert for an attributes failure', async () => {
  const when = '2026-08-01T00:00:00Z';
  const person = { id: 7, revision: 1, display_name: 'Example Person', participant_ids: [1], vcard_uid: 'example-7', created_at: when, updated_at: when };
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/people/7') return Response.json(person, { headers: { ETag: '"person-7-r1"' } });
    if (path.endsWith('/profile')) return Response.json({ person, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] }, { headers: { ETag: '"profile-7-r1"' } });
    if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: when, stale: false });
    if (path.endsWith('/attributes')) return Response.json({ message: 'Attributes unavailable' }, { status: 503 });
    if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
    if (path.endsWith('/employments')) return Response.json({ employments: [] });
    if (path.endsWith('/relationships')) return Response.json({ relationships: [] });
    if (path.endsWith('/network')) return Response.json({ root_person_id: 7, depth: 1, nodes: [], edges: [], truncated: false });
    return meetingFixtureResponse(path) ?? Response.json({});
  });
  const directory = new DirectoryController(client);
  await directory.loadSelection(7);
  const view = render(PersonDetails, { client, directory, personID: 7 });
  try {
    const alerts = screen.getAllByRole('alert');
    expect(alerts).toHaveLength(1);
  } finally { view.unmount(); directory.destroy(); }
});

it.each([{ participantIDs: [] }, { participantIDs: [5] }])('forwards meeting revisions only for resolved identities ($participantIDs)', async ({ participantIDs }) => {
  vi.mocked(MeetingPanel).mockClear();
  const meetingRequests: Request[] = [];
  const when = '2026-08-01T00:00:00Z';
  const person = { id: 7, revision: 1, display_name: 'Example Person', participant_ids: participantIDs, vcard_uid: 'example-7', created_at: when, updated_at: when };
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/people/7') return Response.json(person, { headers: { ETag: '"person-7-r1"' } });
    if (path.endsWith('/profile')) return Response.json({ person, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] }, { headers: { ETag: '"profile-7-r1"' } });
    if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: when, stale: false });
    if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
    if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
    if (path.endsWith('/employments')) return Response.json({ employments: [] });
    if (path.endsWith('/relationships')) return Response.json({ relationships: [] });
    if (path.endsWith('/meetings/metrics') || path.endsWith('/meetings/actions')) meetingRequests.push(request);
    if (path.endsWith('/network')) return Response.json({ root_person_id: 7, depth: 1, nodes: [], edges: [], truncated: false });
    return meetingFixtureResponse(path) ?? Response.json({});
  });
  const directory = new DirectoryController(client);
  await directory.loadSelection(7);
  const view = render(PersonDetails, { client, directory, personID: 7 });
  try {
    if (participantIDs.length === 0) {
      expect(MeetingPanel).not.toHaveBeenCalled();
      expect(meetingRequests).toHaveLength(0);
      return;
    }
    expect(vi.mocked(MeetingPanel).mock.calls.at(-1)?.[1].refreshKey).toBe('1');
    person.revision = 2;
    await directory.profile!.reload();
    await waitFor(() => expect(vi.mocked(MeetingPanel).mock.calls.at(-1)?.[1].refreshKey).toBe('2'));
  } finally { view.unmount(); directory.destroy(); }
});
