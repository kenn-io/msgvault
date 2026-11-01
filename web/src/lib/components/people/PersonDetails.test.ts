import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryController } from '../../directory/controller.svelte';
import PersonDetails from './PersonDetails.svelte';

it('retries a failed section without discarding an open field draft', async () => {
  const when = '2026-08-01T00:00:00Z';
  const person = { id: 7, revision: 1, display_name: 'Example Person', participant_ids: [1], vcard_uid: 'example-7', created_at: when, updated_at: when };
  let contactReads = 0;
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/people/7') return Response.json(person, { headers: { ETag: '"person-7-r1"' } });
    if (path.endsWith('/profile')) return Response.json({ person, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] }, { headers: { ETag: '"profile-7-r1"' } });
    if (path.endsWith('/contact-state')) {
      contactReads += 1;
      return contactReads === 1 ? Response.json({ message: 'Contact state unavailable' }, { status: 503 }) : Response.json({ person_id: 7, cadence_status: 'current', interaction_count: 3, computed_at: when, stale: false });
    }
    if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
    if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
    if (path.endsWith('/employments')) return Response.json({ employments: [] });
    if (path.endsWith('/relationships')) return Response.json({ relationships: [] });
    if (path.endsWith('/network')) return Response.json({ root_person_id: 7, depth: 1, nodes: [], edges: [], truncated: false });
    return Response.json({});
  });
  const directory = new DirectoryController(client);
  await directory.loadSelection(7);
  const owner = directory.profile;
  const entity = directory.entity;
  const view = render(PersonDetails, { client, directory, personID: 7 });
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'Add phone' }));
    const input = screen.getByRole('textbox', { name: 'Contact value' });
    await fireEvent.input(input, { target: { value: '+12025550123' } });
    const alert = screen.getAllByRole('alert').find(element => element.textContent?.includes('Contact state unavailable'))!;
    await fireEvent.click(within(alert).getByRole('button', { name: 'Retry' }));
    expect(await screen.findByText('3 interactions')).toBeDefined();
    await waitFor(() => expect(screen.queryByText(/Contact state unavailable/)).toBeNull());
    expect(directory.profile).toBe(owner);
    expect(directory.entity).toBe(entity);
    expect(screen.getByRole('textbox', { name: 'Contact value' })).toBe(input);
    expect(input).toHaveProperty('value', '+12025550123');
  } finally { view.unmount(); directory.destroy(); }
});
