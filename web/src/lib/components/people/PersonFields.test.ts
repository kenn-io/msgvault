import { act, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import type { AttributeDefinition } from '../../api/generated/models';
import type { DirectoryReadBundle } from '../../directory/models';
import PersonFields from './PersonFields.svelte';

const when = '2026-08-01T00:00:00Z';
const person = { id: 7, revision: 1, display_name: 'Example Person', participant_ids: [], vcard_uid: 'example-7', created_at: when, updated_at: when };
const envelope = { id: 11, ordinal: 2, source: 'user', created_at: when, updated_at: when, vcard: {} };
function profile() { return { person, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] } as NonNullable<DirectoryReadBundle['structuredProfile']>; }
function harness(initial = profile(), options: { fail?: boolean; wait?: Promise<void>; bundle?: Partial<DirectoryReadBundle>; definitions?: AttributeDefinition[] } = {}) {
  let stored = initial;
  const writes: Request[] = [];
  const client = createAPIClient(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (request.method === 'PATCH') {
      writes.push(request.clone()); await options.wait;
      if (options.fail) return Response.json({ error: 'person_revision_conflict', message: 'Profile changed elsewhere' }, { status: 412 });
      const body = await request.json();
      if (body.contact_points) stored = { ...stored, contact_points: (body.contact_points.add ?? []).map((point: object) => ({ ...point, person_id: 7, envelope: { ...envelope, id: 12 } })) };
      return Response.json(stored, { headers: { ETag: '"person-7-r2"' } });
    }
    if (path.endsWith('/profile')) return Response.json(stored, { headers: { ETag: '"person-7-r2"' } });
    if (path.endsWith('/attributes')) return Response.json({ attributes: [] });
    if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: options.definitions ?? [] });
    return Response.json(person, { headers: { ETag: '"person-7-r2"' } });
  });
  const controller = new DirectoryProfileController(client, 7, { person, structuredProfile: initial, etags: { person: '"person-7-r1"', structuredProfile: '"person-7-r1"' }, errors: {}, ...options.bundle });
  const view = render(PersonFields, { controller });
  return { controller, writes, view };
}

it('keeps a confirmed removal visible and gates other writes until the save succeeds', async () => {
  const initial = profile();
  initial.contact_points = [{ person_id: 7, address_kind: 'email', original_value: 'old@example.com', normalized_value: 'old@example.com', normalization: 'email', normalization_version: 1, envelope }];
  let finish!: () => void;
  const { controller, view, writes } = harness(initial, { wait: new Promise<void>(resolve => { finish = resolve; }) });
  await fireEvent.click(screen.getByRole('button', { name: 'Remove Email' }));
  expect(writes).toHaveLength(0);
  await fireEvent.click(screen.getByRole('button', { name: 'Confirm removal' }));
  expect(screen.getByText('old@example.com')).toBeDefined();
  expect(screen.getByRole('button', { name: 'Add phone' })).toHaveProperty('disabled', true);
  await act(() => finish()); await waitFor(() => expect(screen.queryByText('old@example.com')).toBeNull());
  await expect(writes[0]!.json()).resolves.toEqual({ contact_points: { supersede: [11] } });
  await act(() => controller.reload()); expect(screen.queryByText('old@example.com')).toBeNull();
  view.unmount(); controller.destroy();
});

it('keeps the confirmation and row after a failed removal and exposes reload', async () => {
  const initial = profile();
  initial.contact_points = [{ person_id: 7, address_kind: 'email', original_value: 'old@example.com', normalized_value: 'old@example.com', normalization: 'email', normalization_version: 1, envelope }];
  const { controller, view } = harness(initial, { fail: true });
  await fireEvent.click(screen.getByRole('button', { name: 'Remove Email' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Confirm removal' }));
  expect(screen.getByText('old@example.com')).toBeDefined();
  expect(await screen.findByRole('group', { name: 'Confirm removing Email' })).toBeDefined();
  expect(await screen.findByRole('button', { name: 'Reload' })).toBeDefined();
  view.unmount(); controller.destroy();
});

it('shows current URI and inline media with the supported edit and removal actions', () => {
  const initial = profile();
  initial.media = [{ person_id: 7, media_kind: 'photo', original_value: 'Portrait', uri: 'https://example.com/photo.png', has_data: false, envelope }, { person_id: 7, media_kind: 'logo', original_value: 'Stored logo', has_data: true, envelope: { ...envelope, id: 12 } }];
  const { view, controller } = harness(initial);
  expect(screen.getByText('Portrait')).toBeDefined(); expect(screen.getByText('Stored logo')).toBeDefined();
  expect(screen.getByRole('button', { name: 'Edit Photo' })).toHaveProperty('disabled', false);
  expect(screen.getByRole('button', { name: 'Edit Logo' })).toHaveProperty('disabled', true);
  expect(screen.getByRole('button', { name: 'Remove Logo' })).toHaveProperty('disabled', false);
  view.unmount(); controller.destroy();
});

it('displays every structured address component before its original street value', () => {
  const initial = profile();
  initial.addresses = [{ person_id: 7, address_kind: 'postal', original_value: '1 Example Street', street_address: '1 Example Street', extended_address: 'Suite 2', locality: 'Example City', region: 'CA', postal_code: '90001', country_name: 'United States', envelope }];
  const { view, controller } = harness(initial);
  expect(screen.getByText('Suite 2, 1 Example Street, Example City, CA, 90001, United States')).toBeDefined();
  view.unmount(); controller.destroy();
});

it('joins refreshed sensitive and read-only field settings to current values and discards their editor', async () => {
  const definition = { id: 8, universal_id: '00000000-0000-4000-8000-000000000008', slug: 'note', label: 'Note', value_type: 'text', field_type: 'text', cardinality: 'single', object_type: 'person', display_order: 1, ownership: 'user', is_active: true, api_mutable: true, ui_editable: true, ui_creatable: true, is_sensitive: false, revision: 1 } as AttributeDefinition;
  const value = { id: 41, person_id: 7, definition_id: 8, definition_slug: 'note', ordinal: 0, value: { type: 'text' as const, text: 'Private example' }, active_from: when, created_at: when, source: 'user' };
  const options = { definitions: [{ ...definition, label: 'Private note', is_sensitive: true, ui_editable: false, revision: 2 }], bundle: { definitions: { definitions: [definition] }, attributes: { person_id: 7, attributes: [{ definition, current: [value], history: [] }] } } };
  const { controller, view } = harness(profile(), options);
  await fireEvent.click(screen.getByRole('button', { name: 'Edit Note' }));
  await act(() => controller.reloadDefinitions());
  expect(screen.queryByDisplayValue('Private example')).toBeNull(); expect(screen.queryByText('Private example')).toBeNull();
  expect(screen.getByText('Private note')).toBeDefined();
  expect(screen.getByRole('button', { name: 'Edit Private note' })).toHaveProperty('disabled', true);
  await fireEvent.click(screen.getByRole('button', { name: 'Show Private note' })); expect(screen.getByText('Private example')).toBeDefined();
  view.unmount(); controller.destroy();
});

it.each([
  ['Website', 'Messaging address', 'Contact value', 'xmpp:example@example.com', 'Save contact point', 'contact_points', 'address_kind', 'impp'],
  ['Nickname', 'Pronunciation', 'First name, said as', 'Arisu', 'Save name', 'names', 'name_kind', 'phonetic'],
])('switches a new %s editor to %s before saving', async (first, next, input, value, save, section, discriminator, kind) => {
  const { controller, view, writes } = harness();
  await fireEvent.click(screen.getByRole('button', { name: 'More fields' }));
  await fireEvent.click(screen.getByRole('menuitem', { name: first }));
  await fireEvent.click(screen.getByRole('button', { name: 'More fields' }));
  await fireEvent.click(screen.getByRole('menuitem', { name: next }));
  await fireEvent.input(screen.getByRole('textbox', { name: input }), { target: { value } });
  await fireEvent.click(screen.getByRole('button', { name: save }));
  await waitFor(() => expect(writes).toHaveLength(1));
  await expect(writes[0]!.json()).resolves.toMatchObject({ [section]: { add: [{ [discriminator]: kind, original_value: value }] } });
  view.unmount(); controller.destroy();
});
