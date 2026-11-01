import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import type { DirectoryReadBundle } from '../../directory/models';
import FieldEditor from './FieldEditor.svelte';

const when = '2026-08-01T00:00:00Z';
const person = { id: 7, revision: 1, participant_ids: [], vcard_uid: 'example-7', created_at: when, updated_at: when };
const profile = { person, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] };
const envelope = { id: 11, ordinal: 2, source: 'user', created_at: when, updated_at: when, vcard: {} };
function controller(writes: Request[]) {
  return new DirectoryProfileController(createAPIClient(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    writes.push(request);
    return Response.json(profile, { headers: { ETag: '"person-7-r2"' } });
  }), 7, { person, structuredProfile: profile, etags: { person: '"person-7-r1"', structuredProfile: '"person-7-r1"' }, errors: {} } satisfies DirectoryReadBundle);
}
it.each([
  ['--02-14', { month: 2, day: 14 }],
  ['1984', { year: 1984 }],
  ['Every spring', undefined],
])('saves the partial or free-text date %s', async (value, date) => {
  const writes: Request[] = [];
  render(FieldEditor, { controller: controller(writes), section: 'dates' });
  await fireEvent.input(screen.getByRole('textbox', { name: 'Date' }), { target: { value } });
  await fireEvent.click(screen.getByRole('button', { name: /^Save$/ }));
  await waitFor(() => expect(writes).toHaveLength(1));
  await expect(writes[0]!.json()).resolves.toEqual({ dates: { add: [{ date_kind: 'birthday', original_value: value, date_text: value, ...(date ? { date } : {}), envelope: { source: 'user' } }], supersede: [] } });
});
it('keeps pronunciation parts and the existing metadata when replacing a name', async () => {
  const writes: Request[] = [];
  const current = { person_id: 7, name_kind: 'phonetic', original_value: 'Example', given_name: 'Ex', family_name: 'Ample', phonetic_system: 'ipa', is_derived: false, envelope };
  render(FieldEditor, { controller: controller(writes), section: 'names', current });
  await fireEvent.input(screen.getByRole('textbox', { name: 'First name, said as' }), { target: { value: 'Ecks' } });
  await fireEvent.click(screen.getByRole('button', { name: /^Save$/ }));
  await waitFor(() => expect(writes).toHaveLength(1));
  await expect(writes[0]!.json()).resolves.toEqual({ names: { add: [{ name_kind: 'phonetic', original_value: 'Ecks Ample', formatted: 'Ecks Ample', given_name: 'Ecks', family_name: 'Ample', phonetic_system: 'ipa', is_derived: false, envelope: { source: 'user', ordinal: 2 } }], supersede: [11] } });
});
it('retains a contact service, scope, URI, and ordinal during an edit', async () => {
  const writes: Request[] = [];
  const current = { person_id: 7, address_kind: 'username', original_value: 'old-handle', normalized_value: 'old-handle', normalization: 'casefold', normalization_version: 1, service_slug: 'slack', scope_kind: 'workspace', scope_value: 'example-team', uri: 'https://example.com/person', envelope };
  render(FieldEditor, { controller: controller(writes), section: 'contact_points', current });
  await fireEvent.input(screen.getByRole('textbox', { name: 'Username' }), { target: { value: 'new-handle' } });
  await fireEvent.click(screen.getByRole('button', { name: /^Save$/ }));
  await waitFor(() => expect(writes).toHaveLength(1));
  await expect(writes[0]!.json()).resolves.toEqual({ contact_points: { add: [{ address_kind: 'username', original_value: 'new-handle', service_slug: 'slack', scope_kind: 'workspace', scope_value: 'example-team', uri: 'https://example.com/person', envelope: { source: 'user', ordinal: 2 } }], supersede: [11] } });
});
it('retains a free-text address when its structured parts are empty', async () => {
  const writes: Request[] = [];
  const current = { person_id: 7, address_kind: 'postal', original_value: '', free_text: 'Rear entrance', place_uri: 'geo:1,2', envelope };
  render(FieldEditor, { controller: controller(writes), section: 'addresses', current });
  await fireEvent.click(screen.getByRole('button', { name: /^Save$/ }));
  await waitFor(() => expect(writes).toHaveLength(1));
  await expect(writes[0]!.json()).resolves.toEqual({ addresses: { add: [{ address_kind: 'postal', free_text: 'Rear entrance', place_uri: 'geo:1,2', envelope: { source: 'user', ordinal: 2 } }], supersede: [11] } });
});
