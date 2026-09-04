import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { DirectoryEntityController } from '../../directory/entity-controller.svelte';
import { chooseSelectOption, openTypeahead } from '../../../test/kit-ui';
import RelationshipsTab from './RelationshipsTab.svelte';

function requestOf(input: RequestInfo | URL): Request {
  return input instanceof Request ? input : new Request(input);
}

function pathOf(request: Request): string {
  return new URL(request.url, document.baseURI).pathname;
}

function relationship(id = 41, revision = 1, overrides: Record<string, unknown> = {}) {
  return {
    id, revision, relationship_type_id: 31, type_slug: 'mentor', source_person_id: 7, target_person_id: 8,
    forward_label: 'mentors', reverse_label: 'is mentored by', is_symmetric: false, status: 'active', source: 'user',
    created_by: 'user', updated_by: 'user', vcard_identity: {},
    created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', ...overrides
  };
}

function view(id: number, direction: 'incoming' | 'outgoing', label: string, name: string, overrides: Record<string, unknown> = {}) {
  return {
    counterpart_person_id: id + 100,
    counterpart_display_name: name,
    counterpart_label: label,
    counterpart_vcard_uid: `person-${id + 100}`,
    direction,
    relationship: relationship(id, 1, direction === 'incoming'
      ? { source_person_id: id + 100, target_person_id: 7 }
      : { source_person_id: 7, target_person_id: id + 100 }),
    ...overrides
  };
}

function relationshipType(overrides: Record<string, unknown> = {}) {
  return {
    id: 31, revision: 1, slug: 'mentor', forward_label: 'mentors', reverse_label: 'is mentored by',
    is_symmetric: false, is_canonical: false, is_deletable: true, ownership: 'user', universal_id: 'relationship-type-31',
    created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', ...overrides
  };
}

function controllerWith(fetchFn: typeof fetch): { client: ReturnType<typeof createAPIClient>; controller: DirectoryEntityController } {
  const client = createAPIClient(fetchFn);
  return { client, controller: new DirectoryEntityController(client, 7) };
}

describe('RelationshipsTab', () => {
  it('renders returned directional labels once and keeps system relationship types read-only', () => {
    const { client, controller } = controllerWith(vi.fn());
    controller.relationships = [
      view(41, 'outgoing', 'mentors', 'Synthetic Learner'),
      view(42, 'incoming', 'is mentored by', 'Synthetic Guide'),
      view(43, 'outgoing', 'works with', 'Synthetic Peer', {
        relationship: relationship(43, 1, { is_symmetric: true, forward_label: 'works with', reverse_label: 'works with' })
      })
    ];
    controller.relationshipTypes = [
      relationshipType({ ownership: 'system', is_deletable: false }),
      relationshipType({ id: 32, slug: 'custom', forward_label: 'supports', reverse_label: 'is supported by' })
    ];

    render(RelationshipsTab, { client, controller, personID: 7 });

    expect(screen.getByText('Synthetic Learner · mentors')).toBeDefined();
    expect(screen.getByText('Synthetic Guide · is mentored by')).toBeDefined();
    expect(screen.getByText('Synthetic Peer · works with')).toBeDefined();
    expect(screen.getAllByRole('button', { name: 'Edit relationship' })).toHaveLength(3);
    expect(screen.getAllByRole('button', { name: 'Delete relationship' })).toHaveLength(3);
    expect(screen.queryByRole('button', { name: 'Edit mentor relationship type' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Delete mentor relationship type' })).toBeNull();
    expect(screen.getByText('Built in')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Edit custom relationship type' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Delete custom relationship type' })).toBeDefined();
  });

  it('preserves the include-ended choice across explicit refreshes', async () => {
    const includeEnded: string[] = [];
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (pathOf(request) === '/api/v1/people/7/relationships') {
        includeEnded.push(new URL(request.url).searchParams.get('include_ended') ?? '');
        return Response.json({ relationships: [] });
      }
      throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
    }));
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('checkbox', { name: 'Include ended relationships' }));
    await waitFor(() => expect(includeEnded).toEqual(['true']));
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh relationships' }));

    await waitFor(() => expect(includeEnded).toEqual(['true', 'true']));
  });

  it('renders relationship loading, failure with retry, and empty states from real collection requests', async () => {
    let resolveFirst!: (response: Response) => void;
    let reads = 0;
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (pathOf(request) !== '/api/v1/people/7/relationships') throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
      reads += 1;
      if (reads === 1) return new Promise<Response>((resolve) => { resolveFirst = resolve; });
      return Response.json({ relationships: [] });
    }));
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Refresh relationships' }));
    expect(screen.getByRole('status').textContent).toContain('Loading relationships');
    resolveFirst(Response.json({ error: 'unavailable', message: 'relationships unavailable' }, { status: 503 }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('relationships unavailable');
    await fireEvent.click(within(alert).getByRole('button', { name: 'Refresh relationships' }));

    expect(await screen.findByText('No relationships')).toBeDefined();
    expect(reads).toBe(2);
  });

  it('recovers a relationship-type collection error with an explicit matching refresh', async () => {
    let reads = 0;
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (pathOf(request) === '/api/v1/relationship-types') {
        reads += 1;
        return Response.json({ relationship_types: [relationshipType()] });
      }
      throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
    }));
    controller.errors.relationshipTypes = 'relationship types unavailable';
    render(RelationshipsTab, { client, controller, personID: 7 });

    expect(screen.getByRole('alert').textContent).toContain('relationship types unavailable');
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh relationship types' }));

    await waitFor(() => expect(controller.relationshipTypes).toHaveLength(1));
    expect(reads).toBe(1);
    expect(controller.errors.relationshipTypes).toBeUndefined();
  });

  it('searches only the bounded durable Directory, rejects self, and creates an incoming edge with optional fields', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const requests: Request[] = [];
      const created = relationship(44, 1, { source_person_id: 8, target_person_id: 7, start_date: { year: 2025 }, end_date: { year: 2026, month: 8 }, notes: 'Synthetic note' });
      const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
        const request = requestOf(input);
        requests.push(request);
        const path = pathOf(request);
        if (path === '/api/v1/people/directory') return Response.json({ people: [
          { id: 7, revision: 1, display_name: 'Selected Self', categories: [], contact_state: 'active', organizations: [] },
          { id: 8, revision: 1, display_name: 'Synthetic Counterpart', categories: [], contact_state: 'active', organizations: [] }
        ] });
        if (path === '/api/v1/person-relationships' && request.method === 'POST') {
          return new Response(JSON.stringify(created), { status: 201, headers: { ETag: '"relationship-44-r1"' } });
        }
        if (path === '/api/v1/people/7/relationships') return Response.json({ relationships: [view(44, 'incoming', 'is mentored by', 'Synthetic Counterpart', { relationship: created })] });
        throw new Error(`unexpected ${request.method} ${path}`);
      }));
      controller.relationshipTypes = [relationshipType()];
      render(RelationshipsTab, { client, controller, personID: 7 });

      await fireEvent.click(screen.getByRole('button', { name: 'Add relationship' }));
      const personSearch = await openTypeahead('Relationship counterpart');
      await fireEvent.input(personSearch, { target: { value: 'Synthetic' } });
      await vi.advanceTimersByTimeAsync(250);
      expect(await screen.findByRole('option', { name: /Synthetic Counterpart/ })).toBeDefined();
      expect(screen.queryByText('Selected Self')).toBeNull();
      await fireEvent.mouseDown(screen.getByRole('option', { name: /Synthetic Counterpart/ }));
      await chooseSelectOption(screen.getByRole('combobox', { name: /^Relationship direction:/ }), 'Counterpart → selected person');
      await chooseSelectOption(screen.getByRole('combobox', { name: /^Relationship type:/ }), 'mentors / is mentored by');
      await fireEvent.input(screen.getByRole('textbox', { name: 'Relationship start date' }), { target: { value: '2025' } });
      await fireEvent.input(screen.getByRole('textbox', { name: 'Relationship end date' }), { target: { value: '2026-08' } });
      await fireEvent.input(screen.getByRole('textbox', { name: 'Relationship notes' }), { target: { value: 'Synthetic note' } });
      await fireEvent.click(screen.getByRole('button', { name: 'Create relationship' }));

      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Add relationship' })).toBeNull());
      const search = requests.find((request) => pathOf(request) === '/api/v1/people/directory')!;
      expect(new URL(search.url).searchParams.get('q')).toBe('Synthetic');
      expect(new URL(search.url).searchParams.get('limit')).toBe('20');
      const post = requests.find((request) => pathOf(request) === '/api/v1/person-relationships')!;
      await expect(post.clone().json()).resolves.toEqual({
        source_person_id: 8,
        target_person_id: 7,
        relationship_type_slug: 'mentor',
        start_date: '2025',
        end_date: '2026-08',
        notes: 'Synthetic note'
      });
      expect((await post.clone().json()).source).toBeUndefined();
      expect(screen.getByText('Synthetic Counterpart · is mentored by')).toBeDefined();
    } finally {
      vi.useRealTimers();
    }
  });

  it('creates an outgoing edge at the collection endpoint with the selected person as source', async () => {
    const requests: Request[] = [];
    const created = relationship(45, 1, { source_person_id: 7, target_person_id: 9 });
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') return Response.json({ people: [
        { id: 9, revision: 1, display_name: 'Synthetic Colleague', categories: [], contact_state: 'active', organizations: [] }
      ] });
      if (path === '/api/v1/person-relationships' && request.method === 'POST') {
        return new Response(JSON.stringify(created), { status: 201, headers: { ETag: '"relationship-45-r1"' } });
      }
      if (path === '/api/v1/people/7/relationships') {
        return Response.json({ relationships: [view(45, 'outgoing', 'mentors', 'Synthetic Colleague', { relationship: created })] });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.relationshipTypes = [relationshipType()];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Add relationship' }));
    await fireEvent.input(await openTypeahead('Relationship counterpart'), { target: { value: 'Colleague' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Synthetic Colleague/ }));
    await chooseSelectOption(screen.getByRole('combobox', { name: /^Relationship direction:/ }), 'Selected person → counterpart');
    await chooseSelectOption(screen.getByRole('combobox', { name: /^Relationship type:/ }), 'mentors / is mentored by');
    await fireEvent.click(screen.getByRole('button', { name: 'Create relationship' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Add relationship' })).toBeNull());
    const post = requests.find((request) => pathOf(request) === '/api/v1/person-relationships' && request.method === 'POST')!;
    await expect(post.clone().json()).resolves.toEqual({
      source_person_id: 7,
      target_person_id: 9,
      relationship_type_slug: 'mentor'
    });
    expect(screen.getByText('Synthetic Colleague · mentors')).toBeDefined();
  });

  it('retains an edge draft on conflict and uses the fresh individual ETag without auto-retry', async () => {
    const requests: Request[] = [];
    let reads = 0;
    let writes = 0;
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/person-relationships/41' && request.method === 'GET') {
        reads += 1;
        return new Response(JSON.stringify(relationship(41, reads, { notes: reads === 1 ? 'Server old' : 'Server concurrent' })), { headers: { ETag: `"relationship-41-r${reads}"` } });
      }
      if (path === '/api/v1/person-relationships/41' && request.method === 'PATCH') {
        writes += 1;
        return Response.json({ error: 'revision_conflict', message: 'edge changed elsewhere' }, { status: 412 });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.relationships = [view(41, 'outgoing', 'mentors', 'Synthetic Learner')];
    controller.relationshipTypes = [relationshipType()];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Edit relationship' }));
    const notes = await screen.findByRole('textbox', { name: 'Relationship notes' });
    await fireEvent.input(notes, { target: { value: 'Retained local draft' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save relationship' }));

    expect((await screen.findByRole('alert')).textContent).toContain('changed elsewhere');
    expect((screen.getByRole('textbox', { name: 'Relationship notes' }) as HTMLInputElement).value).toBe('Retained local draft');
    expect(screen.getByText(/Server concurrent/)).toBeDefined();
    expect(writes).toBe(1);
    expect(reads).toBe(2);
    const patch = requests.find((request) => request.method === 'PATCH')!;
    expect(patch.headers.get('If-Match')).toBe('"relationship-41-r1"');
  });

  it('patches only a changed end date so a fresh server note is not overwritten', async () => {
    const requests: Request[] = [];
    const opening = relationship(41, 1, { notes: 'Opening note' });
    const fresh = relationship(41, 2, { notes: 'Concurrent note' });
    const updated = relationship(41, 3, { notes: 'Concurrent note', end_date: { year: 2026, month: 8 } });
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/person-relationships/41' && request.method === 'GET') {
        return new Response(JSON.stringify(fresh), { headers: { ETag: '"relationship-41-r2"' } });
      }
      if (path === '/api/v1/person-relationships/41' && request.method === 'PATCH') {
        return new Response(JSON.stringify(updated), { headers: { ETag: '"relationship-41-r3"' } });
      }
      if (path === '/api/v1/people/7/relationships') {
        return Response.json({ relationships: [view(41, 'outgoing', 'mentors', 'Synthetic Learner', { relationship: updated })] });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.relationships = [view(41, 'outgoing', 'mentors', 'Synthetic Learner', { relationship: opening })];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Edit relationship' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Relationship end date' }), { target: { value: '2026-08' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save relationship' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Edit relationship' })).toBeNull());
    const patch = requests.find((request) => request.method === 'PATCH')!;
    expect(patch.headers.get('If-Match')).toBe('"relationship-41-r2"');
    await expect(patch.clone().json()).resolves.toEqual({ end_date: '2026-08' });
  });

  it('clears changed notes without resending an opening end date', async () => {
    const requests: Request[] = [];
    const opening = relationship(41, 1, { notes: 'Opening note', end_date: { year: 2026, month: 6 } });
    const fresh = relationship(41, 2, { notes: 'Opening note', end_date: { year: 2026, month: 7 } });
    const updated = relationship(41, 3, { end_date: { year: 2026, month: 7 } });
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/person-relationships/41' && request.method === 'GET') {
        return new Response(JSON.stringify(fresh), { headers: { ETag: '"relationship-41-r2"' } });
      }
      if (path === '/api/v1/person-relationships/41' && request.method === 'PATCH') {
        return new Response(JSON.stringify(updated), { headers: { ETag: '"relationship-41-r3"' } });
      }
      if (path === '/api/v1/people/7/relationships') {
        return Response.json({ relationships: [view(41, 'outgoing', 'mentors', 'Synthetic Learner', { relationship: updated })] });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.relationships = [view(41, 'outgoing', 'mentors', 'Synthetic Learner', { relationship: opening })];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Edit relationship' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Relationship notes' }), { target: { value: '   ' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save relationship' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Edit relationship' })).toBeNull());
    const patch = requests.find((request) => request.method === 'PATCH')!;
    await expect(patch.clone().json()).resolves.toEqual({ notes: null });
  });

  it('closes an unchanged relationship edit without sending an empty patch', async () => {
    const fetchFn = vi.fn<typeof fetch>();
    const { client, controller } = controllerWith(fetchFn);
    controller.relationships = [view(41, 'outgoing', 'mentors', 'Synthetic Learner', {
      relationship: relationship(41, 1, { notes: 'Opening note', end_date: { year: 2026, month: 6 } })
    })];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Edit relationship' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Save relationship' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Edit relationship' })).toBeNull());
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it('deletes the single underlying edge with its fresh ETag', async () => {
    const requests: Request[] = [];
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/person-relationships/41' && request.method === 'GET') {
        return new Response(JSON.stringify(relationship(41, 3)), { headers: { ETag: '"relationship-41-r3"' } });
      }
      if (path === '/api/v1/person-relationships/41' && request.method === 'DELETE') return new Response(null, { status: 204 });
      if (path === '/api/v1/people/7/relationships') return Response.json({ relationships: [] });
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.relationships = [view(41, 'incoming', 'is mentored by', 'Synthetic Guide')];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Delete relationship' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete relationship' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Delete relationship' })).toBeNull());
    const deletion = requests.find((request) => request.method === 'DELETE')!;
    expect(pathOf(deletion)).toBe('/api/v1/person-relationships/41');
    expect(deletion.headers.get('If-Match')).toBe('"relationship-41-r3"');
    expect(controller.relationships).toEqual([]);
  });

  it('blocks an uncertain edge create until the exact collection refresh and retains the dialog draft', async () => {
    let posts = 0;
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') return Response.json({ people: [
        { id: 8, revision: 1, display_name: 'Synthetic Counterpart', categories: [], contact_state: 'active', organizations: [] }
      ] });
      if (path === '/api/v1/person-relationships' && request.method === 'POST') {
        posts += 1;
        return Response.json({ error: 'unavailable', message: 'outcome unknown' }, { status: 503 });
      }
      if (path === '/api/v1/people/7/relationships') return Response.json({ relationships: [] });
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.relationshipTypes = [relationshipType()];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Add relationship' }));
    await fireEvent.input(await openTypeahead('Relationship counterpart'), { target: { value: 'Counterpart' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Synthetic Counterpart/ }));
    await chooseSelectOption(screen.getByRole('combobox', { name: /^Relationship type:/ }), 'mentors / is mentored by');
    await fireEvent.input(screen.getByRole('textbox', { name: 'Relationship notes' }), { target: { value: 'Retained unknown draft' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create relationship' }));

    expect((await screen.findByRole('alert')).textContent).toContain('outcome is unknown');
    expect((screen.getByRole('textbox', { name: 'Relationship notes' }) as HTMLInputElement).value).toBe('Retained unknown draft');
    expect(screen.getByRole('button', { name: 'Create relationship' })).toHaveProperty('disabled', true);
    await fireEvent.click(within(screen.getByRole('dialog', { name: 'Add relationship' })).getByRole('button', { name: 'Refresh relationships' }));
    await waitFor(() => expect(controller.createBlocked.relationships).toBe(false));
    expect(posts).toBe(1);
  });

  it('creates a user relationship type with required and supplied optional fields', async () => {
    const requests: Request[] = [];
    const created = relationshipType({ id: 33, slug: 'peer', forward_label: 'works with', reverse_label: 'works with', is_symmetric: true, color: '#123456', description: 'Synthetic type' });
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (pathOf(request) === '/api/v1/relationship-types' && request.method === 'POST') {
        return new Response(JSON.stringify(created), { status: 201, headers: { ETag: '"relationship-type-33-r1"' } });
      }
      throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
    }));
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Add relationship type' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Relationship type slug' }), { target: { value: 'peer' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Relationship type forward label' }), { target: { value: 'works with' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Relationship type reverse label' }), { target: { value: 'works with' } });
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Symmetric relationship' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Relationship type color' }), { target: { value: '#123456' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Relationship type description' }), { target: { value: 'Synthetic type' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create relationship type' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Add relationship type' })).toBeNull());
    const post = requests.find((request) => request.method === 'POST')!;
    await expect(post.clone().json()).resolves.toEqual({
      slug: 'peer',
      forward_label: 'works with',
      reverse_label: 'works with',
      is_symmetric: true,
      color: '#123456',
      description: 'Synthetic type'
    });
    expect(controller.relationshipTypes).toContainEqual(created);
  });

  it('blocks a symmetric relationship type with mismatched trimmed labels without making a request', async () => {
    const fetchFn = vi.fn<typeof fetch>();
    const { client, controller } = controllerWith(fetchFn);
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Add relationship type' }));
    const dialog = screen.getByRole('dialog', { name: 'Add relationship type' });
    await fireEvent.input(within(dialog).getByRole('textbox', { name: 'Relationship type slug' }), { target: { value: 'peer' } });
    await fireEvent.input(within(dialog).getByRole('textbox', { name: 'Relationship type forward label' }), { target: { value: 'works with' } });
    await fireEvent.input(within(dialog).getByRole('textbox', { name: 'Relationship type reverse label' }), { target: { value: 'collaborates with' } });
    await fireEvent.click(within(dialog).getByRole('checkbox', { name: 'Symmetric relationship' }));

    expect(within(dialog).getByText('Symmetric relationship labels must match.')).toBeDefined();
    const create = within(dialog).getByRole('button', { name: 'Create relationship type' });
    expect(create).toHaveProperty('disabled', true);
    await fireEvent.click(create);
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it('blocks an existing symmetric relationship type when only one trimmed label changes', async () => {
    const fetchFn = vi.fn<typeof fetch>();
    const symmetricType = relationshipType({
      slug: 'peer', forward_label: 'works with', reverse_label: 'works with', is_symmetric: true
    });
    const { client, controller } = controllerWith(fetchFn);
    controller.relationshipTypes = [symmetricType];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Edit peer relationship type' }));
    const dialog = screen.getByRole('dialog', { name: 'Edit peer relationship type' });
    await fireEvent.input(within(dialog).getByRole('textbox', { name: 'Relationship type forward label' }), { target: { value: 'collaborates with' } });

    expect(within(dialog).getByText('Symmetric relationship labels must match.')).toBeDefined();
    const save = within(dialog).getByRole('button', { name: 'Save relationship type' });
    expect(save).toHaveProperty('disabled', true);
    await fireEvent.click(save);
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it('patches both matched labels for an existing symmetric type with its fresh ETag', async () => {
    const requests: Request[] = [];
    const current = relationshipType({
      slug: 'peer', forward_label: 'works with', reverse_label: 'works with', is_symmetric: true
    });
    const updated = relationshipType({
      slug: 'peer', revision: 2, forward_label: 'collaborates with', reverse_label: 'collaborates with', is_symmetric: true
    });
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/relationship-types/31' && request.method === 'GET') {
        return new Response(JSON.stringify(current), { headers: { ETag: '"relationship-type-31-r1"' } });
      }
      if (path === '/api/v1/relationship-types/31' && request.method === 'PATCH') {
        return new Response(JSON.stringify(updated), { headers: { ETag: '"relationship-type-31-r2"' } });
      }
      if (path === '/api/v1/people/7/relationships') return Response.json({ relationships: [] });
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.relationshipTypes = [current];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Edit peer relationship type' }));
    const dialog = screen.getByRole('dialog', { name: 'Edit peer relationship type' });
    await fireEvent.input(within(dialog).getByRole('textbox', { name: 'Relationship type forward label' }), { target: { value: ' collaborates with ' } });
    await fireEvent.input(within(dialog).getByRole('textbox', { name: 'Relationship type reverse label' }), { target: { value: 'collaborates with' } });
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Save relationship type' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Edit peer relationship type' })).toBeNull());
    const get = requests.find((request) => request.method === 'GET' && pathOf(request) === '/api/v1/relationship-types/31')!;
    const patch = requests.find((request) => request.method === 'PATCH')!;
    expect(get).toBeDefined();
    expect(patch.headers.get('If-Match')).toBe('"relationship-type-31-r1"');
    await expect(patch.clone().json()).resolves.toEqual({
      forward_label: 'collaborates with',
      reverse_label: 'collaborates with'
    });
  });

  it('retains an uncertain relationship-type draft and closes only after an exact collection refresh', async () => {
    let posts = 0;
    let reads = 0;
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/relationship-types' && request.method === 'POST') {
        posts += 1;
        return Response.json({ error: 'unavailable', message: 'outcome unknown' }, { status: 503 });
      }
      if (path === '/api/v1/relationship-types' && request.method === 'GET') {
        reads += 1;
        return Response.json({ relationship_types: [] });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Add relationship type' }));
    const dialog = screen.getByRole('dialog', { name: 'Add relationship type' });
    await fireEvent.input(within(dialog).getByRole('textbox', { name: 'Relationship type slug' }), { target: { value: 'advisor' } });
    await fireEvent.input(within(dialog).getByRole('textbox', { name: 'Relationship type forward label' }), { target: { value: 'advises' } });
    await fireEvent.input(within(dialog).getByRole('textbox', { name: 'Relationship type reverse label' }), { target: { value: 'is advised by' } });
    await fireEvent.input(within(dialog).getByRole('textbox', { name: 'Relationship type description' }), { target: { value: 'Retained type draft' } });
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Create relationship type' }));

    expect((await within(dialog).findByRole('alert')).textContent).toContain('outcome is unknown');
    expect((within(dialog).getByRole('textbox', { name: 'Relationship type description' }) as HTMLInputElement).value).toBe('Retained type draft');
    expect(within(dialog).getByRole('button', { name: 'Create relationship type' })).toHaveProperty('disabled', true);
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Create relationship type' }));
    expect(posts).toBe(1);
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Refresh relationship types' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Add relationship type' })).toBeNull());
    expect(posts).toBe(1);
    expect(reads).toBe(1);
  });

  it('edits a user type with a fresh ETag, refreshes directional labels, and never patches symmetry', async () => {
    const requests: Request[] = [];
    const current = relationshipType();
    const updated = relationshipType({ revision: 2, forward_label: 'guides' });
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/relationship-types/31' && request.method === 'GET') {
        return new Response(JSON.stringify(current), { headers: { ETag: '"relationship-type-31-r1"' } });
      }
      if (path === '/api/v1/relationship-types/31' && request.method === 'PATCH') {
        return new Response(JSON.stringify(updated), { headers: { ETag: '"relationship-type-31-r2"' } });
      }
      if (path === '/api/v1/people/7/relationships') return Response.json({ relationships: [view(41, 'outgoing', 'guides', 'Synthetic Learner')] });
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.relationshipTypes = [current];
    controller.relationships = [view(41, 'outgoing', 'mentors', 'Synthetic Learner')];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Edit mentor relationship type' }));
    expect(await screen.findByText('Symmetry cannot be changed after creation.')).toBeDefined();
    expect(screen.getByText('Directional')).toBeDefined();
    await fireEvent.input(screen.getByRole('textbox', { name: 'Relationship type forward label' }), { target: { value: 'guides' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save relationship type' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Edit mentor relationship type' })).toBeNull());
    const patch = requests.find((request) => request.method === 'PATCH')!;
    expect(patch.headers.get('If-Match')).toBe('"relationship-type-31-r1"');
    const body = await patch.clone().json();
    expect(body).toEqual({ forward_label: 'guides' });
    expect(screen.getByText('Synthetic Learner · guides')).toBeDefined();
  });

  it('clears one optional relationship-type field without resending unchanged metadata', async () => {
    const requests: Request[] = [];
    const current = relationshipType({
      vcard_related_type: 'friend', color: '#123456', icon: 'people', description: 'Existing metadata'
    });
    const updated = relationshipType({ revision: 2 });
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/relationship-types/31' && request.method === 'GET') {
        return new Response(JSON.stringify(current), { headers: { ETag: '"relationship-type-31-r1"' } });
      }
      if (path === '/api/v1/relationship-types/31' && request.method === 'PATCH') {
        return new Response(JSON.stringify(updated), { headers: { ETag: '"relationship-type-31-r2"' } });
      }
      if (path === '/api/v1/people/7/relationships') return Response.json({ relationships: [] });
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.relationshipTypes = [current];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Edit mentor relationship type' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Relationship type color' }), { target: { value: '   ' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save relationship type' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Edit mentor relationship type' })).toBeNull());
    const patch = requests.find((request) => request.method === 'PATCH')!;
    await expect(patch.clone().json()).resolves.toEqual({ color: '' });
  });

  it('closes an unchanged relationship-type edit without sending an empty patch', async () => {
    const fetchFn = vi.fn<typeof fetch>();
    const current = relationshipType({ vcard_related_type: 'friend', color: '#123456', icon: 'people', description: 'Existing metadata' });
    const { client, controller } = controllerWith(fetchFn);
    controller.relationshipTypes = [current];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Edit mentor relationship type' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Save relationship type' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Edit mentor relationship type' })).toBeNull());
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it('shows the complete refreshed relationship type after conflict, retains the draft, and waits for an explicit retry', async () => {
    let reads = 0;
    let writes = 0;
    const initial = relationshipType({
      vcard_related_type: 'friend', color: '#111111', icon: 'old-icon', description: 'Old description'
    });
    const concurrent = relationshipType({
      revision: 2,
      forward_label: 'Current forward',
      reverse_label: 'Current reverse',
      vcard_related_type: '',
      color: '#222222',
      icon: 'current-icon',
      description: 'Current description',
      is_symmetric: true,
      ownership: 'user'
    });
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/relationship-types/31' && request.method === 'GET') {
        reads += 1;
        const entity = reads % 2 === 1 ? initial : concurrent;
        return new Response(JSON.stringify(entity), { headers: { ETag: `"relationship-type-31-r${entity.revision}"` } });
      }
      if (path === '/api/v1/relationship-types/31' && request.method === 'PATCH') {
        writes += 1;
        return Response.json({ error: 'revision_conflict', message: 'type changed elsewhere' }, { status: 412 });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.relationshipTypes = [initial];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Edit mentor relationship type' }));
    const dialog = screen.getByRole('dialog', { name: 'Edit mentor relationship type' });
    await fireEvent.input(within(dialog).getByRole('textbox', { name: 'Relationship type forward label' }), { target: { value: 'Retained draft' } });
    await fireEvent.input(within(dialog).getByRole('textbox', { name: 'Relationship type color' }), { target: { value: '#draft' } });
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Save relationship type' }));

    const alert = await within(dialog).findByRole('alert');
    expect(alert.textContent).toContain('changed elsewhere');
    expect(alert.textContent).toContain('Current forward');
    expect(alert.textContent).toContain('Current reverse');
    expect(alert.textContent).toContain('vCard RELATED typeNot set');
    expect(alert.textContent).toContain('#222222');
    expect(alert.textContent).toContain('current-icon');
    expect(alert.textContent).toContain('Current description');
    expect(alert.textContent).toContain('SymmetrySymmetric');
    expect(alert.textContent).toContain('OwnershipCustom');
    expect((within(dialog).getByRole('textbox', { name: 'Relationship type forward label' }) as HTMLInputElement).value).toBe('Retained draft');
    expect((within(dialog).getByRole('textbox', { name: 'Relationship type color' }) as HTMLInputElement).value).toBe('#draft');
    expect(writes).toBe(1);
    expect(reads).toBe(2);

    await fireEvent.click(within(dialog).getByRole('button', { name: 'Save relationship type' }));
    await waitFor(() => expect(writes).toBe(2));
    expect(reads).toBe(4);
  });

  it('exposes immutable symmetry and obeys the user-type deletable guard', async () => {
    const { client, controller } = controllerWith(vi.fn());
    controller.relationshipTypes = [
      relationshipType(),
      relationshipType({ id: 32, slug: 'locked', ownership: 'user', is_deletable: false, is_symmetric: true })
    ];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Edit locked relationship type' }));

    expect(await screen.findByText('Symmetry cannot be changed after creation.')).toBeDefined();
    expect(screen.getByText('Symmetric')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Delete relationship type' })).toBeNull();
  });

  it('deletes a deletable user type with its fresh ETag', async () => {
    const requests: Request[] = [];
    const current = relationshipType();
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/relationship-types/31' && request.method === 'GET') {
        return new Response(JSON.stringify(current), { headers: { ETag: '"relationship-type-31-r1"' } });
      }
      if (path === '/api/v1/relationship-types/31' && request.method === 'DELETE') return new Response(null, { status: 204 });
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.relationshipTypes = [current];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Delete mentor relationship type' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Delete relationship type' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Delete relationship type' })).toBeNull());
    const deletion = requests.find((request) => request.method === 'DELETE')!;
    expect(deletion.headers.get('If-Match')).toBe('"relationship-type-31-r1"');
    expect(controller.relationshipTypes).toEqual([]);
  });
});
