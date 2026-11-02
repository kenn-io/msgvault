import { emptyProfile } from '../../directory/profile-fixtures.test-support';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it, onTestFinished } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryController } from '../../directory/controller.svelte';
import PersonDetails from './PersonDetails.svelte';
import { meetingFixtureResponse } from '../../meetings/fixtures.test-support';
import { meetingActions, meetingAction } from '../../meetings/fixtures.test-support';

it('shows one alert for an attributes failure', async () => {
  await renderDetails([], { attributesFailure: true });
  expect(screen.getAllByRole('alert')).toHaveLength(1);
});

async function renderDetails(participantIDs: number[], options: { attributesFailure?: boolean; personFailure?: boolean } = {}) {
  const when = '2026-08-01T00:00:00Z';
  const person = { id: 7, revision: 1, display_name: 'Example Person', participant_ids: participantIDs, vcard_uid: 'example-7', created_at: when, updated_at: when };
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/people/7') return options.personFailure ? Response.json({ error: 'unavailable', message: 'Saved profile unavailable' }, { status: 503 }) : Response.json(person, { headers: { ETag: '"person-7-r1"' } });
    if (path.endsWith('/profile')) return Response.json(emptyProfile(person), { headers: { ETag: '"profile-7-r1"' } });
    if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: when, stale: false });
    if (path.endsWith('/attributes')) return options.attributesFailure ? Response.json({ message: 'Attributes unavailable' }, { status: 503 }) : Response.json({ person_id: 7, attributes: [] });
    if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
    if (path.endsWith('/employments')) return Response.json({ employments: [] });
    if (path.endsWith('/relationships')) return Response.json({ relationships: [] });
    if (path.endsWith('/network')) return Response.json({ root_person_id: 7, depth: 1, nodes: [], edges: [], truncated: false });
    if (path === '/api/v1/meetings/actions') {
      return Response.json(meetingActions({ rows: [meetingAction({ action: { ordinal: 0, locator: 'action:0', origin: 'source', title: `Revision ${person.revision} follow-up`, status: 'pending' } })] }));
    }
    return meetingFixtureResponse(path) ?? Response.json({});
  });
  const directory = new DirectoryController(client);
  await directory.loadSelection(7);
  const view = render(PersonDetails, { client, directory, personID: 7 });
  onTestFinished(() => { view.unmount(); directory.destroy(); });
  return { person, directory };
}

it('retains a saved profile failure and recovers through section Retry', async () => {
  const options = { personFailure: true };
  await renderDetails([1], options);
  expect(await screen.findByText('Saved profile unavailable')).toBeDefined();
  expect(screen.queryByText('Loading person…')).toBeNull();
  options.personFailure = false;
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  await waitFor(() => expect(screen.queryByText('Saved profile unavailable')).toBeNull());
});

it('omits the meeting panel for a person without resolved identities', async () => {
  await renderDetails([]);
  expect(screen.queryByRole('region', { name: 'Meeting activity' })).toBeNull();
});

it('forwards the refreshed meeting revision for a person with resolved identities', async () => {
  const { person, directory } = await renderDetails([5]);
  expect(await screen.findByText('Revision 1 follow-up')).toBeDefined();
  person.revision = 2;
  await directory.profile!.reload();
  expect(await screen.findByText('Revision 2 follow-up')).toBeDefined();
  expect(screen.queryByText('Revision 1 follow-up')).toBeNull();
});
