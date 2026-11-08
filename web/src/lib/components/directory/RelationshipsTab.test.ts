import { controllerWith as entityControllerWith, directoryPerson, relationshipType, relationship, requestOf, pathOf } from '../../directory/entity-fixtures.test-support';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { DirectoryEntityController } from '../../directory/entity-controller.svelte';
import { openTypeahead } from '../../../test/kit-ui';
import RelationshipsTab from './RelationshipsTab.svelte';

function controllerWith(fetchFn: typeof fetch, personID = 7) {
  const result = entityControllerWith(async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    if (request.method === 'GET' && pathOf(request) === '/api/v1/person-relationship-reviews') return Response.json({ reviews: [] });
    return fetchFn(input, init);
  }, personID);
  return result;
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

async function rowAction(label: string): Promise<void> {
  await fireEvent.click(screen.getAllByRole('button', { name: /^Actions for /  })[0]);
  await fireEvent.click(await screen.findByRole('menuitem', { name: label }));
}

describe('RelationshipsTab', () => {
  it('freezes the person picker while creating a connection', async () => {
    let finish!: (response: Response) => void;
    const pending = new Promise<Response>(resolve => { finish = resolve; });
    const { client, controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (request.method === 'POST') return pending;
      if (pathOf(request) === '/api/v1/people/directory') return Response.json({ people: [directoryPerson(8)] });
      return Response.json({ relationships: [] });
    });
    controller.relationshipTypes = [relationshipType()];
    const rendered = render(RelationshipsTab, { client, controller, personID: 7 });
    try {
      await fireEvent.click(screen.getByRole('button', { name: 'Add connection…' }));
      await openTypeahead('Connection type');
      await fireEvent.mouseDown(screen.getByRole('treeitem', { name: 'Mentors' }));
      await openTypeahead('Person');
      await fireEvent.mouseDown(await screen.findByRole('option', { name: 'Synthetic Two' }));
      await fireEvent.click(screen.getByRole('button', { name: 'Add connection' }));
      await waitFor(() => expect(screen.getByRole('textbox', { name: 'Notes' })).toHaveProperty('disabled', true));
      expect(screen.getByRole('button', { name: 'Person: Synthetic Two' })).toHaveProperty('disabled', true);
    } finally {
      finish(Response.json(relationship()));
      rendered.unmount();
      controller.destroy();
    }
  });
  it('selects the first type option with Enter after filtering', async () => {
    const { client, controller } = controllerWith(async () => Response.json({}));
    controller.relationshipTypes = [relationshipType({ slug: 'parent', forward_label: 'parent', reverse_label: 'child' })];
    render(RelationshipsTab, { client, controller, personID: 7 });
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);
    const input = await openTypeahead('Connection type');
    await fireEvent.input(input, { target: { value: 'child' } });
    expect(document.getElementById(input.getAttribute('aria-activedescendant')!)?.textContent?.trim()).toBe('Child');
    await fireEvent.keyDown(input, { key: 'Enter' });
    expect(screen.getByRole('button', { name: 'Connection type: is a child of' })).toBeDefined();
  });

  it('shows a type failure with retry, then query-specific empty copy', async () => {
    let reads = 0;
    const { client, controller } = controllerWith(async () => ++reads === 1
      ? Response.json({ message: 'Types unavailable' }, { status: 503 })
      : Response.json({ relationship_types: [relationshipType()] }));
    render(RelationshipsTab, { client, controller, personID: 7 });
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);
    await controller.refreshRelationshipTypes();
    await openTypeahead('Connection type');
    expect(screen.getByRole('alert').textContent).toContain("Couldn't load connection types.");
    expect(screen.queryByText('No matches')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByRole('treeitem', { name: 'Mentors' });
    await fireEvent.input(screen.getByRole('combobox', { name: 'Connection type' }), { target: { value: 'missing' } });
    expect(screen.getByText('No type matches “missing”')).toBeDefined();
  });

  it('retries a failed person search inside the picker', async () => {
    let reads = 0;
    const { client, controller } = controllerWith(async input => {
      const path = pathOf(requestOf(input));
      if (path !== '/api/v1/people/directory') throw new Error(`Unexpected ${path}`);
      if (++reads === 1) return Response.json({message:'Unavailable'}, {status:503});
      return Response.json({people:[directoryPerson(9)]});
    });
    render(RelationshipsTab, {client, controller, personID:7});
    await fireEvent.click(screen.getByRole('button', {name:'Add connection…'}));
    await openTypeahead('Person');
    await screen.findByText("Couldn't search people.");
    await fireEvent.click(screen.getByRole('button', {name:'Retry'}));
    await fireEvent.mouseDown(await screen.findByRole('option', {name:'Synthetic Two'}));
    expect(screen.getByRole('button', {name:'Person: Synthetic Two'})).toBeDefined();
    expect(reads).toBe(2);
  });

  it('distinguishes saved people who share a name without organizations', async () => {
    const { client, controller } = controllerWith(async () => Response.json({ people: [
      directoryPerson(8, { display_name: 'Synthetic Peer', last_contact_at: '2024-07-01T12:00:00Z' }), directoryPerson(9, { display_name: 'Synthetic Peer', last_contact_at: '2023-02-03T12:00:00Z' }),
    ] }));
    render(RelationshipsTab, { client, controller, personID: 7 });
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);
    const search = await openTypeahead('Person');
    expect(search.placeholder).toBe('Person');
    expect(await screen.findByRole('option', { name: 'Synthetic Peer Last contact Jul 1, 2024' })).toBeDefined();
    expect(screen.getByRole('option', { name: 'Synthetic Peer Last contact Feb 3, 2023' })).toBeDefined();
  });

  it('uses a contact title and muted identifier for an unresolved imported UID', async () => {
    const client = createAPIClient(async () => Response.json({ reviews: [{
      id: 1, person_id: 7, raw_related_value: 'urn:uuid:11111111-2222-3333-4444-555555555555',
      raw_related_type: 'friend', value_kind: 'uri', status: 'pending', source: 'carddav_import',
      created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z',
    }] }));
    const controller = new DirectoryEntityController(client, 7);
    render(RelationshipsTab, { client, controller, personID: 7 });
    const title = await screen.findByText('Unknown contact');
    // The reference leads the secondary line, then the type and where it came from.
    const row = title.closest('.detail-row') as HTMLElement;
    expect(row.querySelector('.description')?.textContent).toBe('urn:uuid:11111111-2222-3333-4444-555555555555 · Friend · from CardDAV contacts · Jul 31');
  });

  it('shows saved recovery once in the editor and restores it to the page on close', async () => {
    let saved = false;
    const { client, controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (pathOf(request) === '/api/v1/people/directory') return Response.json({ people: [directoryPerson(8)] });
      if (request.method === 'POST') { saved = true; return Response.json(relationship()); }
      return saved ? Response.json({ message: 'Refresh unavailable' }, { status: 503 }) : Response.json({ relationships: [] });
    });
    controller.relationshipTypes = [relationshipType()];
    render(RelationshipsTab, { client, controller, personID: 7 });
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);
    await openTypeahead('Person');
    await fireEvent.mouseDown(await screen.findByRole('option', { name: 'Synthetic Two' }));
    await openTypeahead('Connection type');
    await fireEvent.mouseDown(screen.getByRole('treeitem', { name: 'Mentors' }));
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);
    await waitFor(() => expect(screen.getAllByRole('alert')).toHaveLength(1));
    expect(screen.getByRole('alert').textContent?.trim()).toContain("Connection saved. Couldn't refresh connections.");
    expect(screen.getAllByRole('button', { name: 'Check again' })).toHaveLength(1);
    expect(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!).toHaveProperty('disabled', true);
    await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(screen.getByRole('alert').textContent).toContain("Couldn't load connections.");
  });

  it('clears old options before debounce so Enter cannot select the wrong person', async () => {
    const { client, controller } = controllerWith(async input => {
      const q = new URL(requestOf(input).url).searchParams.get('q');
      return Response.json({ people: [directoryPerson(q ? 9 : 8)] });
    });
    controller.relationshipTypes = [relationshipType()];
    render(RelationshipsTab, { client, controller, personID: 7 });
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);
    await openTypeahead('Connection type');
    await fireEvent.mouseDown(screen.getByRole('treeitem', { name: 'Mentors' }));
    const input = await openTypeahead('Person');
    await screen.findByRole('option', { name: 'Synthetic Two' });
    vi.useFakeTimers();
    try {
      await fireEvent.input(input, { target: { value: 'Synthetic Two Other' } });
      await fireEvent.keyDown(input, { key: 'Enter' });
      expect(screen.queryByRole('option', { name: 'Synthetic Two' })).toBeNull();
      expect(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!).toHaveProperty('disabled', true);
    } finally { vi.useRealTimers(); }
  });

  it('ignores late results from older requests', async () => {
    const responses: ((response: Response) => void)[] = [];
    const { client, controller } = controllerWith(async () => new Promise<Response>(resolve => responses.push(resolve)));
    render(RelationshipsTab, { client, controller, personID: 7 });
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);
    const input = await openTypeahead('Person');
    await waitFor(() => expect(responses).toHaveLength(1));
    await fireEvent.input(input, { target: { value: 'Other' } });
    await fireEvent.input(input, { target: { value: '' } });
    await waitFor(() => expect(responses).toHaveLength(2));
    responses[1](Response.json({ people: [directoryPerson(9, { display_name: 'Current result' })] }));
    await screen.findByRole('option', { name: 'Current result' });
    const stale = Response.json({ people: [directoryPerson(8, { display_name: 'Stale result' })] });
    responses[0](stale);
    await waitFor(() => expect(stale.bodyUsed).toBe(true));
    expect(screen.queryByRole('option', { name: 'Stale result' })).toBeNull();
    expect(screen.getByRole('option', { name: 'Current result' })).toBeDefined();
  });

  it.each([
    ['Mentors', 'mentors', 'mentor', 'mentors', 'is mentored by', 7, 8],
    ['Child', 'is a child of', 'parent', 'parent', 'child', 8, 7],
  ])('creates literal endpoints for %s', async (caption, phrase, slug, forward, reverse, source, target) => {
    let post: Request | undefined;
    const saved = relationship(41, 1, { relationship_type_id: 1, type_slug: slug, source_person_id: source, target_person_id: target, forward_label: forward, reverse_label: reverse });
    const direction = source === 7 ? 'outgoing' : 'incoming';
    const label = direction === 'outgoing' ? forward : reverse;
    const { client, controller } = controllerWith(async input => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/person-relationships' && request.method === 'POST') { post = request; return Response.json(saved); }
      if (path === '/api/v1/people/directory') return Response.json({ people: [directoryPerson(8)] });
      if (path === '/api/v1/people/7/relationships') return Response.json({ relationships: post ? [view(41, direction, label, 'Synthetic Two', { counterpart_person_id: 8, relationship: saved })] : [] });
      throw new Error(`unexpected ${request.method} ${path}`);
    });
    controller.relationshipTypes = [relationshipType({ id: 1, slug: slug, forward_label: forward, reverse_label: reverse, ...(slug === 'parent' ? { ownership: 'system', inverse_type_id: 2 } : {}) })];
    if (slug === 'parent') controller.relationshipTypes.push(relationshipType({ id: 2, slug: 'child', ownership: 'system', is_canonical: false, inverse_type_id: 1, forward_label: 'child', reverse_label: 'parent' }));
    const rendered = render(RelationshipsTab, { client, controller, personID: 7 });
    try {
      await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);
      await openTypeahead('Person');
      await fireEvent.mouseDown(await screen.findByRole('option', { name: 'Synthetic Two' }));
      const input = await openTypeahead('Connection type');
      if (slug === 'parent') await fireEvent.input(input, { target: { value: 'child' } });
      await fireEvent.mouseDown(screen.getByRole('treeitem', { name: caption }));
      expect(screen.getByRole('button', { name: `Connection type: ${phrase}` })).toBeDefined();
      await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);
      await waitFor(() => expect(post).toBeDefined());
      await expect(post!.clone().json()).resolves.toEqual({ source_person_id: source, target_person_id: target, relationship_type_slug: slug });
      await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
      expect(screen.getByText('Synthetic Two').closest('.detail-row')?.textContent).toContain(label.charAt(0).toUpperCase() + label.slice(1));
    } finally { rendered.unmount(); controller.destroy(); }
  });

  it('shows no-match, empty, and failed searches without losing a selected label', async () => {
    const requests: Request[] = [];
    let fail = true;
    let empty = false;
    const { client, controller } = controllerWith(async input => {
      const request = requestOf(input);
      requests.push(request);
      const q = new URL(request.url).searchParams.get('q');
      if (fail) return Response.json({ message: 'Search unavailable' }, { status: 503 });
      return Response.json({ people: q || empty ? [] : [
        directoryPerson(7, { display_name: 'Selected Self' }),
        directoryPerson(8, { display_name: 'Synthetic Peer', organizations: ['Example Labs'] })
      ] });
    });
    render(RelationshipsTab, { client, controller, personID: 7 });
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);
    let input = await openTypeahead('Person');
    await screen.findByText("Couldn't search people.");
    expect(requests).toHaveLength(1);
    fail = false;
    await fireEvent.keyDown(input, { key: 'Escape' });
    input = await openTypeahead('Person');
    expect(await screen.findByRole('option', { name: /Synthetic Peer/ })).toBeDefined();
    expect(screen.queryByText('Selected Self')).toBeNull();
    expect(requests).toHaveLength(2);
    const query = new URL(requests[0].url).searchParams;
    expect(query.has('q')).toBe(false);
    expect(query.get('sort')).toBe('last_contact_desc');
    expect(query.get('limit')).toBe('20');
    await fireEvent.mouseDown(screen.getByRole('option', { name: /Synthetic Peer/ }));
    expect(screen.getByRole('button', { name: 'Person: Synthetic Peer' })).toBeDefined();
    input = await openTypeahead('Person');
    expect(requests).toHaveLength(2);
    await fireEvent.input(input, { target: { value: 'Nobody' } });
    expect(await screen.findByText('No one matches “Nobody”')).toBeDefined();
    await fireEvent.input(input, { target: { value: '' } });
    expect(await screen.findByRole('option', { name: /Synthetic Peer/ })).toBeDefined();
    await fireEvent.keyDown(input, { key: 'Escape' });
    expect(screen.getByRole('button', { name: 'Person: Synthetic Peer' })).toBeDefined();
    input = await openTypeahead('Person');
    fail = true;
    await fireEvent.input(input, { target: { value: 'Retry' } });
    expect(await screen.findByText("Couldn't search people.")).toBeDefined();
    expect(screen.queryByText('No one matches “Retry”')).toBeNull();
    expect(screen.queryByText('No other saved people')).toBeNull();
    fail = false;
    empty = true;
    await fireEvent.input(input, { target: { value: 'Nobody' } });
    await screen.findByText('No one matches “Nobody”');
    await fireEvent.input(input, { target: { value: '' } });
    expect(await screen.findByText('No other saved people')).toBeDefined();
    await fireEvent.keyDown(input, { key: 'Escape' });
    expect(screen.getByRole('button', { name: 'Person: Synthetic Peer' })).toBeDefined();
  });

  it.each(['incoming', 'outgoing'] as const)('reads the existing %s connection as a sentence', async direction => {
    const { client, controller } = controllerWith(async () => Response.json({ reviews: [] }));
    controller.relationshipTypes = [relationshipType()];
    controller.relationships = [view(41, direction, direction === 'incoming' ? 'is mentored by' : 'mentors', 'Synthetic Peer')];
    render(RelationshipsTab, { client, controller, personID: 7, personName: 'Synthetic Subject' });
    await rowAction('Edit…');
    expect(screen.getByRole('dialog').textContent).toContain(`Synthetic Subject ${direction === 'incoming' ? 'is mentored by' : 'mentors'} Synthetic Peer`);
  });

  it('separates suggestions from reviewed imports', async () => {
    const review = { id: 1, person_id: 7, matched_person_id: 8, raw_related_value: 'Synthetic Peer', raw_related_type: 'friend', value_kind: 'text', status: 'pending', source: 'carddav_import', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z' };
    const client = createAPIClient(async () => Response.json({ reviews: [review, { ...review, id: 2, status: 'rejected', matched_person_id: null, raw_related_value: 'Earlier suggestion', reviewed_at: '2026-08-02T12:00:00Z' }, { ...review, id: 3, status: 'accepted', raw_related_value: 'Confirmed suggestion' }] }));
    const controller = new DirectoryEntityController(client, 7);
    controller.relationships = [view(41, 'outgoing', 'friend', 'Confirmed person', { counterpart_person_id: 8 })];
    const onOpenPerson = vi.fn();
    render(RelationshipsTab, { client, controller, personID: 7, onOpenPerson });
    expect(await screen.findByRole('heading', { name: 'Suggested (1)' })).toBeDefined();
    await fireEvent.click(screen.getByText('2 reviewed'));
    const row = screen.getByText('Synthetic Peer').closest('.detail-row') as HTMLElement;
    expect(row.textContent).toContain('Friend · from CardDAV contacts');
    expect(within(row).queryByText('Pending')).toBeNull();
    expect(screen.getAllByRole('button', { name: 'Open matched person' }).length).toBeGreaterThan(0);
    const rejected = screen.getByText('Rejected').closest('.detail-row') as HTMLElement;
    expect(rejected.querySelector('.description')?.getAttribute('title')).toContain('Reviewed');
    await fireEvent.click(within(row).getByRole('button', { name: 'Open matched person' }));
    expect(onOpenPerson).toHaveBeenCalledWith(8);
    expect(screen.getByText('Rejected').closest('.exception')).toBeTruthy();
  });

  it('renders curated connections', () => {
    const { client, controller } = controllerWith(async input => {
      const path = pathOf(requestOf(input));
      throw new Error(`Unexpected ${path}`);
    });
    controller.relationships = [
      view(41, 'outgoing', 'mentors', 'Synthetic Learner'),
      view(42, 'incoming', 'is mentored by', 'Synthetic Guide'),
      view(43, 'outgoing', 'works with', 'Synthetic Peer', {
        relationship: relationship(43, 1, { is_symmetric: true, forward_label: 'works with', reverse_label: 'works with' })
      })
    ];
    render(RelationshipsTab, { client, controller, personID: 7 });
    expect(screen.queryByRole('heading', { name: 'Connection types' })).toBeNull();
    for (const [name, label] of [['Synthetic Learner', 'Mentors'], ['Synthetic Guide', 'Is mentored by'], ['Synthetic Peer', 'Works with']]) {
      const row = screen.getByText(name).closest('.detail-row') as HTMLElement;
      expect(within(row).getByText(label)).toBeDefined();
    }
    expect(screen.queryByText(/outgoing|incoming/)).toBeNull();
    expect(screen.getAllByRole('button', { name: /^Actions for /  })).toHaveLength(3);
  });

  it('refreshes imported connections when the surviving person reloads in place', async () => {
    vi.stubEnv('TZ', 'UTC');
    let merged = false;
    const imported = { id: 71, person_id: 7, matched_person_id: 141, raw_related_value: 'absorbed@example.com', raw_related_type: 'X-CUSTOM_friend', value_kind: 'text', status: 'accepted', source: 'vcard_import', created_at: '2026-08-01T12:00:00Z', updated_at: '2026-08-01T12:00:00Z', reviewed_at: '2026-08-02T12:00:00Z' };
    const client = createAPIClient(async input => {
      const path = pathOf(requestOf(input));
      if (path === '/api/v1/person-relationship-reviews') return Response.json({ reviews: [{ ...imported, id: 70, raw_related_value: 'existing@example.com' }, ...(merged ? [imported] : [])] });
      if (path === '/api/v1/people/7/relationships') return Response.json({ relationships: [] });
      throw new Error(`Unexpected ${path}`);
    });
    const controller = new DirectoryEntityController(client, 7);
    const rendered = render(RelationshipsTab, { client, controller, personID: 7 });
    try {
      await screen.findByText('existing@example.com');
      expect(screen.getByText('Accepted').closest('.exception')).toBeTruthy();
      expect(screen.getByText('existing@example.com').closest('.detail-row')?.textContent).toContain('X CUSTOM friend · from a contact file');
      merged = true;
      await controller.refreshRelationships();
      expect(await screen.findByText('absorbed@example.com')).toBeDefined();
      expect(screen.getAllByText('Accepted')).toHaveLength(2);
    } finally { rendered.unmount(); controller.destroy(); vi.unstubAllEnvs(); }
  });

  it('renders the shared type catalog without person connections and keeps system types read-only', () => {
    const { client, controller } = controllerWith(vi.fn(), 0);
    controller.relationshipTypes = [
      relationshipType({ ownership: 'system', is_deletable: false }),
      relationshipType({ id: 32, slug: 'custom', forward_label: 'supports', reverse_label: 'is supported by' })
    ];

    render(RelationshipsTab, { client, controller, personID: 0, catalogOnly: true });
    expect(screen.queryByRole('heading', { name: 'Connections' })).toBeNull();
    expect(screen.queryByRole('button', { name: /^Add connection/ })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Edit mentor relationship type' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Delete mentor relationship type' })).toBeNull();
    expect(screen.getByText('Built in')).toBeDefined();
    expect(within(screen.getByRole('list', { name: 'Built-in connection types' })).getByText('mentors / is mentored by')).toBeDefined();
    expect(screen.queryByText('mentor')).toBeNull();
    expect(screen.getByRole('button', { name: 'Edit custom relationship type' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Delete custom relationship type' })).toBeDefined();
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

    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
    void controller.refreshRelationships();
    await screen.findByText('Loading connections…');
    resolveFirst(Response.json({ error: 'unavailable', message: 'relationships unavailable' }, { status: 503 }));

    const alert = (await screen.findByRole('alert')).closest('.inline-notice')!;
    expect(alert.textContent).toContain('relationships unavailable');
    await fireEvent.click(within(alert as HTMLElement).getByRole('button', { name: 'Retry' }));

    expect(await screen.findByText('No connections yet')).toBeDefined();
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
    }), 0);
    controller.errors.relationshipTypes = 'relationship types unavailable';
    render(RelationshipsTab, { client, controller, personID: 0, catalogOnly: true });

    expect(screen.getByRole('alert').textContent).toContain('relationship types unavailable');
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh relationship types' }));

    await waitFor(() => expect(controller.relationshipTypes).toHaveLength(1));
    expect(reads).toBe(1);
    expect(controller.errors.relationshipTypes).toBeUndefined();
  });

  it('saves optional connection dates and notes', async () => {
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

      await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);
      const personSearch = await openTypeahead('Person');
      await fireEvent.input(personSearch, { target: { value: 'Synthetic' } });
      await vi.advanceTimersByTimeAsync(250);
      expect(await screen.findByRole('option', { name: /Synthetic Counterpart/ })).toBeDefined();
      await fireEvent.mouseDown(screen.getByRole('option', { name: /Synthetic Counterpart/ }));
      await openTypeahead('Connection type');
      await fireEvent.mouseDown(screen.getByRole('treeitem', { name: 'Is mentored by' }));
      await fireEvent.input(screen.getByRole('textbox', { name: 'Start date' }), { target: { value: '2025' } });
      await fireEvent.input(screen.getByRole('textbox', { name: 'End date' }), { target: { value: '2026-08' } });
      await fireEvent.input(screen.getByRole('textbox', { name: 'Notes' }), { target: { value: 'Synthetic note' } });
      await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);

      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Add connection' })).toBeNull());
      const post = requests.find((request) => pathOf(request) === '/api/v1/person-relationships')!;
      await expect(post.clone().json()).resolves.toMatchObject({
        source_person_id: 8,
        target_person_id: 7,
        start_date: '2025',
        end_date: '2026-08',
        notes: 'Synthetic note'
      });
      expect((await post.clone().json()).source).toBeUndefined();
      const createdRow = screen.getByText('Synthetic Counterpart').closest('.detail-row') as HTMLElement;
      expect(createdRow.textContent).toContain('Is mentored by');
    } finally {
      vi.useRealTimers();
    }
  });

  it('rejects an impossible relationship date and keeps Create disabled until it is fixed', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const requests: Request[] = [];
      const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
        const request = requestOf(input); requests.push(request);
        if (pathOf(request) === '/api/v1/people/directory') return Response.json({ people: [
          { id: 8, revision: 1, display_name: 'Synthetic Counterpart', categories: [], contact_state: 'active', organizations: [] }
        ] });
        throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
      }));
      controller.relationshipTypes = [relationshipType()];
      render(RelationshipsTab, { client, controller, personID: 7 });

      await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);
      const personSearch = await openTypeahead('Person');
      await fireEvent.input(personSearch, { target: { value: 'Synthetic' } });
      await vi.advanceTimersByTimeAsync(250);
      await fireEvent.mouseDown(await screen.findByRole('option', { name: /Synthetic Counterpart/ }));
      await openTypeahead('Connection type');
      await fireEvent.mouseDown(screen.getByRole('treeitem', { name: 'Mentors' }));
      const create = screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!;
      expect(create).toHaveProperty('disabled', false);

      const start = screen.getByRole('textbox', { name: 'Start date' });
      await fireEvent.input(start, { target: { value: '2019-02-30' } });
      expect(create).toHaveProperty('disabled', true);
      await fireEvent.click(create);
      expect(requests.filter(request => request.method === 'POST')).toHaveLength(0);

      await fireEvent.input(start, { target: { value: '2019' } });
      expect(create).toHaveProperty('disabled', false);

      const end = screen.getByRole('textbox', { name: 'End date' });
      await fireEvent.input(end, { target: { value: '--04-12' } });
      expect(create).toHaveProperty('disabled', true);
      await fireEvent.click(create);
      expect(requests.filter(request => request.method === 'POST')).toHaveLength(0);
    } finally {
      vi.useRealTimers();
    }
  });

  it('reloads relationship types when Add connection opens after the first load failed', async () => {
    let reads = 0;
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (pathOf(request) === '/api/v1/relationship-types') {
        reads += 1;
        return reads === 1 ? Response.json({ message: 'Types unavailable' }, { status: 503 }) : Response.json({ relationship_types: [relationshipType()] });
      }
      if (pathOf(request) === '/api/v1/people/directory') return Response.json({ people: [] });
      throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
    }));
    controller.errors.relationshipTypes = 'relationship types unavailable';
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);
    await waitFor(() => expect(reads).toBe(1));
    const input = await openTypeahead('Connection type');
    await screen.findByText("Couldn't load connection types.");
    await fireEvent.keyDown(input, { key: 'Escape' });
    await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);
    await waitFor(() => expect(controller.relationshipTypes).toHaveLength(1));
    expect(reads).toBe(2);
    expect(controller.errors.relationshipTypes).toBeFalsy();
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
        return new Response(JSON.stringify(relationship(41, reads, { notes: 'Server concurrent' })), { headers: { ETag: `"person-relationship-41-r${reads}"` } });
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

    await rowAction('Edit…');
    const notes = await screen.findByRole('textbox', { name: 'Notes' });
    await fireEvent.input(notes, { target: { value: 'Retained local draft' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    expect((await screen.findByRole('alert')).textContent).toContain('This connection changed since you opened it.');
    expect(screen.getByRole('alert').querySelector('details')?.open).toBe(false);
    expect((screen.getByRole('textbox', { name: 'Notes' }) as HTMLInputElement).value).toBe('Retained local draft');
    const changes = screen.getByRole('table', { name: 'What changed' });
    expect(within(changes).getAllByRole('row').slice(1).map(row => [...row.querySelectorAll('[role=rowheader], [role=cell]')].map(cell => cell.textContent))).toEqual([['Notes', 'Server concurrent', 'Retained local draft']]);
    expect(writes).toBe(1);
    expect(reads).toBe(2);
    const patch = requests.find((request) => request.method === 'PATCH')!;
    expect(patch.headers.get('If-Match')).toBe('"person-relationship-41-r1"');
  });

  it('shows a past connection’s dates as a range', async () => {
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async () => { throw new Error('unexpected request'); }));
    controller.relationships = [view(42, 'outgoing', 'friend', 'Sela Moss', { relationship: { ...relationship(42, 1), status: 'ended', start_date: { year: 2020 }, end_date: { year: 2023 }, notes: 'Former workshop partner.' } })];
    controller.relationshipTypes = [relationshipType()];
    render(RelationshipsTab, { client, controller, personID: 7, personName: 'Mira Vale' });
    await fireEvent.click(screen.getByText('1 past connection'));
    expect(screen.getByText('Sela Moss').closest('.detail-row')?.textContent).toContain('Friend · 2020 – 2023 · Former workshop partner.');
  });

  it('opens Edit with the connection as context: since when, who added it, and prose notes', async () => {
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (pathOf(request) === '/api/v1/person-relationships/41') return new Response(JSON.stringify(relationship(41, 1)), { headers: { ETag: '"relationship-41-r1"' } });
      throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
    }));
    controller.relationships = [view(41, 'outgoing', 'friend', 'Tavi Reed', { relationship: { ...relationship(41, 1), start_date: { year: 2022 }, notes: 'Met at the seed exchange.' } })];
    controller.relationshipTypes = [relationshipType()];
    render(RelationshipsTab, { client, controller, personID: 7, personName: 'Mira Vale' });
    expect(screen.getByText('Tavi Reed').closest('.detail-row')?.textContent).toContain('Friend · 2022 – present · Met at the seed exchange.');
    await rowAction('Edit…');
    const dialog = await screen.findByRole('dialog', { name: 'Edit connection' });
    // A connection the person added says only since when; one from somewhere else says where.
    expect(within(dialog).getByText('Since 2022')).toBeDefined();
    expect(within(dialog).queryByText(/added by you/)).toBeNull();
    expect(within(dialog).queryByText('You')).toBeNull();
    expect(within(dialog).getByRole('textbox', { name: 'Notes' }).tagName).toBe('TEXTAREA');
  });

  it('patches only a changed end date so a fresh server note is not overwritten', async () => {
    const requests: Request[] = [];
    let reads = 0;
    const opening = relationship(41, 1, { notes: 'Opening note', start_date: { year: 2024, month: 3 } });
    const fresh = relationship(41, 2, { notes: 'Concurrent note' });
    const updated = relationship(41, 3, { notes: 'Concurrent note', end_date: { year: 2026, month: 8 } });
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/person-relationships/41' && request.method === 'GET') {
        return ++reads === 1
          ? Response.json(opening, { headers: { ETag: '"relationship-41-r1"' } })
          : Response.json(fresh, { headers: { ETag: '"relationship-41-r2"' } });
      }
      if (path === '/api/v1/person-relationships/41' && request.method === 'PATCH') {
        if (request.headers.get('If-Match') === '"relationship-41-r1"') return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
        return new Response(JSON.stringify(updated), { headers: { ETag: '"relationship-41-r3"' } });
      }
      if (path === '/api/v1/people/7/relationships') {
        return Response.json({ relationships: [view(41, 'outgoing', 'mentors', 'Synthetic Learner', { relationship: updated })] });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.relationships = [view(41, 'outgoing', 'mentors', 'Synthetic Learner', { relationship: opening })];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await rowAction('Edit…');
    await fireEvent.input(screen.getByRole('textbox', { name: 'End date' }), { target: { value: '2024-02' } });
    expect(screen.getByText('End date must be on or after start date.')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Save' })).toHaveProperty('disabled', true);
    await fireEvent.input(screen.getByRole('textbox', { name: 'End date' }), { target: { value: '2026-08' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Keep yours' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Edit connection' })).toBeNull());
    const patches = requests.filter(request => request.method === 'PATCH');
    expect(patches.map(request => request.headers.get('If-Match'))).toEqual(['"relationship-41-r1"', '"relationship-41-r2"']);
    const patch = patches[1];
    await expect(patch.clone().json()).resolves.toEqual({ end_date: '2026-08' });
  });

  it('clears changed notes without resending an opening end date', async () => {
    const requests: Request[] = [];
    let reads = 0;
    const opening = relationship(41, 1, { notes: 'Opening note', end_date: { year: 2026, month: 6 } });
    const fresh = relationship(41, 2, { notes: 'Opening note', end_date: { year: 2026, month: 7 } });
    const updated = relationship(41, 3, { end_date: { year: 2026, month: 7 } });
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/person-relationships/41' && request.method === 'GET') {
        return ++reads === 1
          ? Response.json(opening, { headers: { ETag: '"relationship-41-r1"' } })
          : Response.json(fresh, { headers: { ETag: '"relationship-41-r2"' } });
      }
      if (path === '/api/v1/person-relationships/41' && request.method === 'PATCH') {
        if (request.headers.get('If-Match') === '"relationship-41-r1"') return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
        return new Response(JSON.stringify(updated), { headers: { ETag: '"relationship-41-r3"' } });
      }
      if (path === '/api/v1/people/7/relationships') {
        return Response.json({ relationships: [view(41, 'outgoing', 'mentors', 'Synthetic Learner', { relationship: updated })] });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.relationships = [view(41, 'outgoing', 'mentors', 'Synthetic Learner', { relationship: opening })];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await rowAction('Edit…');
    await fireEvent.input(screen.getByRole('textbox', { name: 'Notes' }), { target: { value: '   ' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Keep yours' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Edit connection' })).toBeNull());
    const patches = requests.filter(request => request.method === 'PATCH');
    expect(patches.map(request => request.headers.get('If-Match'))).toEqual(['"relationship-41-r1"', '"relationship-41-r2"']);
    const patch = patches[1];
    await expect(patch.clone().json()).resolves.toEqual({ notes: null });
  });

  it('closes an unchanged relationship edit without sending an empty patch', async () => {
    const fetchFn = vi.fn<typeof fetch>(async input => {
      const request = requestOf(input);
      throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
    });
    const { client, controller } = controllerWith(fetchFn);
    controller.relationships = [view(41, 'outgoing', 'mentors', 'Synthetic Learner', {
      relationship: relationship(41, 1, { notes: 'Opening note', end_date: { year: 2026, month: 6 } })
    })];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await rowAction('Edit…');
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Edit connection' })).toBeNull());
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it('deletes the single underlying edge with its fresh ETag', async () => {
    const requests: Request[] = [];
    const { client, controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/person-relationships/41' && request.method === 'GET') {
        return new Response(JSON.stringify(relationship(41, 1)), { headers: { ETag: '"relationship-41-r3"' } });
      }
      if (path === '/api/v1/person-relationships/41' && request.method === 'DELETE') return new Response(null, { status: 204 });
      if (path === '/api/v1/people/7/relationships') return Response.json({ relationships: [] });
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.relationships = [view(41, 'incoming', 'is mentored by', 'Synthetic Guide')];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await rowAction('Delete…');
    await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: /^Delete connection with/  })).toBeNull());
    const deletion = requests.find((request) => request.method === 'DELETE')!;
    expect(pathOf(deletion)).toBe('/api/v1/person-relationships/41');
    expect(deletion.headers.get('If-Match')).toBe('"relationship-41-r3"');
    expect(controller.relationships).toEqual([]);
  });

  it('keeps a delete conflict open, shows only changed fields, and reloads the connection', async () => {
    let reads = 0, deletes = 0, refreshes = 0;
    const current = relationship(41, 2, { notes: 'Saved note' });
    const { client, controller } = controllerWith(async input => {
      const request = requestOf(input), path = pathOf(request);
      if (path === '/api/v1/person-relationships/41' && request.method === 'GET') {
        reads++;
        return Response.json(reads === 1 ? relationship() : current, { headers: { ETag: `"person-relationship-41-r${reads}"` } });
      }
      if (path === '/api/v1/person-relationships/41' && request.method === 'DELETE') {
        deletes++;
        return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
      }
      if (path === '/api/v1/people/7/relationships') {
        refreshes++;
        return Response.json({ relationships: [view(41, 'outgoing', 'mentors', 'Synthetic Peer', { relationship: current })] });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    });
    controller.relationships = [view(41, 'outgoing', 'mentors', 'Synthetic Peer')];
    render(RelationshipsTab, { client, controller, personID: 7 });
    await rowAction('Delete…');
    await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('This connection changed since you opened it.');
    const changes = within(alert).getByRole('table', { name: 'What changed' });
    expect(within(changes).getAllByRole('rowheader').map(cell => cell.textContent)).toEqual(['Notes']);
    expect(changes.textContent).toContain('Saved note');
    expect(deletes).toBe(1);
    await fireEvent.click(within(alert).getByRole('button', { name: 'Reload' }));
    await waitFor(() => expect(screen.queryByRole('alert')).toBeNull());
    expect(refreshes).toBe(1);
    expect(controller.relationships[0].relationship.notes).toBe('Saved note');
    expect(screen.getByRole('dialog', { name: 'Delete connection with Synthetic Peer?' })).toBeDefined();
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

    await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);
    await fireEvent.input(await openTypeahead('Person'), { target: { value: 'Counterpart' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Synthetic Counterpart/ }));
    await openTypeahead('Connection type');
    await fireEvent.mouseDown(screen.getByRole('treeitem', { name: 'Mentors' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Notes' }), { target: { value: 'Retained unknown draft' } });
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);

    expect((await screen.findByRole('alert')).textContent).toContain("Couldn't confirm the connection was saved.");
    expect(screen.getByRole('alert').textContent).not.toContain('outcome unknown');
    expect((screen.getByRole('textbox', { name: 'Notes' }) as HTMLInputElement).value).toBe('Retained unknown draft');
    expect(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!).toHaveProperty('disabled', true);
    await fireEvent.click(within(screen.getByRole('dialog', { name: 'Add connection' })).getByRole('button', { name: 'Check again' }));
    await waitFor(() => expect(controller.createBlocked.relationships).toBe(false));
    expect((screen.getByRole('textbox', { name: 'Notes' }) as HTMLInputElement).value).toBe('Retained unknown draft');
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
    }), 0);
    render(RelationshipsTab, { client, controller, personID: 0, catalogOnly: true });

    await fireEvent.click(screen.getByRole('button', { name: 'Add type' }));
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
    const { client, controller } = controllerWith(fetchFn, 0);
    render(RelationshipsTab, { client, controller, personID: 0, catalogOnly: true });

    await fireEvent.click(screen.getByRole('button', { name: 'Add type' }));
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
    const { client, controller } = controllerWith(fetchFn, 0);
    controller.relationshipTypes = [symmetricType];
    render(RelationshipsTab, { client, controller, personID: 0, catalogOnly: true });

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
      throw new Error(`unexpected ${request.method} ${path}`);
    }), 0);
    controller.relationshipTypes = [current];
    render(RelationshipsTab, { client, controller, personID: 0, catalogOnly: true });

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
    }), 0);
    render(RelationshipsTab, { client, controller, personID: 0, catalogOnly: true });

    await fireEvent.click(screen.getByRole('button', { name: 'Add type' }));
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

  it('edits a catalog type with a fresh ETag without reading a person or patching symmetry', async () => {
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
      throw new Error(`unexpected ${request.method} ${path}`);
    }), 0);
    controller.relationshipTypes = [current];
    render(RelationshipsTab, { client, controller, personID: 0, catalogOnly: true });

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
    expect(screen.getByText(/guides \/ is mentored by/)).toBeDefined();
    expect(requests.map((request) => [request.method, pathOf(request)])).toEqual([['GET', '/api/v1/relationship-types/31'], ['PATCH', '/api/v1/relationship-types/31']]);
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
      throw new Error(`unexpected ${request.method} ${path}`);
    }), 0);
    controller.relationshipTypes = [current];
    render(RelationshipsTab, { client, controller, personID: 0, catalogOnly: true });

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
    const { client, controller } = controllerWith(fetchFn, 0);
    controller.relationshipTypes = [current];
    render(RelationshipsTab, { client, controller, personID: 0, catalogOnly: true });

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
        return Response.json({ error: 'relationship_revision_conflict', message: 'type changed elsewhere' }, { status: 409 });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }), 0);
    controller.relationshipTypes = [initial];
    render(RelationshipsTab, { client, controller, personID: 0, catalogOnly: true });

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
    const { client, controller } = controllerWith(vi.fn(), 0);
    controller.relationshipTypes = [
      relationshipType(),
      relationshipType({ id: 32, slug: 'locked', ownership: 'user', is_deletable: false, is_symmetric: true, forward_label: 'works with', reverse_label: 'works with' })
    ];
    render(RelationshipsTab, { client, controller, personID: 0, catalogOnly: true });

    await fireEvent.click(screen.getByRole('button', { name: 'Edit locked relationship type' }));

    expect(await screen.findByText('Symmetry cannot be changed after creation.')).toBeDefined();
    expect(screen.getByText('Symmetric')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Delete' })).toBeNull();
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
    }), 0);
    controller.relationshipTypes = [current];
    render(RelationshipsTab, { client, controller, personID: 0, catalogOnly: true });

    await fireEvent.click(screen.getByRole('button', { name: 'Delete mentor relationship type' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    const deletion = requests.find((request) => request.method === 'DELETE')!;
    expect(deletion.headers.get('If-Match')).toBe('"relationship-type-31-r1"');
    expect(controller.relationshipTypes).toEqual([]);
  });
});

it.each([2024, 2028])('validates connection retries against the saved start year %s', async year => {
  const opening = relationship(41, 1, { start_date: { year: 2026 }, notes: 'Original note' });
  const saved = relationship(41, 2, { start_date: { year }, notes: 'Concurrent note' });
  const { client, controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'PATCH') return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
    return Response.json(saved, { headers: { ETag: '"person-relationship-41-r2"' } });
  });
  controller.relationships = [view(41, 'outgoing', 'mentors', 'Synthetic Learner', { relationship: opening })];
  render(RelationshipsTab, { client, controller, personID: 7 });
  await rowAction('Edit…');
  const end = await screen.findByRole('textbox', { name: 'End date' });
  await fireEvent.input(end, { target: { value: '2027' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  await screen.findByRole('button', { name: 'Keep yours' });
  await fireEvent.input(end, { target: { value: year === 2028 ? '2027' : '2025' } });
  expect(screen.getByRole('button', { name: 'Keep yours' })).toHaveProperty('disabled', year === 2028);
});

it('shows only the connection values Keep yours will actually overwrite', async () => {
  const bodies: unknown[] = [];
  const opening = relationship(41, 1, { notes: 'Original note' });
  const saved = relationship(41, 2, { notes: 'Concurrent note' });
  const { client, controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'PATCH') {
      bodies.push(await request.json());
      return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
    }
    return Response.json(bodies.length ? saved : opening, { headers: { ETag: bodies.length ? '"person-relationship-41-r2"' : '"person-relationship-41-r1"' } });
  });
  controller.relationships = [view(41, 'outgoing', 'mentors', 'Synthetic Learner', { relationship: opening })];
  render(RelationshipsTab, { client, controller, personID: 7 });
  await rowAction('Edit…');
  await fireEvent.input(await screen.findByRole('textbox', { name: 'End date' }), { target: { value: '2027' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  await screen.findByRole('button', { name: 'Keep yours' });
  expect(screen.queryByRole('rowheader', { name: 'Notes' })).toBeNull();
  expect(screen.getByText('Other details also changed')).toBeDefined();
  expect(screen.getByText('Notes', { selector: 'dt' }).nextElementSibling?.textContent).toBe('Concurrent note');
  await fireEvent.click(screen.getByRole('button', { name: 'Keep yours' }));
  await waitFor(() => expect(bodies).toHaveLength(2));
  expect(bodies).toEqual([{ end_date: '2027' }, { end_date: '2027' }]);
});

it.each(['failed', 'committed'])('shows the latest outcome after a %s connection retry', async outcome => {
  let writes = 0;
  const opening = relationship(41, 1, { notes: 'Original note' });
  const { client, controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'PATCH') {
      if (++writes === 1) return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
      return outcome === 'failed'
        ? Response.json({ message: 'Disk full' }, { status: 500 })
        : Response.json(relationship(41, 3, { notes: 'Draft note' }), { headers: { ETag: '"relationship-41-r3"' } });
    }
    if (pathOf(request).includes('/people/7/relationships')) return Response.json({ message: 'Refresh failed' }, { status: 500 });
    return Response.json(opening, { headers: { ETag: '"relationship-41-r2"' } });
  });
  controller.relationships = [view(41, 'outgoing', 'mentors', 'Synthetic Learner', { relationship: opening })];
  render(RelationshipsTab, { client, controller, personID: 7 });
  await rowAction('Edit…');
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Notes' }), { target: { value: 'Draft note' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  await fireEvent.click(await screen.findByRole('button', { name: 'Keep yours' }));
  await screen.findByText(outcome === 'failed' ? "Couldn't save this connection." : "Connection saved. Couldn't refresh connections.");
  expect(screen.queryByRole('button', { name: 'Keep yours' })).toBeNull();
  if (outcome === 'failed') expect(screen.getByText('Disk full')).toBeDefined();
});

it.each(['cleared notes', 'derived status'])('compares %s after an end-date conflict', async change => {
  const opening = relationship(41, 1, change === 'cleared notes' ? { notes: 'Opening notes' } : {});
  const saved = relationship(41, 2, { end_date: { year: 2025 }, status: 'ended' });
  const { client, controller } = controllerWith(async input => requestOf(input).method === 'PATCH'
    ? Response.json({ message: 'Changed elsewhere' }, { status: 412 })
    : Response.json(saved, { headers: { ETag: '"person-relationship-41-r2"' } }));
  controller.relationships = [view(41, 'outgoing', 'mentors', 'Synthetic Learner', { relationship: opening })];
  render(RelationshipsTab, { client, controller, personID: 7 });
  await rowAction('Edit…');
  await fireEvent.input(await screen.findByRole('textbox', { name: 'End date' }), { target: { value: '2026' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  await screen.findByRole('button', { name: 'Keep yours' });
  if (change === 'cleared notes') {
    expect(screen.getByText('Other details also changed')).toBeDefined();
    expect(screen.getByText('Notes', { selector: 'dt' }).nextElementSibling?.textContent).toBe('None');
  } else expect(screen.queryByText('Other details also changed')).toBeNull();
});

it.each([false, true])('reloads a pending connection deletion, disappeared=%s', async gone => {
  const tags: (string | null)[] = [];
  const saved = view(41, 'outgoing', 'mentors', 'Synthetic Peer', { relationship: relationship(41, 2) });
  const { client, controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (pathOf(request) === '/api/v1/people/7/relationships') return Response.json({ relationships: gone ? [] : [saved] });
    if (request.method === 'GET') return Response.json(tags.length ? saved.relationship : relationship(41, 1), { headers: { ETag: tags.length ? '"person-relationship-41-r2"' : '"person-relationship-41-r1"' } });
    tags.push(request.headers.get('If-Match'));
    return tags.length === 1 ? Response.json({ message: 'Changed elsewhere' }, { status: 412 }) : new Response(null, { status: 204 });
  });
  controller.relationships = [view(41, 'outgoing', 'mentors', 'Synthetic Peer')];
  render(RelationshipsTab, { client, controller, personID: 7 });
  await rowAction('Delete…');
  await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
  await fireEvent.click(await screen.findByRole('button', { name: 'Reload' }));
  await waitFor(() => expect(screen.queryByText('This connection changed since you opened it.')).toBeNull());
  if (gone) expect(screen.queryByRole('dialog')).toBeNull();
  else {
    await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(tags).toHaveLength(2));
    expect(tags[1]).toBe('"person-relationship-41-r2"');
  }
});
