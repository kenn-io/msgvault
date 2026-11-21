import { emptyProfile } from '../../directory/profile-fixtures.test-support';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it, onTestFinished, vi } from 'vitest';
import PersonMeetings from './PersonMeetings.svelte';
vi.mock('./PersonMeetings.svelte', { spy: true });
import { createAPIClient } from '../../api/client';
import { DirectoryController } from '../../directory/controller.svelte';
import PersonDetails from './PersonDetails.svelte';
import { meetingFixtureResponse } from '../../meetings/fixtures.test-support';
import { meetingActions, meetingAction } from '../../meetings/fixtures.test-support';

it('shows one alert for an attributes failure', async () => {
  await renderDetails([], { attributesFailure: true });
  expect(screen.getAllByRole('alert')).toHaveLength(1);
});

async function renderDetails(participantIDs: number[], options: { meetingRequests?: Request[]; attributesFailure?: boolean; personFailure?: boolean; personDeleted?: boolean; detailsFailure?: boolean; pins?: 'save' | 'clear'; taskChange?: 'merge' | 'split'; loops?: boolean } = {}) {
  const when = '2026-08-01T00:00:00Z';
  const person = { id: 7, revision: 1, display_name: 'Example Person', participant_ids: participantIDs, vcard_uid: 'example-7', created_at: when, updated_at: when };
  let pinned = false;
  const definition = { id: 8, universal_id: '00000000-0000-4000-8000-000000000008', slug: 'location', label: 'Location', value_type: 'text', field_type: 'text', cardinality: 'single', object_type: 'person', ownership: 'user', display_order: 1, is_active: true, api_mutable: true, ui_editable: true, ui_creatable: true, is_sensitive: false, revision: 1 };
  const value = { id: 41, person_id: 7, definition_id: 8, definition_slug: 'location', ordinal: 0, value: { type: 'text', text: 'Example City' }, active_from: when, created_at: when, source: 'user' };
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (options.personDeleted && /^\/api\/v1\/people\/7(?:\/|$)/.test(path)) return Response.json({ error: 'person_profile_not_found', message: 'Person profile not found' }, { status: 404 });
    if (path.startsWith('/api/v1/meetings/')) options.meetingRequests?.push(request.clone());
    if (path === '/api/v1/integrations/kata/status' && options.taskChange) return Response.json({ state: 'ready' });
    if (path === '/api/v1/people/7/agenda' && options.taskChange) {
      const hasTask = options.taskChange === 'merge' ? person.revision > 1 : person.revision === 1;
      return Response.json({ project: 'msgvault', items: hasTask ? [{ uid: 'task-one', ref: 'one', title: 'Ask about the proposal', list: 'agenda', state: 'open', status: 'open' }] : [] });
    }
    if (path === '/api/v1/loops' && options.loops) {
      const loop = { kind: 'reply_owed', anchor_message_id: 40 + person.revision, question_message_ids: [40 + person.revision], conversation_id: 3, source_id: 1, channel: 'email',
        counterparty: { participant_id: 5, display_name: 'Example Contact', address: 'contact@example.test' }, summary: `Revision ${person.revision} question?`, owed_since_at: when, last_activity_at: when };
      return Response.json({ items: [loop], truncated: false, skipped_unparseable: 0, coverage_incomplete: false });
    }
    if (path === '/api/v1/people/directory') return Response.json({ people: [person] });
    if (path.endsWith('/attributes/location') && request.method === 'PUT') { pinned = true; return Response.json({ value }); }
    if (path.endsWith('/attributes/location') && request.method === 'DELETE') { pinned = true; return Response.json({ superseded: { ...value, active_until: when } }); }
    if (path.endsWith('/person-fact-targets')) return Response.json({ fingerprint: 'synthetic', version: '1', targets: [{ kind: 'attribute', key: definition.universal_id, revision: `sha256:${'a'.repeat(64)}`, description: 'Location', value_type: 'string', cardinality: 'single', sensitive: false, choices: null, fields: null, slug: definition.slug, universal_id: definition.universal_id }] });
    if (path.endsWith('/fact-pins')) return Response.json({ pins: pinned ? [{ pinned: true, actor: 'user', event_id: 1, target: { kind: 'attribute', key: definition.universal_id, revision: `sha256:${'a'.repeat(64)}` } }] : [] });
    if (path.endsWith('/fact-evidence')) return Response.json({ evidence: [] });
    if (path.endsWith('/fact-claims')) return Response.json({ claims: [] });
    if (path.endsWith('/fact-decisions')) return Response.json({ decisions: [] });
    if (path === '/api/v1/people/7') return options.personFailure ? Response.json({ error: 'unavailable', message: 'Saved profile unavailable' }, { status: 503 }) : Response.json(person, { headers: { ETag: `"person-7-r${person.revision}"` } });
    if (path.endsWith('/profile')) return options.detailsFailure ? Response.json({ message: 'Structured profile unavailable' }, { status: 503 }) : Response.json(emptyProfile(person), { headers: { ETag: `"person-7-r${person.revision}"` } });
    if (path.endsWith('/contact-state')) return options.detailsFailure ? Response.json({ message: 'Contact state unavailable' }, { status: 503 }) : Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: when, stale: false });
    if (path.endsWith('/attributes')) return options.attributesFailure ? Response.json({ message: 'Attributes unavailable' }, { status: 503 }) : Response.json({ person_id: 7, attributes: (options.pins === 'clear' ? !pinned : pinned) ? [{ definition, current: [value], history: [] }] : [] });
    if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: options.pins ? [definition] : [] });
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

it('shows empty Meetings without requests when no identities are linked', async () => {
  const meetingRequests: Request[] = [];
  await renderDetails([], { meetingRequests });
  expect(await screen.findByText('No meetings yet')).toBeDefined();
  expect(meetingRequests).toEqual([]);
});

it('forwards the person and revision to Meetings', async () => {
  const { person, directory } = await renderDetails([5]);
  await waitFor(() => expect(vi.mocked(PersonMeetings).mock.calls.at(-1)![1]).toMatchObject({ personID: 7, refreshKey: '1' }));
  person.revision = 2;
  await directory.profile!.reload();
  await waitFor(() => expect(vi.mocked(PersonMeetings).mock.calls.at(-1)![1]).toMatchObject({ personID: 7, refreshKey: '2' }));
});

it.each(['merge', 'split'] as const)('refreshes tasks after %s reconciliation', async operation => {
  const { person, directory } = await renderDetails([5], { taskChange: operation });
  await screen.findByText(operation === 'merge' ? 'No tasks yet' : 'Ask about the proposal');

  person.revision = 2;
  if (operation === 'merge') await directory.refreshSelectedPerson();
  else await directory.reconcilePersonSplit({ sourcePersonID: 7, newPersonID: 9 });

  expect(await screen.findByText(operation === 'merge' ? 'Ask about the proposal' : 'No tasks yet')).toBeDefined();
  if (operation === 'split') expect(screen.queryByText('Ask about the proposal')).toBeNull();
});

it('reloads open loops after an identity split', async () => {
  const { person, directory } = await renderDetails([5], { loops: true });
  await screen.findByRole('link', { name: 'Revision 1 question?' });

  person.revision = 2;
  await directory.reconcilePersonSplit({ sourcePersonID: 7, newPersonID: 9 });

  expect(await screen.findByRole('link', { name: 'Revision 2 question?' })).toBeDefined();
  expect(screen.queryByRole('link', { name: 'Revision 1 question?' })).toBeNull();
});

it.each(['save', 'clear'] as const)('refreshes open fact pins after an attribute %s without changing the ledger section', async operation => {
  await renderDetails([], { pins: operation });
  const facts = within(screen.getByRole('heading', { name: 'Facts' }).closest('section')!);
  await fireEvent.click(facts.getByRole('button', { name: 'Show' }));
  await fireEvent.click(await screen.findByRole('radio', { name: 'Pins' }));
  await screen.findByText('No fact pins.');
  if (operation === 'save') {
    await fireEvent.click(screen.getByRole('button', { name: 'More fields' }));
    await fireEvent.click(await screen.findByRole('menuitem', { name: 'Location' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Location' }), { target: { value: 'Example City' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await screen.findByText('Example City');
  } else {
    await fireEvent.click(screen.getByRole('button', { name: 'Remove Location: Example City' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Remove' }));
  }
  expect(await screen.findByText('attribute · Pinned')).toBeDefined();
  expect(screen.getByRole('radio', { name: 'Pins' }).getAttribute('aria-checked')).toBe('true');
});

it('shows one alert when the person was deleted', async () => {
  await renderDetails([], { personDeleted: true });
  expect(screen.getAllByRole('alert')).toHaveLength(1);
  const notice = within(screen.getByRole('alert'));
  expect(notice.getByText("This person isn't in your archive anymore.")).toBeDefined();
  expect(notice.queryByRole('button', { name: 'Retry' })).toBeNull();
});

it('shows profile failure details and recovers through Retry', async () => {
  const options = { detailsFailure: true, personFailure: true };
  await renderDetails([], options);
  const notice = within(screen.getByRole('alert'));
  expect(notice.getByText("Couldn't load these details.")).toBeDefined();
  await fireEvent.click(notice.getByText('Details'));
  expect(notice.getByText('Saved profile unavailable Structured profile unavailable Contact state unavailable')).toBeDefined();
  options.detailsFailure = false; options.personFailure = false;
  await fireEvent.click(notice.getByRole('button', { name: 'Retry' }));
  await waitFor(() => expect(screen.queryByText("Couldn't load these details.")).toBeNull());
});
