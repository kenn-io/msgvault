import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import type { AttributeDefinition } from '../../api/generated/models';
import type { DirectoryReadBundle } from '../../directory/models';
import PersonFields from './PersonFields.svelte';
import { emptyProfile } from '../../directory/profile-fixtures.test-support';

const when = '2026-08-01T00:00:00Z';
const person = { id: 7, revision: 1, display_name: 'Example Person', participant_ids: [], vcard_uid: 'example-7', created_at: when, updated_at: when };
const envelope = { id: 11, ordinal: 2, source: 'user', created_at: when, updated_at: when, vcard: {} };
function profile() { return emptyProfile(person); }
function harness(initial = profile(), options: { fail?: boolean; wait?: Promise<void>; onAnnounce?: (text: string) => void; bundle?: Partial<DirectoryReadBundle> } = {}) {
  let stored = initial;
  const writes: Request[] = [];
  const client = createAPIClient(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (request.method === 'PATCH') {
      writes.push(request.clone()); await options.wait;
      if (options.fail) return Response.json({ error: 'person_revision_conflict', message: 'Profile changed elsewhere' }, { status: 409 });
      const body = await request.json();
      if (body.contact_points) stored = { ...stored, contact_points: (body.contact_points.add ?? []).map((point: object) => ({ ...point, person_id: 7, envelope: { ...envelope, id: 12 } })) };
      return Response.json(stored, { headers: { ETag: '"person-7-r2"' } });
    }
    if (path.endsWith('/profile')) return Response.json(stored, { headers: { ETag: '"person-7-r2"' } });
    if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
    if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
    if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: when, stale: false });
    return Response.json(person, { headers: { ETag: '"person-7-r2"' } });
  });
  const controller = new DirectoryProfileController(client, 7, { person, structuredProfile: initial, attributes: { person_id: 7, attributes: [] }, etags: { person: '"person-7-r1"', structuredProfile: '"person-7-r1"' }, errors: {}, ...options.bundle });
  const view = render(PersonFields, { controller, onAnnounce: options.onAnnounce });
  return { controller, writes, view };
}

it('keeps a conflicted structured editor available when Cancel is clicked during reload', async () => {
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
    const cancel = screen.getByRole('button', { name: /^Cancel$/ });
    await fireEvent.click(cancel);
    expect(screen.getByDisplayValue('+12025550123')).toBeDefined();
  } finally { releaseReload(Response.json({ message: 'Profile unavailable' }, { status: 503 })); view.unmount(); controller.destroy(); }
});

it('identifies row actions by value and service scope and copies an own field with feedback', async () => {
  const initial = profile();
  initial.contact_points = [
    ...['first@example.com', 'second@example.com'].map((value, index) => ({ person_id: 7, identity_key: `email-${index}`, address_kind: 'email', original_value: value, normalized_value: value, normalization: 'email', normalization_version: 1, envelope: { ...envelope, id: 20 + index, ordinal: index } })),
    ...['T-WORK', 'T-PERSONAL'].map((scope, index) => ({ person_id: 7, identity_key: `slack-${index}`, address_kind: 'username', original_value: 'example', normalized_value: 'example', service_slug: 'slack', scope_kind: 'workspace', scope_value: scope, normalization: 'casefold', normalization_version: 1, envelope: { ...envelope, id: 40 + index, ordinal: index } })),
    ...['+12025550123', '+12025550124'].map((value, index) => ({ person_id: 7, identity_key: `phone-${index}`, address_kind: 'phone', original_value: value, normalized_value: value, normalization: 'phone', normalization_version: 1, envelope: { ...envelope, id: 30 + index, ordinal: index } })),
  ];
  const note = { id: 8, universal_id: '00000000-0000-4000-8000-000000000008', slug: 'note', label: 'Note', value_type: 'text', field_type: 'text', cardinality: 'multi', object_type: 'person', display_order: 1, ownership: 'user', is_active: true, api_mutable: true, ui_editable: true, ui_creatable: true, is_sensitive: false, revision: 1 } as AttributeDefinition;
  const values = (definition: AttributeDefinition, texts: string[]) => texts.map((text, ordinal) => ({ id: definition.id * 10 + ordinal, person_id: 7, definition_id: definition.id, definition_slug: definition.slug, ordinal, value: { type: 'text' as const, text }, active_from: when, created_at: when, source: 'user' }));
  const writeText = vi.fn(async () => undefined);
  const originalClipboard = navigator.clipboard;
  Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
  const onAnnounce = vi.fn();
  const { controller, view } = harness(initial, { onAnnounce, bundle: { definitions: { definitions: [note] }, attributes: { person_id: 7, attributes: [{ definition: note, current: values(note, ['First note', 'Second note']), history: [] }] } } });
  try {
    expect(screen.getAllByText('Email', { selector: '.label' })).toHaveLength(1);
    for (const label of ['Email: first@example.com', 'Email: second@example.com', 'Username: example · slack · workspace · T-WORK', 'Username: example · slack · workspace · T-PERSONAL', 'Phone: +12025550123', 'Phone: +12025550124', 'Note: First note', 'Note: Second note']) {
      expect(screen.getByRole('button', { name: `Edit ${label}` })).toBeDefined();
      await fireEvent.click(screen.getByRole('button', { name: `Remove ${label}` }));
      const confirmation = screen.getByRole('group', { name: `Confirm removing ${label}` });
      expect(confirmation.textContent).toContain(label);
      await fireEvent.click(within(confirmation).getByRole('button', { name: 'Cancel' }));
    }
    await fireEvent.click(screen.getByRole('button', { name: 'Copy first@example.com' }));
    await waitFor(() => expect(onAnnounce).toHaveBeenLastCalledWith('Copied'));
    expect(writeText).toHaveBeenCalledWith('first@example.com');
  } finally { Object.defineProperty(navigator, 'clipboard', { configurable: true, value: originalClipboard }); view.unmount(); controller.destroy(); }
});

it('blocks attribute additions after a failed read and routes Retry to the profile controller', async () => {
  const definition = { id: 8, universal_id: '00000000-0000-4000-8000-000000000008', slug: 'note', label: 'Note', value_type: 'text', field_type: 'text', cardinality: 'single', object_type: 'person', display_order: 1, ownership: 'user', is_active: true, api_mutable: true, ui_editable: true, ui_creatable: true, is_sensitive: false, revision: 1 } as AttributeDefinition;
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path.endsWith('/attributes')) return Response.json({ message: 'Attributes unavailable' }, { status: 503 });
    if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [definition] });
    if (path.endsWith('/profile')) return Response.json(profile(), { headers: { ETag: '"profile-7-r1"' } });
    if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, interaction_count: 0, cadence_status: 'unknown', computed_at: when, stale: false });
    if (path === '/api/v1/people/7') return Response.json(person, { headers: { ETag: '"person-7-r1"' } });
    throw new Error(`unexpected ${request.method} ${path}`);
  });
  const controller = new DirectoryProfileController(client, 7, { person, structuredProfile: profile(), attributes: { person_id: 7, attributes: [] }, definitions: { definitions: [definition] }, etags: { person: '"person-7-r1"', structuredProfile: '"profile-7-r1"' }, errors: {} });
  await controller.reload();
  const view = render(PersonFields, { controller });

  try {
    const reload = vi.spyOn(controller, 'reload');
    const alert = screen.getByRole('alert');
    expect(alert.textContent).toContain("Couldn't load field values.");
    // The server's message waits in the notice's Details.
    expect(view.container.querySelector('details pre')?.textContent).toBe('Attributes unavailable');
    await fireEvent.click(screen.getByRole('button', { name: 'More fields' }));
    expect((await screen.findByRole('menuitem', { name: 'Note' })).getAttribute('aria-disabled')).toBe('true');
    await fireEvent.keyDown(screen.getByRole('menu', { name: 'More fields' }), { key: 'Escape' });
    await fireEvent.click(within(alert).getByRole('button', { name: 'Retry' }));
    expect(reload).toHaveBeenCalledOnce();
    await reload.mock.results[0]!.value;
  } finally { view.unmount(); controller.destroy(); }
});

it.each([false, true])('keeps removal state until its outcome settles (failure=%s)', async fail => {
  const initial = profile();
  initial.contact_points = [{ person_id: 7, identity_key: 'old-email-key', address_kind: 'email', original_value: 'old@example.com', normalized_value: 'old@example.com', normalization: 'email', normalization_version: 1, envelope }];
  let finish!: () => void;
  const { controller, view, writes } = harness(initial, { fail, wait: new Promise<void>(resolve => { finish = resolve; }) });
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'Remove Email: old@example.com' }));
    expect(writes).toHaveLength(0);
    await fireEvent.click(screen.getByRole('button', { name: 'Remove' }));
    if (!fail) {
      expect(screen.getByText('old@example.com')).toBeDefined();
      expect(screen.getByRole('button', { name: 'Add phone' })).toHaveProperty('disabled', true);
    }
    await act(() => finish());
    if (fail) {
      expect(await screen.findByRole('button', { name: 'Reload' })).toBeDefined();
      expect(screen.getByRole('group', { name: 'Confirm removing Email: old@example.com' })).toBeDefined();
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
    expect(screen.getByRole('button', { name: 'Edit Photo: Portrait' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Edit Logo: Stored logo' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Remove Logo: Stored logo' })).toHaveProperty('disabled', false);
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

async function refreshOpenField(refreshed: Partial<AttributeDefinition>, cardinality: 'single' | 'multi' = 'single', extraValues: string[] = []) {
  const definition = { id: 8, universal_id: '00000000-0000-4000-8000-000000000008', slug: 'note', label: 'Note', value_type: 'text', field_type: 'text', cardinality, object_type: 'person', display_order: 1, ownership: 'user', is_active: true, api_mutable: true, ui_editable: true, ui_creatable: true, is_sensitive: false, revision: 1 } as AttributeDefinition;
  const value = { id: 41, person_id: 7, definition_id: 8, definition_slug: 'note', ordinal: 0, value: { type: 'text' as const, text: 'Private example' }, active_from: when, created_at: when, source: 'user' };
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/people/7/attributes/note' && request.method === 'PUT') return Response.json({ message: 'Unable to save field.' }, { status: 503 });
    if (path === '/api/v1/attribute-definitions') return Response.json({ definitions: [{ ...definition, revision: 2, ...refreshed }] });
    throw new Error(`unexpected ${request.method} ${path}`);
  });
  const controller = new DirectoryProfileController(client, 7, { person, structuredProfile: profile(), definitions: { definitions: [definition] }, attributes: { person_id: 7, attributes: [{ definition, current: [value, ...extraValues.map((text, index) => ({ ...value, id: 42 + index, ordinal: index + 1, value: { type: 'text' as const, text } }))], history: [] }] }, etags: {}, errors: {} });
  const view = render(PersonFields, { controller });
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'Edit Note: Private example' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Note' }), { target: { value: 'Unsaved local note' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(controller.draft?.kind).toBe('setAttribute'));
    await act(() => controller.reloadDefinitions());
    return { controller, view };
  } catch (error) { view.unmount(); controller.destroy(); throw error; }
}

it.each([
  { name: 'rename keeps the draft', refreshed: { label: 'Renamed note' }, drafts: 1, draftKind: 'setAttribute', editLabel: 'Edit Renamed note: Private example', absent: false },
  { name: 'inactive field closes the draft', refreshed: { label: 'Private note', is_active: false }, drafts: 0, draftKind: undefined, editLabel: 'Edit Private note: Private example', absent: true },
])('joins refreshed field settings into an open draft: $name', async ({ refreshed, drafts, draftKind, editLabel, absent }) => {
  const { controller, view } = await refreshOpenField(refreshed);
  try {
    expect(screen.queryAllByDisplayValue('Unsaved local note')).toHaveLength(drafts);
    expect(controller.draft?.kind).toBe(draftKind);
    if (absent) {
      expect(screen.getByRole('button', { name: 'Remove Private note: Private example' })).toBeDefined();
      expect(screen.queryByRole('button', { name: editLabel })).toBeNull();
    } else expect(screen.getByRole('button', { name: editLabel })).toHaveProperty('disabled', false);
  } finally { view.unmount(); controller.destroy(); }
});


it('closes the field draft and conceals values when refreshed settings mark the field sensitive', async () => {
  const { controller, view } = await refreshOpenField({ label: 'Private note', is_sensitive: true }, 'multi', ['Second private example']);
  try {
    expect(screen.queryByDisplayValue('Unsaved local note')).toBeNull();
    expect(controller.draft).toBeNull();
    expect(screen.queryByRole('button', { name: 'Edit Private note: value 1' })).toBeNull();
    for (const position of [1, 2]) {
      expect(screen.getByRole('button', { name: `Remove Private note: value ${position}` })).toHaveProperty('disabled', true);
      expect(screen.getByRole('button', { name: `Show Private note: value ${position}` })).toBeDefined();
    }
    expect(screen.queryByText('Private example')).toBeNull();
    expect(screen.queryByText('Second private example')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Show Private note: value 1' }));
    expect(screen.getByText('Private example')).toBeDefined();
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

it('orders unused additions, offers populated kinds in More fields, and refreshes birthday availability', async () => {
  const initial = profile();
  initial.contact_points = [{ person_id: 7, identity_key: identityKeyFixture('email', 'old@example.com'), address_kind: 'email', original_value: 'old@example.com', normalized_value: 'old@example.com', normalization: 'email', normalization_version: 1, envelope }];
  const refreshed = { ...initial, dates: [{ person_id: 7, date_kind: 'birthday', date: { year: 1990, month: 5, day: 6 }, original_value: '1990-05-06', envelope: { ...envelope, id: 30, ordinal: 0 } }] };
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path.endsWith('/profile')) return Response.json(refreshed, { headers: { ETag: '"profile-7-r2"' } });
    if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
    if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
    if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: when, stale: false });
    if (path === '/api/v1/people/7') return Response.json(person, { headers: { ETag: '"person-7-r1"' } });
    throw new Error(`unexpected ${request.method} ${path}`);
  });
  const controller = new DirectoryProfileController(client, 7, { person, structuredProfile: initial, attributes: { person_id: 7, attributes: [] }, etags: { person: '"person-7-r1"', structuredProfile: '"profile-7-r1"' }, errors: {} });
  const view = render(PersonFields, { controller });
  try {
    const fields = view.container.querySelector('.fields')!;
    const add = fields.querySelector('.add')!;
    expect([...add.querySelectorAll('button')].map(button => button.textContent?.trim())).toEqual(['Add phone', 'Add address', 'Add birthday', 'More fields']);
    expect(fields.lastElementChild).toBe(add);
    expect(screen.queryByRole('button', { name: 'Add email' })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'More fields' }));
    expect(await screen.findByRole('menuitem', { name: 'Email' })).toBeDefined();
    await fireEvent.keyDown(screen.getByRole('menu', { name: 'More fields' }), { key: 'Escape' });
    await act(async () => { await controller.reload(); });
    expect(screen.getByText('1990-05-06')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Add birthday' })).toBeNull();
  } finally { view.unmount(); controller.destroy(); }
});

it('conceals and discards a sensitive new-value draft when Hide is clicked during a pending save', async () => {
  const definition = { id: 9, universal_id: '00000000-0000-4000-8000-000000000009', slug: 'religion', label: 'Religion', value_type: 'text', field_type: 'text', cardinality: 'single', object_type: 'person', display_order: 1, ownership: 'user', is_active: true, api_mutable: true, ui_editable: true, ui_creatable: true, is_sensitive: true, revision: 1 } as AttributeDefinition;
  let resolveSave!: (response: Response) => void;
  const pendingSave = new Promise<Response>(resolve => { resolveSave = resolve; });
  const writes: Request[] = [];
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/attribute-definitions') return Response.json({ definitions: [definition] });
    if (request.method === 'PUT' && path === '/api/v1/people/7/attributes/religion') { writes.push(request.clone()); return pendingSave; }
    throw new Error(`unexpected ${request.method} ${path}`);
  });
  const controller = new DirectoryProfileController(client, 7, { person, structuredProfile: profile(), attributes: { person_id: 7, attributes: [] }, definitions: { definitions: [] }, etags: { person: '"person-7-r1"', structuredProfile: '"profile-7-r1"' }, errors: {} });
  const view = render(PersonFields, { controller });
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'More fields' }));
    await fireEvent.click(await screen.findByRole('menuitem', { name: 'Religion' }));
    await fireEvent.input(await screen.findByRole('textbox', { name: 'Religion' }), { target: { value: 'Private new value' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveProperty('disabled', true);
    await fireEvent.click(screen.getByRole('button', { name: 'Hide Religion' }));
    expect(screen.queryByRole('textbox', { name: 'Religion' })).toBeNull();
    expect(view.container.innerHTML).not.toContain('Private new value');
    expect(controller.draft).toBeNull();
  } finally {
    await act(() => resolveSave(Response.json({ message: 'Save unavailable' }, { status: 503 })));
    await waitFor(() => expect(controller.mutationPending).toBe(false));
    view.unmount(); controller.destroy();
  }
});
