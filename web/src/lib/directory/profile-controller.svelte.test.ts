import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import type { DirectoryReadBundle } from './models';
import { DirectoryProfileController } from './profile-controller.svelte';

function person(id = 7, revision = 3, displayName = 'Test User') {
  return {
    id,
    revision,
    display_name: displayName,
    participant_ids: [],
    created_at: '2026-08-01T00:00:00Z',
    updated_at: '2026-08-01T00:00:00Z',
    vcard_uid: `person-${id}`
  };
}

function profile(id = 7, revision = 3) {
  return { person: person(id, revision), names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] };
}

function attributes(id = 7) {
  return { person_id: id, attributes: [] };
}

function bundle(id = 7): DirectoryReadBundle {
  return {
    person: person(id),
    structuredProfile: profile(id),
    attributes: attributes(id),
    etags: { person: '"person-7-r3"', structuredProfile: '"profile-7-r3"' },
    errors: {}
  };
}

function requests(fetchFn: ReturnType<typeof vi.fn>): Request[] {
  return fetchFn.mock.calls.map(([input]) => input instanceof Request ? input : new Request(input));
}

function pathOf(request: Request): string {
  return new URL(request.url, document.baseURI).pathname;
}

describe('DirectoryProfileController', () => {
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

  it('preserves structured drafts and a typed conflict until an explicit reload', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'PATCH') return Response.json({ error: 'person_revision_conflict', message: 'changed elsewhere' }, { status: 409 });
      if (pathOf(request) === '/api/v1/people/7') return new Response(JSON.stringify(person(7, 4)), { headers: { ETag: '"person-7-r4"' } });
      if (pathOf(request).endsWith('/profile')) return new Response(JSON.stringify(profile(7, 4)), { headers: { ETag: '"profile-7-r4"' } });
      if (pathOf(request).endsWith('/attributes')) return Response.json(attributes());
      if (pathOf(request) === '/api/v1/attribute-definitions') return Response.json({ definitions: [] });
      throw new Error(`unexpected ${request.url}`);
    });
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());
    const patch = { names: { add: [], supersede: [] } };

    await controller.patchProfile(patch);

    expect(controller.conflict).toMatchObject({ code: 'person_revision_conflict' });
    expect(controller.draft).toEqual({ kind: 'profile', body: patch });
    expect(controller.canWriteProfile).toBe(false);
    expect(requests(fetchFn)).toHaveLength(1);

    await controller.reload();
    expect(requests(fetchFn)).toHaveLength(5);
    expect(controller.draft).toBeNull();
    expect(controller.conflict).toBeNull();
    expect(controller.structuredProfileETag).toBe('"profile-7-r4"');
    expect(controller.canWriteProfile).toBe(true);
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
      if (pathOf(request) === '/api/v1/people/7') return new Response(JSON.stringify(person(7, 4)), { headers: { ETag: '"person-7-r4"' } });
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
      if (pathOf(request) === '/api/v1/people/7') return new Response(JSON.stringify(person(7, 4)), { headers: { ETag: '"person-7-r4"' } });
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
      if (pathOf(request) === '/api/v1/people/7') return new Response(JSON.stringify(person(7, 4)), { headers: { ETag: '"person-7-r4"' } });
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
    const fetchFn = vi.fn<typeof fetch>(async () => new Response(JSON.stringify(person(7, 4, 'Alice Example')), { headers: { ETag: '"person-7-r4"' } }));
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
    const fetchFn = vi.fn<typeof fetch>(async () => {
      write += 1;
      return write === 1
        ? new Response(JSON.stringify(person(7, 4, 'Alice Example')), { headers: { ETag: '"person-7-r4"' } })
        : new Response(JSON.stringify(profile(7, 5)), { headers: { ETag: '"person-7-r5"' } });
    });
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    await controller.rename('Alice Example');
    await controller.patchProfile({ names: { add: [], supersede: [] } });

    expect(requests(fetchFn)[1]!.headers.get('If-Match')).toBe('"person-7-r4"');
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
      value_type: 'text' as const, field_type: 'select', cardinality: 'single' as const, display_order: 0,
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
  ] as const)('synthesizes the first %s attribute group from the refreshed definition', async (_name, cardinality) => {
    const createdDefinition = {
      id: 71, universal_id: `created-${cardinality}`, object_type: 'person' as const, slug: `created_${cardinality}`,
      label: `Created ${cardinality}`, value_type: 'text' as const, field_type: 'text', cardinality,
      display_order: 71, is_required: false, ownership: 'user' as const, ui_creatable: true, ui_editable: true,
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
    const controller = new DirectoryProfileController(createAPIClient(vi.fn<typeof fetch>(async () =>
      Response.json({ dry_run: false, value })
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
    const fetchFn = vi.fn<typeof fetch>(async () => new Promise<Response>((resolve) => { resolveFirst = resolve; }));
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    const first = controller.rename('First name');
    await vi.waitFor(() => expect(resolveFirst).toBeDefined());
    void controller.rename('Later name');
    expect(requests(fetchFn)).toHaveLength(1);
    expect(controller.draft).toEqual({ kind: 'rename', body: { display_name: 'Later name' } });
    expect(controller.conflict).toMatchObject({ code: 'mutation_in_progress' });

    resolveFirst?.(new Response(JSON.stringify(person(7, 4, 'First name')), { headers: { ETag: '"person-7-r4"' } }));
    await first;
    expect(controller.draft).toEqual({ kind: 'rename', body: { display_name: 'Later name' } });
    expect(controller.conflict).toMatchObject({ code: 'mutation_in_progress' });
  });

  it('rejects a rapid second profile patch without reusing the stale ETag', async () => {
    let resolveFirst: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async () => new Promise<Response>((resolve) => { resolveFirst = resolve; }));
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
      if (pathOf(request) === '/api/v1/people/7') return new Response(JSON.stringify(person(7, 4)), { headers: { ETag: '"person-7-r4"' } });
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

    resolvePerson?.(new Response(JSON.stringify(person(7, 4)), { headers: { ETag: '"person-7-r4"' } }));
    await expect(reload).resolves.toEqual({ ok: true });
    expect(controller.personETag).toBe('"person-7-r4"');
    expect(controller.canWritePerson).toBe(true);
    expect(controller.canWriteProfile).toBe(true);
  });

  it('restores write capabilities after reload failure leaves the prior ETags intact', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (pathOf(request) === '/api/v1/people/7' || pathOf(request).endsWith('/profile')) {
        return Response.json({ error: 'unavailable', message: 'reload unavailable' }, { status: 503 });
      }
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
      if (pathOf(request) === '/api/v1/people/7') return new Response(JSON.stringify(person(7, 4)), { headers: { ETag: '"person-7-r4"' } });
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

  it('loads and refreshes definitions after creating a user definition', async () => {
    let listed = 0;
    const created = { id: 4, universal_id: 'field-4', slug: 'preferred-channel', label: 'Preferred channel', field_type: 'choice', value_type: 'text', cardinality: 'single', object_type: 'person', ownership: 'user', api_mutable: true, history_exempt: false, display_order: 0, is_active: true, is_audited: false, is_deletable: true, is_required: false, is_searchable: true, is_sensitive: true, ui_creatable: true, ui_editable: true, revision: 1, created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z' };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'POST') return Response.json(created, { status: 201 });
      listed += 1;
      return Response.json({ definitions: listed === 1 ? [] : [created] });
    });
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());

    await controller.reloadDefinitions();
    await controller.createDefinition({ label: 'Preferred channel', field_type: 'choice', value_type: 'text', object_type: 'person', cardinality: 'single', is_sensitive: true });

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
      if (pathOf(request) === '/api/v1/people/7') return new Response(JSON.stringify(person()), { headers: { ETag: '"person-7-r3"' } });
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
    resolvePerson?.(new Response(JSON.stringify(person(7, 4)), { headers: { ETag: '"person-7-r4"' } }));
    await reload;

    expect(controller.personETag).toBe('"person-7-r3"');
    expect(invalidated).toEqual([]);
  });
});
