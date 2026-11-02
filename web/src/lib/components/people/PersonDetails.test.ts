import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryController } from '../../directory/controller.svelte';
import PersonDetails from './PersonDetails.svelte';
import MeetingPanel from '../meetings/MeetingPanel.svelte';

vi.mock('../meetings/MeetingPanel.svelte', { spy: true });

it('shows one attributes alert and forwards the refreshed meeting revision', async () => {
  const when = '2026-08-01T00:00:00Z';
  const person = { id: 7, revision: 1, display_name: 'Example Person', participant_ids: [], vcard_uid: 'example-7', created_at: when, updated_at: when };
  let attributeReads = 0;
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/people/7') return Response.json(person, { headers: { ETag: '"person-7-r1"' } });
    if (path.endsWith('/profile')) return Response.json({ person, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] }, { headers: { ETag: '"profile-7-r1"' } });
    if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'current', interaction_count: 3, computed_at: when, stale: false });
    if (path.endsWith('/attributes')) {
      attributeReads += 1;
      return attributeReads === 1 ? Response.json({ message: 'Attributes unavailable' }, { status: 503 }) : Response.json({ person_id: 7, attributes: [] });
    }
    if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
    if (path.endsWith('/employments')) return Response.json({ employments: [] });
    if (path.endsWith('/relationships')) return Response.json({ relationships: [] });
    if (path.endsWith('/meetings/metrics') || path.endsWith('/meetings/actions')) return Response.json({ error: 'person_scope_empty', message: 'Person has no resolved identities' }, { status: 422 });
    if (path.endsWith('/network')) return Response.json({ root_person_id: 7, depth: 1, nodes: [], edges: [], truncated: false });
    return Response.json({});
  });
  const directory = new DirectoryController(client);
  await directory.loadSelection(7);
  const view = render(PersonDetails, { client, directory, personID: 7 });
  try {
    const alerts = screen.getAllByRole('alert');
    expect(alerts).toHaveLength(1);
    const alert = alerts[0]!;
    expect(alert.textContent).toContain('Attributes unavailable');
    expect(alert.textContent).not.toContain('attributes:');
    expect(screen.getAllByRole('button', { name: /Retry/ })).toHaveLength(1);
    expect(vi.mocked(MeetingPanel).mock.calls.at(-1)?.[1].refreshKey).toBe('1');
    person.revision = 2;
    await fireEvent.click(within(alert).getByRole('button', { name: 'Retry attributes' }));
    await waitFor(() => expect(vi.mocked(MeetingPanel).mock.calls.at(-1)?.[1].refreshKey).toBe('2'));
  } finally { view.unmount(); directory.destroy(); }
});
