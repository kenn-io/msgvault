import { pathOf, person } from './entity-fixtures.test-support';
import { emptyProfile } from './profile-fixtures.test-support';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import type { AttributeDefinition, DirectoryReadBundle, PersonAttributeValue } from './models';
import { DirectoryProfileController } from './profile-controller.svelte';

function profile(id = 7, revision = 3) {
  return emptyProfile(person(id, revision, [], 'Test User'));
}

function attributes(id = 7) {
  return { person_id: id, attributes: [] };
}

function bundle(id = 7): DirectoryReadBundle {
  return {
    person: person(id, 3, [], 'Test User'),
    structuredProfile: profile(id),
    attributes: attributes(id),
    etags: { person: '"person-7-r3"', structuredProfile: '"profile-7-r3"' },
    errors: {}
  };
}

function requests(fetchFn: ReturnType<typeof vi.fn>): Request[] {
  return fetchFn.mock.calls.map(([input]) => input instanceof Request ? input : new Request(input));
}

describe('DirectoryProfileController', () => {
  it.each(['pending attribute save', 'pending profile reload', 'pending structured PATCH'])('refreshes linked bindings after a %s and permits the next structured edit', async cause => {
    let releaseSave!: (response: Response) => void;
    let releaseReload!: (response: Response) => void;
    const pendingSave = new Promise<Response>(resolve => releaseSave = resolve);
    const pendingReload = new Promise<Response>(resolve => releaseReload = resolve);
    let holdProfileRead = false;
    let staleProfile: Response | undefined;
    let linked = false;
    const patches: Request[] = [];
    const definition = { id: 1, universal_id: '00000000-0000-4000-8000-000000000001', object_type: 'person', slug: 'note', label: 'Note', value_type: 'text', field_type: 'text', cardinality: 'single', display_order: 0, ownership: 'user', ui_creatable: true, ui_editable: true, api_mutable: true, is_sensitive: false, is_active: true, revision: 1 };
    const value = { id: 41, person_id: 7, definition_id: 1, definition_slug: 'note', ordinal: 0, value: { type: 'text', text: 'A note' }, active_from: '2026-08-01T00:00:00Z', created_at: '2026-08-01T00:00:00Z', source: 'user' };
    const currentPerson = () => person(7, linked ? 4 : cause === 'pending structured PATCH' && patches.length === 0 ? 2 : 3, linked ? [11, 12] : [11], 'Synthetic Person');
    const profile = () => ({ person: currentPerson(), names: cause === 'pending structured PATCH' && patches.length > 0 ? [{ person_id: 7, name_kind: 'nickname', formatted: 'Saved nickname', original_value: 'Saved nickname', is_derived: false, envelope: { id: 81, ordinal: 0, source: 'user', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', vcard: {} } }] : [], contact_points: [], addresses: [], dates: [], categories: [], media: [] });
    const client = createAPIClient(async input => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/7/attributes/note') return pendingSave;
      if (path === '/api/v1/people/7/profile' && request.method === 'PATCH') {
        patches.push(request);
        if (cause === 'pending structured PATCH' && patches.length === 1) { staleProfile = Response.json(profile(), { headers: { ETag: '"person-7-r3"' } }); return pendingSave; }
        if (request.headers.get('If-Match') !== '"person-7-r4"') return Response.json({ error: 'person_revision_conflict', message: 'Reload this profile.' }, { status: 409 });
      }
      if (path === '/api/v1/people/7/contact-state') return Response.json({ person_id: 7, interaction_count: linked ? 1 : 3, last_contact_at: linked ? '2026-08-01T00:00:00Z' : '2026-08-15T00:00:00Z', cadence_status: linked ? 'overdue' : 'ok', computed_at: '2026-08-20T00:00:00Z', stale: false });
      if (path === '/api/v1/people/7') return Response.json(currentPerson(), { headers: { ETag: `"person-7-r${currentPerson().revision}"` } });
      if (path === '/api/v1/people/7/profile') {
        const response = Response.json(profile(), { headers: { ETag: `"person-7-r${currentPerson().revision}"` } });
        if (holdProfileRead) { holdProfileRead = false; staleProfile = response; return pendingReload; }
        return response;
      }
      if (path === '/api/v1/attribute-definitions') return Response.json({ definitions: [definition] });
      if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [{ definition, current: linked ? [value] : [], history: [] }] });
      throw new Error(`Unexpected request: ${path}`);
    });
    const controller = new DirectoryProfileController(client, 7, bundle());
    let save: ReturnType<typeof controller.setAttribute> | undefined;
    let reload: ReturnType<typeof controller.reload> | undefined;
    try {
      await controller.reload();
      if (cause === 'pending attribute save') save = controller.setAttribute('note', { value: { type: 'text', text: 'A note' } });
      if (cause === 'pending structured PATCH') save = controller.patchProfile({ names: { add: [{ name_kind: 'nickname', formatted: 'Saved nickname', envelope: { source: 'user' } }] } });
      if (cause === 'pending attribute save' || cause === 'pending structured PATCH') expect(controller.mutationPending).toBe(true);
      if (cause === 'pending profile reload') {
        holdProfileRead = true;
        reload = controller.reload();
        await vi.waitFor(() => expect(staleProfile).toBeDefined());
      }
      linked = true;
      if (cause === 'pending profile reload') await controller.reload();
      releaseSave(cause === 'pending structured PATCH' ? staleProfile! : Response.json({ dry_run: false, value }));
      await save;
      releaseReload(staleProfile ?? Response.json({}));
      await reload;
      expect(controller.person?.participant_ids).toEqual([11, 12]);
      expect(controller.structuredProfile?.person.participant_ids).toEqual([11, 12]);
      expect(controller.personETag).toBe('"person-7-r4"');
      expect(controller.structuredProfileETag).toBe('"person-7-r4"');
      await controller.patchProfile({ contact_points: { add: [{ address_kind: 'phone', original_value: '+12025550123', envelope: { source: 'user' } }] } });
      expect(patches).toHaveLength(cause === 'pending structured PATCH' ? 2 : 1);
      expect(patches.at(-1)!.headers.get('If-Match')).toBe('"person-7-r4"');
      expect(controller.conflict).toBeNull();
    } finally {
      releaseSave(Response.json({ message: 'Save unavailable' }, { status: 503 }));
      releaseReload(staleProfile ?? Response.json({ message: 'Profile unavailable' }, { status: 503 }));
      await Promise.all([save, reload]);
      controller.destroy();
    }
  });

  it('notifies deletion with a cleared draft without reading the deleted person', async () => {
    const fetchFn = vi.fn<typeof fetch>(async input => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'DELETE') return new Response(null, { status: 204 });
      return Response.json({ error: 'person_not_found', message: 'Person not found.' }, { status: 404 });
    });
    let readsBeforeNotification: Request[] = [];
    let draftAtNotification: unknown;
    const onDeleted = vi.fn(() => {
      readsBeforeNotification = requests(fetchFn).filter(request => request.method === 'GET' && pathOf(request).startsWith('/api/v1/people/7'));
      draftAtNotification = controller.draft;
    });
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle(), { onDeleted });
    try {
      await controller.deletePerson();
      expect(onDeleted).toHaveBeenCalledWith(7);
      expect(readsBeforeNotification).toEqual([]);
      expect(draftAtNotification).toBeNull();
      expect(controller.draft).toBeNull();
    } finally { controller.destroy(); }
  });

  it.each(['save', 'clear'] as const)('publishes refreshed contact state and attributes after an attribute write (%s)', async (operation) => {
    const when = '2026-08-04T00:00:00Z';
    const definition: AttributeDefinition = {
      id: 8, universal_id: '34b52841-dcf5-40c6-a24e-628b049e85b2', object_type: 'person', slug: 'contact_frequency', label: 'Contact frequency',
      value_type: 'integer', field_type: 'duration', cardinality: 'single', ownership: 'system', display_order: 20,
      is_active: true, api_mutable: true, ui_editable: true, ui_creatable: true, is_sensitive: false, revision: 1, options: { unit: 'days' },
      created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', history_exempt: false, is_audited: true, is_deletable: false, is_required: false, is_searchable: false,
    };
    const previous: PersonAttributeValue = { id: 41, person_id: 7, definition_id: 8, definition_slug: 'contact_frequency', ordinal: 0, value: { type: 'integer', integer: 2 }, source: 'user', active_from: '2026-08-01T00:00:00Z', created_at: '2026-08-01T00:00:00Z' };
    const superseded = { ...previous, active_until: when };
    let current = [previous];
    const contactState = { person_id: 7, interaction_count: 1, last_contact_at: '2026-08-01T00:00:00Z', cadence_status: 'overdue', cadence_due_at: '2026-08-03T00:00:00Z', computed_at: when, stale: false };
    const onDetailChange = vi.fn();
    const client = createAPIClient(async input => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path.endsWith('/attributes/contact_frequency')) {
        if (request.method === 'PUT') {
          const body = await request.json();
          current = [{ ...previous, id: 42, value: body.value, active_from: when, created_at: when }];
          return Response.json({ value: current[0], superseded });
        }
        if (request.method === 'DELETE') { current = []; return Response.json({ superseded }); }
      }
      if (path.endsWith('/contact-state')) {
        const days = current[0]?.value.integer;
        return Response.json({ ...contactState, cadence_status: days === undefined ? 'unknown' : days === 4 ? 'ok' : 'overdue', cadence_due_at: days === 4 ? '2026-08-05T00:00:00Z' : days === 2 ? '2026-08-03T00:00:00Z' : undefined });
      }
      if (path === '/api/v1/people/7') return Response.json(person(7, 3, [], 'Test User'), { headers: { ETag: '"person-7-r3"' } });
      if (path.endsWith('/profile')) return Response.json(profile(), { headers: { ETag: '"profile-7-r3"' } });
      if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [{ definition, current, history: [superseded] }] });
      if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [definition] });
      throw new Error(`unexpected ${request.method} ${path}`);
    });
    const initial = { ...bundle(), contactState, definitions: { definitions: [definition] }, attributes: { person_id: 7, attributes: [{ definition, current: [previous], history: [] }] } };
    const controller = new DirectoryProfileController(client, 7, initial, { onDetailChange });
    try {
      if (operation === 'save') await controller.setAttribute('contact_frequency', { value: { type: 'integer', integer: 4 }, expected_value_id: 41 });
      else await controller.clearAttribute('contact_frequency', 41);
      expect(onDetailChange).toHaveBeenLastCalledWith(expect.objectContaining({
        contactState: expect.objectContaining({ cadence_status: operation === 'save' ? 'ok' : 'unknown' }),
        attributes: expect.objectContaining({ attributes: [expect.objectContaining({ current: operation === 'save' ? [expect.objectContaining({ value: { type: 'integer', integer: 4 } })] : [] })] }),
      }));
    } finally { controller.destroy(); }
  });

  it('sends the latest profile ETag and replaces the structured resource from the response', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => new Response(JSON.stringify(profile(7, 4)), { headers: { ETag: '"profile-7-r4"' } }));
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());
    const patch = { names: { add: [], supersede: [] } };

    await controller.patchProfile(patch);

    expect(requests(fetchFn)[0]!.headers.get('If-Match')).toBe('"profile-7-r3"');
    expect(controller.structuredProfileETag).toBe('"profile-7-r4"');
    expect(controller.structuredProfile).toEqual(profile(7, 4));
    expect(controller.draft).toBeNull();
  });

  it('reloads a conflicted draft, recovers attribute and contact-state errors, and publishes refreshed detail', async () => {
    let attributesUnavailable = true;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'PATCH') return Response.json({ error: 'person_revision_conflict', message: 'changed elsewhere' }, { status: 409 });
      if (pathOf(request) === '/api/v1/people/7') return new Response(JSON.stringify(person(7, 4, [], 'Test User')), { headers: { ETag: '"person-7-r4"' } });
      if (pathOf(request).endsWith('/profile')) return new Response(JSON.stringify(profile(7, 4)), { headers: { ETag: '"profile-7-r4"' } });
      if (pathOf(request).endsWith('/attributes')) return attributesUnavailable
        ? Response.json({ message: 'attributes unavailable' }, { status: 503 })
        : Response.json(attributes());
      if (pathOf(request) === '/api/v1/attribute-definitions') return Response.json({ definitions: [] });
      if (pathOf(request) === '/api/v1/people/7/contact-state') return Response.json({ person_id: 7, interaction_count: 0, cadence_status: 'unknown', computed_at: '2026-08-20T00:00:00Z', stale: false });
      throw new Error(`unexpected ${request.url}`);
    });
    const onDetailChange = vi.fn();
    const initial = bundle();
    initial.errors.contactState = 'initial contact state read failed';
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, initial, { onDetailChange });
    const patch = { names: { add: [], supersede: [] } };

    await controller.patchProfile(patch);

    expect(controller.conflict).toMatchObject({ code: 'person_revision_conflict' });
    expect(controller.draft).toEqual({ kind: 'profile', body: patch });
    expect(controller.canWriteProfile).toBe(false);
    expect(requests(fetchFn)).toHaveLength(1);

    const draft = controller.draft, conflict = controller.conflict;
    await controller.reload(false);
    expect(controller.person?.revision).toBe(4);
    expect(controller.draft).toBe(draft);
    expect(controller.conflict).toBe(conflict);

    await controller.reload();
    expect(controller.attributesError).toBe('attributes unavailable');
    expect(onDetailChange).toHaveBeenLastCalledWith(expect.objectContaining({
      contactState: { person_id: 7, interaction_count: 0, cadence_status: 'unknown', computed_at: '2026-08-20T00:00:00Z', stale: false },
      errors: {},
    }));
    expect(controller.draft).toBeNull();
    expect(controller.conflict).toBeNull();
    expect(controller.structuredProfileETag).toBe('"profile-7-r4"');
    expect(controller.canWriteProfile).toBe(true);

    attributesUnavailable = false;
    await controller.reload();
    expect(controller.attributesError).toBeNull();
    expect(onDetailChange).toHaveBeenLastCalledWith(expect.objectContaining({ attributes: attributes() }));
  });

  it('retains the exact display-name draft on a missing precondition until reload', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ error: 'precondition_required', message: 'read first' }, { status: 428 }));
    const noETag = bundle();
    noETag.etags.person = undefined;
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, noETag);

    await controller.rename('Alice Example');

    expect(fetchFn).not.toHaveBeenCalled();
    expect(controller.draft).toEqual({ kind: 'rename', body: { display_name: 'Alice Example' } });
    expect(controller.conflict).toMatchObject({ code: 'precondition_required' });
  });

  it('retains a server precondition draft until an explicit reload obtains a new ETag', async () => {
    const patch = { names: { add: [], supersede: [] } };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'PATCH') return Response.json({ error: 'precondition_required', message: 'read again' }, { status: 428 });
      if (pathOf(request) === '/api/v1/people/7') return new Response(JSON.stringify(person(7, 4, [], 'Test User')), { headers: { ETag: '"person-7-r4"' } });
      if (pathOf(request).endsWith('/profile')) return new Response(JSON.stringify(profile(7, 4)), { headers: { ETag: '"profile-7-r4"' } });
      if (pathOf(request).endsWith('/attributes')) return Response.json(attributes());
      return Response.json({ definitions: [] });
    });
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    await controller.patchProfile(patch);

    expect(controller.draft).toEqual({ kind: 'profile', body: patch });
    expect(controller.conflict).toMatchObject({ code: 'precondition_required', status: 428 });
    expect(controller.structuredProfileETag).toBe('"profile-7-r3"');
    expect(controller.canWriteProfile).toBe(false);

    await controller.reload();
    expect(controller.draft).toBeNull();
    expect(controller.structuredProfileETag).toBe('"profile-7-r4"');
    expect(controller.canWriteProfile).toBe(true);
  });

  it('requires a changed strong profile ETag before clearing a revision-conflict gate', async () => {
    const patch = { names: { add: [], supersede: [] } };
    let profileReloads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'PATCH') return Response.json({ error: 'person_revision_conflict', message: 'changed elsewhere' }, { status: 409 });
      if (pathOf(request) === '/api/v1/people/7') return new Response(JSON.stringify(person(7, 4, [], 'Test User')), { headers: { ETag: '"person-7-r4"' } });
      if (pathOf(request).endsWith('/profile')) {
        profileReloads += 1;
        return new Response(JSON.stringify(profile(7, 4)), { headers: { ETag: profileReloads === 1 ? '"profile-7-r3"' : '"profile-7-r4"' } });
      }
      if (pathOf(request).endsWith('/attributes')) return Response.json(attributes());
      return Response.json({ definitions: [] });
    });
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    await controller.patchProfile(patch);
    await controller.reload();

    expect(controller.draft).toEqual({ kind: 'profile', body: patch });
    expect(controller.conflict).toMatchObject({ code: 'precondition_required' });
    expect(controller.canWriteProfile).toBe(false);

    await controller.reload();
    expect(controller.draft).toBeNull();
    expect(controller.structuredProfileETag).toBe('"profile-7-r4"');
    expect(controller.canWriteProfile).toBe(true);
  });

  it('retains a domain 409 as a request failure without requiring a revision reload', async () => {
    const patch = { categories: { add: [{ original_value: 'Friends', envelope: { source: 'user' } }], supersede: [] } };
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      error: 'duplicate_category', message: 'Category already exists.'
    }, { status: 409 }));
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    await controller.patchProfile(patch);

    expect(controller.draft).toEqual({ kind: 'profile', body: patch });
    expect(controller.conflict).toMatchObject({ code: 'request_failed', status: 409, message: 'Category already exists.' });
    expect(controller.canWriteProfile).toBe(true);
  });

  it('keeps a 428 structured-profile draft when only that reload resource fails', async () => {
    const patch = { names: { add: [], supersede: [] } };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'PATCH') return Response.json({ error: 'precondition_required', message: 'read again' }, { status: 428 });
      if (pathOf(request) === '/api/v1/people/7') return new Response(JSON.stringify(person(7, 4, [], 'Test User')), { headers: { ETag: '"person-7-r4"' } });
      if (pathOf(request).endsWith('/profile')) return Response.json({ error: 'unavailable', message: 'profile reload unavailable' }, { status: 503 });
      if (pathOf(request).endsWith('/attributes')) return Response.json(attributes());
      return Response.json({ definitions: [] });
    });
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    await controller.patchProfile(patch);
    await controller.reload();

    expect(controller.draft).toEqual({ kind: 'profile', body: patch });
    expect(controller.conflict).toMatchObject({ code: 'request_failed', status: 503, message: 'profile reload unavailable' });
    expect(controller.structuredProfileETag).toBe('"profile-7-r3"');
  });

  it('renames with the person ETag and invalidates only the changed Directory row', async () => {
    const invalidated: number[] = [];
    const fetchFn = vi.fn<typeof fetch>(async () => new Response(JSON.stringify(person(7, 4, [], 'Alice Example')), { headers: { ETag: '"person-7-r4"' } }));
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle(), {
      invalidateRow: (id) => {
        invalidated.push(id);
      }
    });

    await controller.rename('Alice Example');

    const request = requests(fetchFn)[0]!;
    expect(request.headers.get('If-Match')).toBe('"person-7-r3"');
    await expect(request.clone().json()).resolves.toEqual({ display_name: 'Alice Example' });
    expect(controller.personETag).toBe('"person-7-r4"');
    expect(controller.person?.display_name).toBe('Alice Example');
    expect(invalidated).toEqual([7]);
  });

  it('uses the revision returned by a rename for the next structured-profile patch', async () => {
    let write = 0;
    const fetchFn = vi.fn<typeof fetch>(async input => {
      if ((input instanceof Request ? input : new Request(input)).method !== 'PATCH') return Response.json({ error: 'unavailable', message: 'Refresh unavailable' }, { status: 503 });
      write += 1;
      return write === 1
        ? new Response(JSON.stringify(person(7, 4, [], 'Alice Example')), { headers: { ETag: '"person-7-r4"' } })
        : new Response(JSON.stringify(profile(7, 5)), { headers: { ETag: '"person-7-r5"' } });
    });
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    await controller.rename('Alice Example');
    await controller.patchProfile({ names: { add: [], supersede: [] } });

    expect(requests(fetchFn).filter(request => request.method === 'PATCH')[1]!.headers.get('If-Match')).toBe('"person-7-r4"');
    expect(controller.personETag).toBe('"person-7-r5"');
  });

  it('invalidates the Directory row with exact current categories after a profile write', async () => {
    const invalidated: Array<[number, unknown]> = [];
    const updatedProfile = {
      ...profile(7, 4),
      categories: [{
        person_id: 7,
        original_value: 'Close Friends',
        normalized_value: 'close friends',
        envelope: { id: 31, ordinal: 0, source: 'user', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', vcard: {} }
      }]
    };
    const fetchFn = vi.fn<typeof fetch>(async () => new Response(JSON.stringify(updatedProfile), { headers: { ETag: '"person-7-r4"' } }));
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle(), {
      invalidateRow: (id, update) => { invalidated.push([id, update]); }
    });

    await controller.patchProfile({ categories: { add: [{ original_value: 'Close Friends', envelope: { source: 'user' } }] } });

    expect(invalidated).toEqual([[7, { categories: ['Close Friends'] }]]);
  });

  it.each([
    ['name', { names: { supersede: [41] } }],
    ['contact point', { contact_points: { supersede: [51] } }]
  ])('requests a Directory refresh after a %s profile write', async (_field, body) => {
    const invalidated: Array<[number, unknown, boolean | undefined]> = [];
    const fetchFn = vi.fn<typeof fetch>(async () => new Response(JSON.stringify(profile(7, 4)), { headers: { ETag: '"person-7-r4"' } }));
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle(), {
      invalidateRow: (id, update, refreshDirectory) => { invalidated.push([id, update, refreshDirectory]); }
    });

    await controller.patchProfile(body);

    expect(invalidated).toEqual([[7, undefined, true]]);
  });

  it('does not treat seeded preferred-channel writes as Directory summary changes', async () => {
    const invalidated: Array<[number, unknown]> = [];
    let write = 0;
    const current = {
      id: 41, person_id: 7, definition_id: 1, definition_slug: 'primary_channel', ordinal: 0,
      value: { type: 'text' as const, text: 'chat' }, active_from: '2026-08-01T00:00:00Z',
      created_at: '2026-08-01T00:00:00Z', source: 'user'
    };
    const closed = { ...current, active_until: '2026-08-02T00:00:00Z', superseded_at: '2026-08-02T00:00:00Z' };
    const primaryDefinition = {
      id: 1, universal_id: 'primary-channel-id', object_type: 'person' as const, slug: 'primary_channel', label: 'Primary channel',
      value_type: 'text' as const, field_type: 'select', cardinality: 'single' as const,
      display_order: 0, stability: 'durable' as const, inference_eligible: false,
      is_required: false, ownership: 'system' as const, ui_creatable: true, ui_editable: true, api_mutable: true,
      is_searchable: false, is_sensitive: false, is_audited: true, is_deletable: false, history_exempt: false,
      options: { choices: [{ value: 'chat', label: 'Chat' }] }, is_active: true, revision: 1,
      created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z'
    };
    const seeded = bundle();
    seeded.attributes = { person_id: 7, attributes: [{ definition: primaryDefinition, current: [], history: [] }] };
    seeded.definitions = { definitions: [primaryDefinition] };
    const fetchFn = vi.fn<typeof fetch>(async () => {
      write += 1;
      return write === 1
        ? Response.json({ dry_run: false, value: current })
        : Response.json({ dry_run: false, superseded: closed });
    });
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, seeded, {
      invalidateRow: (id, update) => { invalidated.push([id, update]); }
    });

    await controller.setAttribute('primary_channel', { value: { type: 'text', text: 'chat' } });
    await controller.clearAttribute('primary_channel', current.id);

    expect(invalidated).toEqual([]);
  });

  it.each([
    ['single', 'single'],
    ['multi', 'multi']
  ] as const)('synthesizes the first %s attribute group from the refreshed definition even if the follow-up read fails', async (_name, cardinality) => {
    const createdDefinition = {
      id: 71, universal_id: `created-${cardinality}`, object_type: 'person' as const, slug: `created_${cardinality}`,
      label: `Created ${cardinality}`, value_type: 'text' as const, field_type: 'text', cardinality,
      display_order: 71, stability: 'durable' as const, inference_eligible: true,
      is_required: false, ownership: 'user' as const,
      ui_creatable: true, ui_editable: true,
      api_mutable: true, is_searchable: false, is_sensitive: false, is_audited: true, is_deletable: true,
      history_exempt: false, is_active: true, revision: 1, created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z'
    };
    const value = {
      id: 91, person_id: 7, definition_id: createdDefinition.id, definition_slug: createdDefinition.slug,
      ordinal: 0, value: { type: 'text' as const, text: 'First value' }, active_from: '2026-08-01T00:00:00Z',
      created_at: '2026-08-01T00:00:00Z', source: 'user'
    };
    const seeded = bundle();
    seeded.attributes = { person_id: 7, attributes: [] };
    seeded.definitions = { definitions: [createdDefinition] };
    const controller = new DirectoryProfileController(createAPIClient(vi.fn<typeof fetch>(async input =>
      (input instanceof Request ? input : new Request(input)).method === 'PUT' ? Response.json({ dry_run: false, value }) : Response.json({ error: 'unavailable', message: 'Refresh unavailable' }, { status: 503 })
    )), 7, seeded);

    await controller.setAttribute(createdDefinition.slug, { value: { type: 'text', text: 'First value' } });

    expect(controller.attributes?.attributes).toEqual([{
      definition: createdDefinition,
      current: [value],
      history: []
    }]);
  });

  it('rejects a rapid second rename without letting the first response erase its draft', async () => {
    let resolveFirst: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async input => (input instanceof Request ? input : new Request(input)).method === 'PATCH' ? new Promise<Response>((resolve) => { resolveFirst = resolve; }) : Response.json({ error: 'unavailable', message: 'Refresh unavailable' }, { status: 503 }));
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    const first = controller.rename('First name');
    await vi.waitFor(() => expect(resolveFirst).toBeDefined());
    void controller.rename('Later name');
    expect(requests(fetchFn)).toHaveLength(1);
    expect(controller.draft).toEqual({ kind: 'rename', body: { display_name: 'Later name' } });
    expect(controller.conflict).toMatchObject({ code: 'mutation_in_progress' });

    resolveFirst?.(new Response(JSON.stringify(person(7, 4, [], 'First name')), { headers: { ETag: '"person-7-r4"' } }));
    await first;
    expect(controller.draft).toEqual({ kind: 'rename', body: { display_name: 'Later name' } });
    expect(controller.conflict).toMatchObject({ code: 'mutation_in_progress' });
  });

  it('rejects a rapid second profile patch without reusing the stale ETag', async () => {
    let resolveFirst: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async input => (input instanceof Request ? input : new Request(input)).method === 'PATCH' ? new Promise<Response>((resolve) => { resolveFirst = resolve; }) : Response.json({ error: 'unavailable', message: 'Refresh unavailable' }, { status: 503 }));
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());
    const firstPatch = { names: { add: [], supersede: [] } };
    const laterPatch = { categories: { add: [], supersede: [] } };

    const first = controller.patchProfile(firstPatch);
    await vi.waitFor(() => expect(resolveFirst).toBeDefined());
    void controller.patchProfile(laterPatch);
    expect(requests(fetchFn)).toHaveLength(1);
    expect(controller.draft).toEqual({ kind: 'profile', body: laterPatch });
    expect(controller.conflict).toMatchObject({ code: 'mutation_in_progress' });

    resolveFirst?.(new Response(JSON.stringify(profile(7, 4)), { headers: { ETag: '"person-7-r4"' } }));
    await first;
    expect(controller.draft).toEqual({ kind: 'profile', body: laterPatch });
    expect(controller.conflict).toMatchObject({ code: 'mutation_in_progress' });
  });

  it('rejects reload during an in-flight profile write without losing its later 409 draft', async () => {
    let resolvePatch: ((response: Response) => void) | undefined;
    const patch = { names: { add: [], supersede: [] } };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'PATCH') return new Promise<Response>((resolve) => { resolvePatch = resolve; });
      if (pathOf(request) === '/api/v1/people/7') return new Response(JSON.stringify(person(7, 4, [], 'Test User')), { headers: { ETag: '"person-7-r4"' } });
      if (pathOf(request).endsWith('/profile')) return new Response(JSON.stringify(profile(7, 4)), { headers: { ETag: '"person-7-r4"' } });
      if (pathOf(request).endsWith('/attributes')) return Response.json(attributes());
      return Response.json({ definitions: [] });
    });
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    const write = controller.patchProfile(patch);
    await vi.waitFor(() => expect(resolvePatch).toBeDefined());
    await expect(controller.reload()).resolves.toEqual({ ok: false, code: 'mutation_in_progress' });
    expect(requests(fetchFn)).toHaveLength(1);
    expect(controller.draft).toEqual({ kind: 'profile', body: patch });
    expect(controller.structuredProfileETag).toBe('"profile-7-r3"');

    resolvePatch?.(Response.json({ error: 'person_revision_conflict', message: 'changed elsewhere' }, { status: 409 }));
    await write;
    expect(controller.draft).toEqual({ kind: 'profile', body: patch });
    expect(controller.conflict).toMatchObject({ code: 'person_revision_conflict', message: 'changed elsewhere' });
    expect(controller.canReload).toBe(true);
  });

  it('rejects a mutation while reload is in flight without sending a stale write', async () => {
    let resolvePerson: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (pathOf(request) === '/api/v1/people/7') return new Promise<Response>((resolve) => { resolvePerson = resolve; });
      if (pathOf(request).endsWith('/profile')) return new Response(JSON.stringify(profile(7, 4)), { headers: { ETag: '"person-7-r4"' } });
      if (pathOf(request).endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: '2026-08-20T00:00:00Z', stale: false });
      if (pathOf(request).endsWith('/attributes')) return Response.json(attributes());
      return Response.json({ definitions: [] });
    });
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    const reload = controller.reload();
    await vi.waitFor(() => expect(resolvePerson).toBeDefined());
    expect(controller.canWritePerson).toBe(false);
    expect(controller.canWriteProfile).toBe(false);
    await expect(controller.patchProfile({ names: { add: [], supersede: [] } })).resolves.toEqual({ ok: false, code: 'reload_in_progress' });
    expect(requests(fetchFn).some((request) => request.method === 'PATCH')).toBe(false);
    expect(controller.draft).toBeNull();
    expect(controller.conflict).toBeNull();

    resolvePerson?.(new Response(JSON.stringify(person(7, 4, [], 'Test User')), { headers: { ETag: '"person-7-r4"' } }));
    await expect(reload).resolves.toEqual({ ok: true });
    expect(controller.personETag).toBe('"person-7-r4"');
    expect(controller.canWritePerson).toBe(true);
    expect(controller.canWriteProfile).toBe(true);
  });

  it.each(['person', 'structuredProfile', 'all'])('publishes a failed %s refresh with cached data and clears the error on retry', async section => {
    let unavailable = true;
    const initial = bundle();
    initial.structuredProfile!.names = [{
      person_id: 7, name_kind: 'nickname', formatted: 'Moved nickname', original_value: 'Moved nickname', is_derived: false,
      envelope: { id: 81, ordinal: 0, source: 'user', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', vcard: {} },
    }];
    const onDetailChange = vi.fn();
    const client = createAPIClient(async input => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/7') return unavailable && section !== 'structuredProfile'
        ? Response.json({ message: 'Person unavailable' }, { status: 503 })
        : Response.json(person(7, 4), { headers: { ETag: '"person-7-r4"' } });
      if (path.endsWith('/profile')) return unavailable && section !== 'person'
        ? Response.json({ message: 'Profile unavailable' }, { status: 503 })
        : Response.json(profile(7, 4), { headers: { ETag: '"profile-7-r4"' } });
      if (unavailable && section === 'all') return Response.json({ message: 'Section unavailable' }, { status: 503 });
      if (path.endsWith('/attributes')) return Response.json(attributes());
      if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
      if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: '2026-08-20T00:00:00Z', stale: false });
      throw new Error(`Unexpected ${path}`);
    });
    const controller = new DirectoryProfileController(client, 7, initial, { onDetailChange });
    try {
      await controller.reload();
      expect(onDetailChange).toHaveBeenLastCalledWith(expect.objectContaining({
        person: section === 'structuredProfile' ? person(7, 4) : initial.person,
        structuredProfile: section === 'person' ? profile(7, 4) : initial.structuredProfile,
        errors: expect.objectContaining(section === 'person'
          ? { person: 'Person unavailable' }
          : section === 'structuredProfile'
            ? { structuredProfile: 'Profile unavailable' }
            : { person: 'Person unavailable', structuredProfile: 'Profile unavailable' }),
      }));

      unavailable = false;
      await controller.reload();
      expect(onDetailChange).toHaveBeenLastCalledWith(expect.objectContaining({ person: person(7, 4), structuredProfile: profile(7, 4), errors: {} }));
    } finally { controller.destroy(); }
  });

  it('restores write capabilities after reload failure leaves the prior ETags intact', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (pathOf(request) === '/api/v1/people/7' || pathOf(request).endsWith('/profile')) {
        return Response.json({ error: 'unavailable', message: 'reload unavailable' }, { status: 503 });
      }
      if (pathOf(request).endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: '2026-08-20T00:00:00Z', stale: false });
      if (pathOf(request).endsWith('/attributes')) return Response.json(attributes());
      return Response.json({ definitions: [] });
    });
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    const reload = controller.reload();
    expect(controller.canWritePerson).toBe(false);
    expect(controller.canWriteProfile).toBe(false);
    await expect(reload).resolves.toEqual({ ok: true });

    expect(controller.personETag).toBe('"person-7-r3"');
    expect(controller.structuredProfileETag).toBe('"profile-7-r3"');
    expect(controller.canWritePerson).toBe(true);
    expect(controller.canWriteProfile).toBe(true);
  });

  it.each([
    ['success', () => new Response(JSON.stringify(profile(7, 4)), { headers: { ETag: '"person-7-r4"' } })],
    ['failure', () => Response.json({ error: 'person_revision_conflict', message: 'changed elsewhere' }, { status: 409 })]
  ])('releases the operation gate after a profile write %s', async (_outcome, patchResponse) => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'PATCH') return patchResponse();
      if (pathOf(request) === '/api/v1/people/7') return new Response(JSON.stringify(person(7, 4, [], 'Test User')), { headers: { ETag: '"person-7-r4"' } });
      if (pathOf(request).endsWith('/profile')) return new Response(JSON.stringify(profile(7, 4)), { headers: { ETag: '"person-7-r4"' } });
      if (pathOf(request).endsWith('/attributes')) return Response.json(attributes());
      return Response.json({ definitions: [] });
    });
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    await controller.patchProfile({ names: { add: [], supersede: [] } });

    expect(controller.mutationPending).toBe(false);
    expect(controller.canReload).toBe(true);
    await expect(controller.reload()).resolves.toEqual({ ok: true });
  });

  it('sets an attribute with user provenance and its exact compare-and-swap values', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ dry_run: false, value: { id: 20, person_id: 7, definition_id: 1, definition_slug: 'alias', ordinal: 2, source: 'user', active_from: '2026-08-01T00:00:00Z', created_at: '2026-08-01T00:00:00Z', value: { type: 'text', text: 'Alice' } } }));
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    await controller.setAttribute('alias', { value: { type: 'text', text: 'Alice' }, expected_value_id: 19, ordinal: 2 });

    const request = requests(fetchFn)[0]!;
    expect(pathOf(request)).toBe('/api/v1/people/7/attributes/alias');
    await expect(request.clone().json()).resolves.toEqual({ value: { type: 'text', text: 'Alice' }, expected_value_id: 19, ordinal: 2, source: 'user' });
  });

  it('clears a multi-value attribute with its exact value ID and ordinal', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ dry_run: false }));
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    await controller.clearAttribute('alias', 19, 2);

    const query = new URL(requests(fetchFn)[0]!.url).searchParams;
    expect(query.get('expected_value_id')).toBe('19');
    expect(query.get('ordinal')).toBe('2');
  });

  it('clears a single-value attribute with only its exact current value ID', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ dry_run: false }));
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    await controller.clearAttribute('alias', 19);

    const query = new URL(requests(fetchFn)[0]!.url).searchParams;
    expect(query.get('expected_value_id')).toBe('19');
    expect(query.has('ordinal')).toBe(false);
  });

  it('retains an attribute draft when the server returns a typed CAS conflict', async () => {
    const body = { value: { type: 'text', text: 'Alice' }, expected_value_id: 19, ordinal: 2 };
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      error: 'attribute_value_conflict', message: 'changed elsewhere', current_value_id: 21,
      current_value: { id: 21, person_id: 7, definition_id: 1, definition_slug: 'alias', ordinal: 2, source: 'user', active_from: '2026-08-01T00:00:00Z', created_at: '2026-08-01T00:00:00Z', value: { type: 'text', text: 'Updated' } }
    }, { status: 409 }));
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    await controller.setAttribute('alias', body);

    expect(controller.draft).toEqual({ kind: 'setAttribute', slug: 'alias', body: { ...body, source: 'user' } });
    expect(controller.conflict).toMatchObject({ code: 'attribute_conflict', currentValueID: 21 });
  });

  it('keeps an unresolved attribute conflict as a controller-wide mutation gate', async () => {
    const firstBody = { value: { type: 'text', text: 'Local alias' }, expected_value_id: 19 };
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      error: 'attribute_value_conflict', message: 'changed elsewhere', current_value_id: 21
    }, { status: 409 }));
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    await controller.setAttribute('alias', firstBody);
    const retainedDraft = controller.draft;
    const retainedConflict = controller.conflict;

    await expect(controller.setAttribute('nickname', { value: { type: 'text', text: 'Second write' } }))
      .resolves.toEqual({ ok: false, code: 'conflict_unresolved' });
    await expect(controller.patchProfile({ names: { add: [], supersede: [] } }))
      .resolves.toEqual({ ok: false, code: 'conflict_unresolved' });

    expect(requests(fetchFn)).toHaveLength(1);
    expect(controller.draft).toBe(retainedDraft);
    expect(controller.conflict).toBe(retainedConflict);
    expect(controller.canWriteProfile).toBe(false);
  });

  it('retains the exact draft when a write receives a non-JSON error response', async () => {
    const patch = { names: { add: [], supersede: [] } };
    const fetchFn = vi.fn<typeof fetch>(async () => new Response('upstream unavailable', { status: 503, headers: { 'Content-Type': 'text/plain' } }));
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    await controller.patchProfile(patch);

    expect(controller.draft).toEqual({ kind: 'profile', body: patch });
    expect(controller.conflict).toMatchObject({ code: 'request_failed', status: 503 });
  });

  it.each([
    ['registry retry', 'success'],
    ['registry retry', 'failure'],
    ['profile reload', 'success'],
    ['profile reload', 'failure'],
  ])('ignores a superseded %s registry response (%s)', async (olderRead, outcome) => {
    const stale = { id: 41, slug: 'favorite_color', label: 'Favorite color' };
    const current = { ...stale, label: 'Preferred color' };
    let reads = 0;
    let release!: (response: Response) => void;
    const pending = new Promise<Response>(resolve => { release = resolve; });
    const client = createAPIClient(async input => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path.endsWith('/attribute-definitions')) {
        reads += 1;
        return reads === 1 ? pending : Response.json({ definitions: [current] });
      }
      if (path === '/api/v1/people/7') return Response.json(person(7, 3, [], 'Test User'), { headers: { ETag: '"person-7-r3"' } });
      if (path.endsWith('/profile')) return Response.json(profile(), { headers: { ETag: '"profile-7-r3"' } });
      if (path.endsWith('/attributes')) return Response.json(attributes());
      if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: '2026-08-20T00:00:00Z', stale: false });
      throw new Error(`Unexpected ${path}`);
    });
    const controller = new DirectoryProfileController(client, 7, bundle());
    const older = olderRead === 'registry retry' ? controller.reloadDefinitions() : controller.reload();
    try {
      await vi.waitFor(() => expect(reads).toBe(1));
      if (olderRead === 'registry retry') await controller.reload();
      else await controller.reloadDefinitions();
      expect(controller.definitions).toEqual([current]);

      release(outcome === 'success'
        ? Response.json({ definitions: [stale] })
        : Response.json({ message: 'Registry unavailable' }, { status: 503 }));
      await older;
      expect(controller.definitions).toEqual([current]);
      expect(controller.definitionsError).toBeNull();
    } finally {
      release(Response.json({ definitions: [] }));
      await older;
      controller.destroy();
    }
  });

  it.each([false, true])('loads and refreshes definitions after creating a user definition (initial read pending=%s)', async pending => {
    let listed = 0; let finishInitial!: (response: Response) => void;
    const initialResponse = new Promise<Response>(resolve => { finishInitial = resolve; });
    const created = { id: 4, universal_id: 'field-4', slug: 'preferred-channel', label: 'Preferred channel', field_type: 'choice', value_type: 'text', cardinality: 'single', object_type: 'person', ownership: 'user', api_mutable: true, history_exempt: false, display_order: 0, is_active: true, is_audited: false, is_deletable: true, is_required: false, is_searchable: true, is_sensitive: true, ui_creatable: true, ui_editable: true, revision: 1, created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z' };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'POST') return Response.json(created, { status: 201 });
      listed += 1;
      if (listed === 1 && pending) return initialResponse;
      return Response.json({ definitions: listed === 1 ? [] : [created] });
    });
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    const initial = controller.reloadDefinitions();
    if (!pending) await initial;
    await controller.createDefinition({ label: 'Preferred channel', field_type: 'choice', value_type: 'text', object_type: 'person', cardinality: 'single', is_sensitive: true });

    finishInitial(Response.json({ definitions: [] }));
    await initial;
    expect(controller.createdDefinition).toEqual(created);
    expect(controller.definitions).toEqual([created]);
    const create = requests(fetchFn).find((request) => request.method === 'POST')!;
    await expect(create.clone().json()).resolves.toEqual({ label: 'Preferred channel', field_type: 'choice', value_type: 'text', object_type: 'person', cardinality: 'single', is_sensitive: true });
  });

  it('keeps the first create draft authoritative across a rapid direct second call', async () => {
    const created = { id: 4, universal_id: 'field-4', slug: 'preferred-channel', label: 'Preferred channel', field_type: 'choice', value_type: 'text', cardinality: 'single', object_type: 'person', ownership: 'user', api_mutable: true, history_exempt: false, display_order: 0, is_active: true, is_audited: false, is_deletable: true, is_required: false, is_searchable: true, is_sensitive: true, ui_creatable: true, ui_editable: true, revision: 1, created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z' };
    const firstBody = { label: 'Preferred channel', field_type: 'choice', value_type: 'text', object_type: 'person', cardinality: 'single', is_sensitive: true } as const;
    const laterBody = { label: 'Later field', field_type: 'text', value_type: 'text', object_type: 'person', cardinality: 'single', is_sensitive: false } as const;
    let resolvePost: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      return request.method === 'POST'
        ? new Promise<Response>((resolve) => { resolvePost = resolve; })
        : Response.json({ definitions: [created] });
    });
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    const first = controller.createDefinition(firstBody);
    await vi.waitFor(() => expect(resolvePost).toBeDefined());
    const retainedDraft = controller.draft;
    await expect(controller.createDefinition(laterBody)).resolves.toEqual({ ok: false, code: 'mutation_in_progress' });

    expect(controller.draft).toBe(retainedDraft);
    expect(controller.draft).toEqual({ kind: 'createDefinition', body: firstBody });
    expect(controller.conflict).toBeNull();
    expect(requests(fetchFn).filter((request) => request.method === 'POST')).toHaveLength(1);

    resolvePost?.(Response.json(created, { status: 201 }));
    await first;

    expect(requests(fetchFn).map((request) => request.method)).toEqual(['POST', 'GET']);
    expect(controller.createdDefinition).toBe(controller.definitions[0]);
    expect(controller.definitionCreationCommit).toBeNull();
    expect(controller.draft).toBeNull();
    expect(controller.conflict).toBeNull();

    await expect(controller.createDefinition(laterBody)).resolves.toEqual({ ok: false, code: 'conflict_unresolved' });
    expect(requests(fetchFn).filter((request) => request.method === 'POST')).toHaveLength(1);
  });

  it('reloads attributes with history and preserves current and historical values', async () => {
    const values = {
      person_id: 7,
      attributes: [{
        definition: { slug: 'alias' },
        current: [{ id: 20, person_id: 7, definition_id: 1, definition_slug: 'alias', ordinal: 0, source: 'user', active_from: '2026-08-02T00:00:00Z', created_at: '2026-08-02T00:00:00Z', value: { type: 'text', text: 'Current' } }],
        history: [{ id: 19, person_id: 7, definition_id: 1, definition_slug: 'alias', ordinal: 0, source: 'user', active_from: '2026-08-01T00:00:00Z', created_at: '2026-08-01T00:00:00Z', superseded_at: '2026-08-02T00:00:00Z', value: { type: 'text', text: 'Previous' } }]
      }]
    };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (pathOf(request) === '/api/v1/people/7') return new Response(JSON.stringify(person(7, 3, [], 'Test User')), { headers: { ETag: '"person-7-r3"' } });
      if (pathOf(request).endsWith('/profile')) return new Response(JSON.stringify(profile()), { headers: { ETag: '"person-7-r3"' } });
      if (pathOf(request).endsWith('/attributes')) return Response.json(values);
      return Response.json({ definitions: [] });
    });
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    await controller.reload();

    const attributeRequest = requests(fetchFn).find((request) => pathOf(request).endsWith('/attributes'))!;
    expect(new URL(attributeRequest.url).searchParams.get('history')).toBe('true');
    expect(controller.attributes).toEqual(values);
  });

  it('suppresses a disposed controller reload response instead of invalidating a newer selection', async () => {
    let resolvePerson: ((response: Response) => void) | undefined;
    const invalidated: number[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (pathOf(request) === '/api/v1/people/7') return new Promise<Response>((resolve) => { resolvePerson = resolve; });
      return Response.json(pathOf(request).endsWith('/profile') ? profile() : pathOf(request).endsWith('/attributes') ? attributes() : { definitions: [] });
    });
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle(), {
      invalidateRow: (id) => {
        invalidated.push(id);
      }
    });

    const reload = controller.reload();
    await vi.waitFor(() => expect(resolvePerson).toBeDefined());
    controller.destroy();
    resolvePerson?.(new Response(JSON.stringify(person(7, 4, [], 'Test User')), { headers: { ETag: '"person-7-r4"' } }));
    await reload;

    expect(controller.personETag).toBe('"person-7-r3"');
    expect(invalidated).toEqual([]);
  });
});
