import { act, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
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
function harness(initial = profile(), options: { fail?: boolean; wait?: Promise<void>; bundle?: Partial<DirectoryReadBundle>; definitions?: AttributeDefinition[]; summary?: boolean } = {}) {
  let stored = initial;
  const writes: Request[] = [];
  const client = createAPIClient(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/people/7/attributes/note' && request.method === 'PUT') return Response.json({ message: 'Unable to save field.' }, { status: 503 });
    if (request.method === 'PATCH') {
      writes.push(request.clone()); await options.wait;
      if (options.fail) return Response.json({ error: 'person_revision_conflict', message: 'Profile changed elsewhere' }, { status: 409 });
      const body = await request.json();
      if (body.contact_points) stored = { ...stored, contact_points: (body.contact_points.add ?? []).map((point: object) => ({ ...point, person_id: 7, envelope: { ...envelope, id: 12 } })) };
      return Response.json(stored, { headers: { ETag: '"person-7-r2"' } });
    }
    if (path.endsWith('/profile')) return Response.json(stored, { headers: { ETag: '"person-7-r2"' } });
    if (path.endsWith('/attributes')) return Response.json({ attributes: [] });
    if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: (options.definitions ?? []).filter(definition => new URL(request.url).searchParams.get('include_hidden') === 'true' || definition.is_active) });
    return Response.json(person, { headers: { ETag: '"person-7-r2"' } });
  });
  const controller = new DirectoryProfileController(client, 7, { person, structuredProfile: initial, etags: { person: '"person-7-r1"', structuredProfile: '"person-7-r1"' }, errors: {}, ...options.bundle });
  const summary = createRawSnippet(() => ({ render: () => '<p>Summary row</p>' }));
  const view = render(PersonFields, options.summary ? { controller, summary } : { controller });
  return { controller, writes, view };
}

it('keeps a conflicted structured editor available to cancel after a deferred failed reload', async () => {
  let releaseReload!: (response: Response) => void;
  const pendingReload = new Promise<Response>(resolve => releaseReload = resolve);
  const initial = profile();
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (request.method === 'PATCH') return Response.json({ error: 'person_revision_conflict', message: 'Profile changed elsewhere' }, { status: 409 });
    if (path.endsWith('/profile')) return pendingReload;
    if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
    if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
    if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: when, stale: false });
    return Response.json(person, { headers: { ETag: '"person-7-r1"' } });
  });
  const controller = new DirectoryProfileController(client, 7, { person, structuredProfile: initial, attributes: { person_id: 7, attributes: [] }, etags: { person: '"person-7-r1"', structuredProfile: '"profile-7-r1"' }, errors: {} });
  const view = render(PersonFields, { controller });
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'Add phone' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Contact value' }), { target: { value: '+12025550123' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save contact point' }));
    await screen.findByRole('button', { name: 'Reload profile' });
    await fireEvent.click(screen.getByRole('button', { name: 'Reload profile' }));
    expect(controller.reloadPending).toBe(true);
    const cancel = screen.getByRole('button', { name: /^Cancel$/ });
    await fireEvent.click(cancel);
    expect(screen.getByDisplayValue('+12025550123')).toBeDefined();
    await act(() => releaseReload(Response.json({ message: 'Profile unavailable' }, { status: 503 })));
    await waitFor(() => expect(controller.reloadPending).toBe(false));
    expect(controller.conflict).not.toBeNull();
    await fireEvent.click(cancel);
    expect(screen.queryByDisplayValue('+12025550123')).toBeNull();
    expect(controller.draft).toBeNull();
    expect(controller.conflict).toBeNull();
    expect(controller.canWriteProfile).toBe(true);
  } finally { releaseReload(Response.json({ message: 'Profile unavailable' }, { status: 503 })); view.unmount(); controller.destroy(); }
});

it('blocks attribute additions after a failed read and retries before editing the unseen value', async () => {
  const definition = { id: 8, universal_id: '00000000-0000-4000-8000-000000000008', slug: 'note', label: 'Note', value_type: 'text', field_type: 'text', cardinality: 'single', object_type: 'person', display_order: 1, ownership: 'user', is_active: true, api_mutable: true, ui_editable: true, ui_creatable: true, is_sensitive: false, revision: 1 } as AttributeDefinition;
  const current = { id: 41, person_id: 7, definition_id: 8, definition_slug: 'note', ordinal: 0, value: { type: 'text', text: 'Existing note' }, active_from: when, created_at: when, source: 'user' };
  let reads = 0;
  const writes: Request[] = [];
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/people/7/attributes/note' && request.method === 'PUT') {
      writes.push(request.clone());
      return Response.json({ value: { ...current, id: 42, value: { type: 'text', text: 'Updated note' } }, superseded: { ...current, superseded_at: when } });
    }
    if (path.endsWith('/attributes')) {
      reads += 1;
      return reads === 1 ? Response.json({ message: 'Attributes unavailable' }, { status: 503 }) : Response.json({ person_id: 7, attributes: [{ definition, current: [current], history: [] }] });
    }
    if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [definition] });
    if (path.endsWith('/profile')) return Response.json(profile(), { headers: { ETag: '"profile-7-r1"' } });
    if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, interaction_count: 0, cadence_status: 'unknown', computed_at: when, stale: false });
    if (path === '/api/v1/people/7') return Response.json(person, { headers: { ETag: '"person-7-r1"' } });
    throw new Error(`unexpected ${request.method} ${path}`);
  });
  const controller = new DirectoryProfileController(client, 7, { person, structuredProfile: profile(), definitions: { definitions: [definition] }, etags: { person: '"person-7-r1"', structuredProfile: '"profile-7-r1"' }, errors: { attributes: 'Attributes unavailable' } });
  const view = render(PersonFields, { controller });
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'More fields' }));
    expect((await screen.findByRole('menuitem', { name: 'Note' })).getAttribute('aria-disabled')).toBe('true');
    expect(writes).toHaveLength(0);
    await fireEvent.keyDown(screen.getByRole('menu', { name: 'More fields' }), { key: 'Escape' });
    await fireEvent.click(screen.getByRole('button', { name: 'Retry attributes' }));
    await waitFor(() => expect(screen.getByRole('alert').textContent).toContain('Attributes unavailable'));
    await fireEvent.click(screen.getByRole('button', { name: 'Retry attributes' }));
    expect(await screen.findByText('Existing note')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Edit Note' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Note' }), { target: { value: 'Updated note' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));
    expect(await screen.findByText('Updated note')).toBeDefined();
    await expect(writes[0]!.json()).resolves.toMatchObject({ expected_value_id: 41, value: { type: 'text', text: 'Updated note' } });
  } finally { view.unmount(); controller.destroy(); }
});

it.each([false, true])('keeps removal state until its outcome settles (failure=%s)', async fail => {
  const initial = profile();
  initial.contact_points = [{ person_id: 7, address_kind: 'email', original_value: 'old@example.com', normalized_value: 'old@example.com', normalization: 'email', normalization_version: 1, envelope }];
  let finish!: () => void;
  const { controller, view, writes } = harness(initial, { fail, wait: new Promise<void>(resolve => { finish = resolve; }) });
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'Remove Email' }));
    expect(writes).toHaveLength(0);
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm removal' }));
    if (!fail) {
      expect(screen.getByText('old@example.com')).toBeDefined();
      expect(screen.getByRole('button', { name: 'Add phone' })).toHaveProperty('disabled', true);
    }
    await act(() => finish());
    if (fail) {
      expect(await screen.findByRole('button', { name: 'Reload' })).toBeDefined();
      expect(screen.getByRole('group', { name: 'Confirm removing Email' })).toBeDefined();
      expect(screen.getByText('old@example.com')).toBeDefined();
    } else {
      await waitFor(() => expect(screen.queryByText('old@example.com')).toBeNull());
      await expect(writes[0]!.json()).resolves.toEqual({ contact_points: { supersede: [11] } });
    }
  } finally { view.unmount(); controller.destroy(); }
});

it('gates media editing by available data and keeps removal available', () => {
  const initial = profile();
  initial.media = [{ person_id: 7, media_kind: 'photo', original_value: 'Portrait', uri: 'https://example.com/photo.png', has_data: false, envelope }, { person_id: 7, media_kind: 'logo', original_value: 'Stored logo', has_data: true, content_hash: 'a'.repeat(64), envelope: { ...envelope, id: 12 } }];
  const { view, controller } = harness(initial);
  try {
    expect(screen.getByText('Portrait')).toBeDefined(); expect(screen.getByText('Stored logo')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Edit Photo' })).toHaveProperty('disabled', false);
    expect(screen.getByRole('button', { name: 'Edit Logo' })).toHaveProperty('disabled', true);
    expect(screen.getByRole('button', { name: 'Remove Logo' })).toHaveProperty('disabled', false);
  } finally { view.unmount(); controller.destroy(); }
});

it('displays every structured address component before its original street value', () => {
  const initial = profile();
  initial.addresses = [{ person_id: 7, address_kind: 'postal', original_value: '1 Example Street', street_address: '1 Example Street', post_office_box: 'PO Box 7', extended_address: 'Suite 2', extended_components: 'Building C', locality: 'Example City', region: 'CA', postal_code: '90001', country_name: 'United States', envelope }];
  const { view, controller } = harness(initial);
  try {
    expect(screen.getByText('PO Box 7, Suite 2, 1 Example Street, Example City, CA, 90001, United States, Building C')).toBeDefined();
  } finally { view.unmount(); controller.destroy(); }
});

it.each<{ name: string; refreshed: Partial<AttributeDefinition>[]; menu?: boolean }>([
  { name: 'rename keeps the draft', refreshed: [{ label: 'Renamed note' }] },
  { name: 'inactive field closes the draft', refreshed: [{ label: 'Private note', is_active: false }] },
  { name: 'sensitive field closes the draft and conceals the value', refreshed: [{ label: 'Private note', is_sensitive: true }] },
  { name: 'menu open reloads choices', refreshed: [{ label: 'Renamed note' }, { id: 41, universal_id: '00000000-0000-4000-8000-000000000041', slug: 'favorite_color', label: 'Favorite color' }], menu: true },
])('joins refreshed field settings: $name', async ({ refreshed, menu }) => {
  const definition = { id: 8, universal_id: '00000000-0000-4000-8000-000000000008', slug: 'note', label: 'Note', value_type: 'text', field_type: 'text', cardinality: 'single', object_type: 'person', display_order: 1, ownership: 'user', is_active: true, api_mutable: true, ui_editable: true, ui_creatable: true, is_sensitive: false, revision: 1 } as AttributeDefinition;
  const value = { id: 41, person_id: 7, definition_id: 8, definition_slug: 'note', ordinal: 0, value: { type: 'text' as const, text: 'Private example' }, active_from: when, created_at: when, source: 'user' };
  const options = { definitions: refreshed.map(settings => ({ ...definition, revision: 2, ...settings })), bundle: { definitions: { definitions: [definition] }, attributes: { person_id: 7, attributes: [{ definition, current: [value], history: [] }] } } };
  const { controller, view } = harness(profile(), options);
  try {
    if (menu) {
      await fireEvent.click(screen.getByRole('button', { name: 'More fields' }));
      expect(await screen.findByRole('menuitem', { name: 'Favorite color' })).toBeDefined();
    } else {
      await fireEvent.click(screen.getByRole('button', { name: 'Edit Note' }));
      await fireEvent.input(screen.getByRole('textbox', { name: 'Note' }), { target: { value: 'Unsaved local note' } });
      await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));
      await waitFor(() => expect(controller.draft?.kind).toBe('setAttribute'));
      await act(() => controller.reloadDefinitions());
      if (refreshed[0]!.is_active === false || refreshed[0]!.is_sensitive) {
        expect(screen.queryByDisplayValue('Unsaved local note')).toBeNull();
        expect(controller.draft).toBeNull();
        expect(screen.getByRole('button', { name: 'Edit Private note' })).toHaveProperty('disabled', true);
      } else {
        expect(screen.getByDisplayValue('Unsaved local note')).toBeDefined();
      }
      if (refreshed[0]!.is_sensitive) {
        expect(screen.queryByText('Private example')).toBeNull();
        await fireEvent.click(screen.getByRole('button', { name: 'Show Private note' }));
        expect(screen.getByText('Private example')).toBeDefined();
      }
    }
  } finally { view.unmount(); controller.destroy(); }
});

it.each([
  ['Website', 'Messaging address', 'Contact value', 'xmpp:example@example.com', 'Save contact point', 'contact_points', 'address_kind', 'impp'],
  ['Nickname', 'Pronunciation', 'First name, said as', 'Arisu', 'Save name', 'names', 'name_kind', 'phonetic'],
])('switches a new %s editor to %s before saving', async (first, next, input, value, save, section, discriminator, kind) => {
  const { controller, view, writes } = harness();
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'More fields' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: first }));
    await fireEvent.click(screen.getByRole('button', { name: 'More fields' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: next }));
    await fireEvent.input(screen.getByRole('textbox', { name: input }), { target: { value } });
    await fireEvent.click(screen.getByRole('button', { name: save }));
    await waitFor(() => expect(writes).toHaveLength(1));
    await expect(writes[0]!.json()).resolves.toMatchObject({ [section]: { add: [{ [discriminator]: kind, original_value: value }] } });
  } finally { view.unmount(); controller.destroy(); }
});

it('shows a repeated label once, the editor after the list, and the summary before More fields', async () => {
  const initial = profile();
  initial.contact_points = ['first@example.com', 'second@example.com'].map((value, index) => ({ person_id: 7, address_kind: 'email', original_value: value, normalized_value: value, normalization: 'email', normalization_version: 1, envelope: { ...envelope, id: 20 + index, ordinal: index } }));
  const { controller, view } = harness(initial, { summary: true });
  try {
    expect(screen.getAllByText('Email', { selector: '.label' })).toHaveLength(1);
    const more = screen.getByRole('button', { name: 'More fields' });
    expect(screen.getByText('Summary row').compareDocumentPosition(more) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

    await fireEvent.click(screen.getAllByRole('button', { name: 'Edit Email' })[0]!);
    const input = await screen.findByRole('textbox', { name: /email|contact value/i });
    expect(screen.queryByText('first@example.com')).toBeNull();
    expect(input.compareDocumentPosition(screen.getByText('second@example.com')) & Node.DOCUMENT_POSITION_PRECEDING).toBeTruthy();
  } finally { view.unmount(); controller.destroy(); }
});

it('shows sensitive values as Hidden and lets More fields reveal a hidden field and open its form', async () => {
  const note = { id: 8, universal_id: '00000000-0000-4000-8000-000000000008', slug: 'note', label: 'Private note', value_type: 'text', field_type: 'text', cardinality: 'single', object_type: 'person', display_order: 1, ownership: 'user', is_active: true, api_mutable: true, ui_editable: true, ui_creatable: true, is_sensitive: true, revision: 1 } as AttributeDefinition;
  const religion = { ...note, id: 9, universal_id: '00000000-0000-4000-8000-000000000009', slug: 'religion', label: 'Religion', display_order: 2 } as AttributeDefinition;
  const value = { id: 41, person_id: 7, definition_id: 8, definition_slug: 'note', ordinal: 0, value: { type: 'text' as const, text: 'Secret example' }, active_from: when, created_at: when, source: 'user' };
  const { controller, view } = harness(profile(), { definitions: [note, religion], bundle: { definitions: { definitions: [note, religion] }, attributes: { person_id: 7, attributes: [{ definition: note, current: [value], history: [] }] } } });
  try {
    expect(screen.getByText('Hidden')).toBeDefined();
    expect(screen.queryByText('Secret example')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'More fields' }));
    await fireEvent.click(await screen.findByRole('menuitem', { name: 'Religion' }));
    expect(await screen.findByRole('textbox', { name: 'Religion' })).toBeDefined();
  } finally { view.unmount(); controller.destroy(); }
});
