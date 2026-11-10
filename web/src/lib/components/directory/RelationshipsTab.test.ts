import { controllerWith as entityControllerWith, directoryPerson, relationshipType, relationship, requestOf, pathOf } from '../../directory/entity-fixtures.test-support';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterAll, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { DirectoryEntityController } from '../../directory/entity-controller.svelte';
import { openTypeahead } from '../../../test/kit-ui';
import RelationshipsTab from './RelationshipsTab.svelte';

const formatter = vi.hoisted(() => {
  const DateTimeFormat = Intl.DateTimeFormat;
  return vi.spyOn(Intl, 'DateTimeFormat').mockImplementation(function (locale, options) { return new DateTimeFormat(locale ?? 'en-US', options); });
});
afterAll(() => formatter.mockRestore());

function controllerWith(fetchFn: typeof fetch, personID = 7, options: { onDirectoryChange?: () => void | Promise<void> } = {}) {
  const result = entityControllerWith(async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    if (request.method === 'GET' && pathOf(request) === '/api/v1/person-relationship-reviews') return Response.json({ reviews: [] });
    return fetchFn(input, init);
  }, personID, options);
  return result;
}

function view(id: number, direction: 'incoming' | 'outgoing', label: string, name: string, overrides: Record<string, unknown> = {}) {
  const counterpartID = (overrides.counterpart_person_id as number | undefined) ?? id + 100;
  const edge = (overrides.relationship as ReturnType<typeof relationship> | undefined) ?? relationship(id, 1);
  return {
    counterpart_person_id: counterpartID,
    counterpart_display_name: name,
    counterpart_label: label,
    counterpart_vcard_uid: `person-${id + 100}`,
    direction,
    ...overrides,
    relationship: { ...edge, source_person_id: direction === 'incoming' ? counterpartID : 7, target_person_id: direction === 'incoming' ? 7 : counterpartID }
  };
}

async function rowAction(label: string): Promise<void> {
  await fireEvent.click(screen.getAllByRole('button', { name: /^Actions for /  })[0]);
  await fireEvent.click(await screen.findByRole('menuitem', { name: label }));
  if (label === 'Edit…') await waitFor(() => expect(screen.queryByText('Loading connection…')).toBeNull());
  if (label === 'Delete…') await waitFor(() => expect(screen.getByRole('button', { name: 'Delete' })).toHaveProperty('disabled', false));
}

describe('RelationshipsTab', () => {
  it('shows ended connection dates with day precision', async () => {
    const { client, controller } = controllerWith(async () => Response.json({ relationships: [] }));
    const item = view(41, 'outgoing', 'is mentored by', 'Synthetic Peer', { relationship: relationship(41, 1, { source_person_id: 7, target_person_id: 141, status: 'ended', start_date: { year: 2024, month: 1, day: 5 }, end_date: { year: 2024, month: 1, day: 6 } }) });
    controller.relationships = [item];
    render(RelationshipsTab, { client, controller, personID: 7 });
    await fireEvent.click(screen.getByText('1 past connection'));
    expect(screen.getByText(/Jan 5, 2024 – Jan 6, 2024/)).toBeDefined();
  });
  it('selects the first type option with Enter after filtering', async () => {
    const { client, controller } = controllerWith(async input => {
      const path = pathOf(requestOf(input));
      if (path === '/api/v1/relationship-types') return Response.json({ relationship_types: [relationshipType({ slug: 'parent', forward_label: 'parent', reverse_label: 'child' })] });
      throw new Error(`unexpected ${path}`);
    });
    controller.relationshipTypes = [relationshipType({ slug: 'parent', forward_label: 'parent', reverse_label: 'child' })];
    render(RelationshipsTab, { client, controller, personID: 7 });
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);
    const input = await openTypeahead('Connection type');
    await fireEvent.input(input, { target: { value: 'child' } });
    expect(document.getElementById(input.getAttribute('aria-activedescendant')!)?.textContent?.trim()).toBe('Child');
    await fireEvent.keyDown(input, { key: 'Enter' });
    expect(screen.queryByRole('combobox', { name: 'Connection type' })).toBeNull();
  });

  it('shows a type failure with retry, then query-specific empty copy', async () => {
    let reads = 0;
    const { client, controller } = controllerWith(async input => {
      const path = pathOf(requestOf(input));
      if (path !== '/api/v1/relationship-types') throw new Error(`unexpected ${path}`);
      return ++reads === 1 ? Response.json({ message: 'Types unavailable' }, { status: 503 }) : Response.json({ relationship_types: [relationshipType()] });
    });
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

  it.each([
    ['uri', 'urn:uuid:11111111-2222-3333-4444-555555555555', null, 'Unknown contact', 'vcard_import', 'Friend · from a contact file · Jul 31 · urn:uuid:11111111-2222-3333-4444-555555555555'],
    ['uri', 'urn:uuid:11111111-2222-3333-4444-555555555555', 8, 'Matched contact', 'vcard_import', 'Friend · from a contact file · Jul 31 · urn:uuid:11111111-2222-3333-4444-555555555555'],
    ['text', 'Alex', null, 'Alex', 'carddav_import', 'Text · Friend · from CardDAV contacts · Jul 31'],
  ])('describes imported %s value %s with match %s', async (valueKind, value, matched, title, source, description) => {
    vi.setSystemTime(new Date(2026, 10, 6, 12));
    try {
    const client = createAPIClient(async () => Response.json({ reviews: [{
      id: 1, person_id: 7, raw_related_value: value, raw_related_type: 'friend', value_kind: valueKind, status: 'pending', matched_person_id: matched,
      source, created_at: '2026-07-31T12:00:00Z', updated_at: '2026-07-31T12:00:00Z',
    }] }));
    const onOpenPerson = vi.fn();
    render(RelationshipsTab, { client, controller: new DirectoryEntityController(client, 7), personID: 7, onOpenPerson });
    const row = (await screen.findByText(title)).closest('.detail-row') as HTMLElement;
    expect(row.querySelector('.description')?.textContent).toBe(description);
    if (matched) { await fireEvent.click(within(row).getByRole('button', { name: 'Open matched person' })); expect(onOpenPerson).toHaveBeenCalledWith(matched); }
    } finally { vi.useRealTimers(); }
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

  it.each([
    ['Mentors', 'mentors', 'mentor', 'mentors', 'is mentored by', 7, 8],
    ['Child', 'is a child of', 'parent', 'parent', 'child', 8, 7],
  ])('creates literal endpoints for %s', async (caption, phrase, slug, forward, reverse, source, target) => {
    let post: Request | undefined;
    let releasePost!: (response: Response) => void;
    const pendingPost = new Promise<Response>(resolve => { releasePost = resolve; });
    const saved = relationship(41, 1, { relationship_type_id: 1, type_slug: slug, source_person_id: source, target_person_id: target, forward_label: forward, reverse_label: reverse });
    const direction = source === 7 ? 'outgoing' : 'incoming';
    const label = direction === 'outgoing' ? reverse : forward;
    const { client, controller } = controllerWith(async input => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/person-relationships' && request.method === 'POST') { post = request; return pendingPost; }
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
      if (caption === 'Mentors') {
        const start = screen.getByRole('textbox', { name: 'Start date' });
        await fireEvent.input(start, { target: { value: '2019-02-30' } });
        expect(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!).toHaveProperty('disabled', true);
        await fireEvent.input(start, { target: { value: '2025' } });
        await fireEvent.input(screen.getByRole('textbox', { name: 'End date' }), { target: { value: '2026-08' } });
        await fireEvent.input(screen.getByRole('textbox', { name: 'Notes' }), { target: { value: 'Synthetic note' } });
      }
      await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);
      await waitFor(() => expect(post).toBeDefined());
      expect(screen.getByRole('textbox', { name: 'Notes' })).toHaveProperty('disabled', true);
      expect(screen.getByRole('button', { name: 'Person: Synthetic Two' })).toHaveProperty('disabled', true);
      await expect(post!.clone().json()).resolves.toEqual({ source_person_id: source, target_person_id: target, relationship_type_slug: slug, ...(caption === 'Mentors' ? { start_date: '2025', end_date: '2026-08', notes: 'Synthetic note' } : {}) });
      releasePost(Response.json(saved));
      await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    } finally { releasePost(Response.json(saved)); rendered.unmount(); controller.destroy(); }
  });

  it('shows no-match and empty searches without losing a selected label', async () => {
    const requests: Request[] = [];
    let empty = false;
    const { client, controller } = controllerWith(async input => {
      const request = requestOf(input);
      requests.push(request);
      const q = new URL(request.url).searchParams.get('q');
      return Response.json({ people: q || empty ? [] : [
        directoryPerson(7, { display_name: 'Selected Self' }),
        directoryPerson(8, { display_name: 'Synthetic Peer', organizations: ['Example Labs'] })
      ] });
    });
    render(RelationshipsTab, { client, controller, personID: 7 });
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);
    let input = await openTypeahead('Person');
    expect(await screen.findByRole('option', { name: /Synthetic Peer/ })).toBeDefined();
    expect(screen.queryByText('Selected Self')).toBeNull();
    expect(requests).toHaveLength(1);
    const query = new URL(requests[0].url).searchParams;
    expect(query.has('q')).toBe(false);
    expect(query.get('sort')).toBe('last_contact_desc');
    expect(query.get('limit')).toBe('20');
    await fireEvent.mouseDown(screen.getByRole('option', { name: /Synthetic Peer/ }));
    expect(screen.getByRole('button', { name: 'Person: Synthetic Peer' })).toBeDefined();
    input = await openTypeahead('Person');
    expect(requests).toHaveLength(1);
    await fireEvent.input(input, { target: { value: 'Nobody' } });
    expect(await screen.findByText('No one matches “Nobody”')).toBeDefined();
    await fireEvent.input(input, { target: { value: '' } });
    expect(await screen.findByRole('option', { name: /Synthetic Peer/ })).toBeDefined();
    await fireEvent.keyDown(input, { key: 'Escape' });
    expect(screen.getByRole('button', { name: 'Person: Synthetic Peer' })).toBeDefined();
    input = await openTypeahead('Person');
    empty = true;
    await fireEvent.input(input, { target: { value: 'Nobody' } });
    await screen.findByText('No one matches “Nobody”');
    await fireEvent.input(input, { target: { value: '' } });
    expect(await screen.findByText('No other saved people')).toBeDefined();
    await fireEvent.keyDown(input, { key: 'Escape' });
    expect(screen.getByRole('button', { name: 'Person: Synthetic Peer' })).toBeDefined();
  });

  it.each([
    ['incoming', {}, 'Synthetic Subject · Is mentored by · Synthetic Peer'],
    ['outgoing', {}, 'Synthetic Subject · Mentors · Synthetic Peer'],
    ['outgoing', { relationship_type_id: 32, type_slug: 'friend', forward_label: 'friend', reverse_label: 'friend', is_symmetric: true }, 'Synthetic Subject is a friend of Synthetic Peer'],
  ] as const)('names the existing %s connection %o as %s', async (direction, type, line) => {
    const existing = relationship(41, 1, { ...type, source_person_id: direction === 'incoming' ? 141 : 7, target_person_id: direction === 'incoming' ? 7 : 141, start_date: { year: 2022 }, notes: 'Met at the seed exchange.' });
    const { client, controller } = controllerWith(async () => Response.json(existing, { headers: { ETag: '"person-relationship-41-r1"' } }));
    controller.relationshipTypes = [relationshipType(), relationshipType({ id: 32, slug: 'friend', forward_label: 'friend', reverse_label: 'friend', is_symmetric: true })];
    controller.relationships = [view(41, direction, direction === 'incoming' ? 'is mentored by' : 'mentors', 'Synthetic Peer', { relationship: existing })];
    render(RelationshipsTab, { client, controller, personID: 7, personName: 'Synthetic Subject' });
    await rowAction('Edit…');
    const dialog = await screen.findByRole('dialog', { name: 'Edit connection' });
    expect(within(dialog).getByRole('textbox', { name: 'Notes' }).tagName).toBe('TEXTAREA');
    expect(dialog.textContent).toContain(line);
    expect(await within(dialog).findByText('Since 2022')).toBeDefined();
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
    expect(row.textContent).toContain('Friend');
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
      view(41, 'outgoing', 'is mentored by', 'Synthetic Learner'),
      view(42, 'incoming', 'mentors', 'Synthetic Guide'),
      view(43, 'outgoing', 'works with', 'Synthetic Peer', {
        relationship: relationship(43, 1, { is_symmetric: true, forward_label: 'works with', reverse_label: 'works with' })
      })
    ];
    render(RelationshipsTab, { client, controller, personID: 7 });
    expect(screen.queryByRole('heading', { name: 'Connection types' })).toBeNull();
    for (const [name, label] of [['Synthetic Learner', 'Is mentored by'], ['Synthetic Guide', 'Mentors'], ['Synthetic Peer', 'Works with']]) {
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
      expect(screen.getByText('existing@example.com').closest('.detail-row')?.textContent).toContain('X CUSTOM friend');
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

  it('shows the saved connection, and Keep yours writes the form over it', async () => {
    const requests: Request[] = [];
    let reads = 0;
    const opening = relationship(41, 1, { status: 'ended', notes: 'Opening note', start_date: { year: 2024, month: 3 }, end_date: { year: 2026, month: 6 } });
    const fresh = relationship(41, 2, { status: 'ended', notes: 'Concurrent note', start_date: { year: 2024, month: 3 }, end_date: { year: 2026, month: 7 } });
    const updated = relationship(41, 3, { status: 'ended', end_date: { year: 2026, month: 7 } });
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
        if (requests.filter(sent => sent.method === 'PATCH').length === 2) return Response.json({ message: 'Disk full' }, { status: 500 });
        return new Response(JSON.stringify(updated), { headers: { ETag: '"relationship-41-r3"' } });
      }
      if (path === '/api/v1/people/7/relationships') {
        return Response.json({ relationships: [view(41, 'outgoing', 'is mentored by', 'Synthetic Learner', { relationship: updated })] });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.relationships = [view(41, 'outgoing', 'is mentored by', 'Synthetic Learner', { relationship: opening })];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await fireEvent.click(screen.getByText('1 past connection'));
    await rowAction('Edit…');
    await fireEvent.input(screen.getByRole('textbox', { name: 'End date' }), { target: { value: '2026-06' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Notes' }), { target: { value: '   ' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('This connection changed since you opened it.')).toBeDefined();
    await screen.findByRole('button', { name: 'Keep yours' });
    expect((screen.getByRole('textbox', { name: 'Notes' }) as HTMLTextAreaElement).value).toBe('   ');
    expect(requests.filter(request => request.method === 'PATCH')).toHaveLength(1);
    await fireEvent.input(screen.getByRole('textbox', { name: 'Notes' }), { target: { value: 'Opening note' } });
    expect(within(screen.getByRole('region', { name: 'Saved connection' })).getByText('Notes').nextElementSibling?.textContent).toBe('Concurrent note');
    await fireEvent.click(screen.getByRole('button', { name: 'Keep yours' }));
    // The form is back to its opening values, and a failed Keep yours still retries.
    await fireEvent.click(await screen.findByRole('button', { name: 'Retry' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Edit connection' })).toBeNull());
    const patches = requests.filter(request => request.method === 'PATCH');
    await expect(patches[0].clone().json()).resolves.toEqual({ end_date: '2026-06', notes: null });
    for (const retry of patches.slice(1)) {
      await expect(retry.clone().json()).resolves.toEqual({ end_date: '2026-06', notes: 'Opening note' });
    }
    expect(patches).toHaveLength(3);
  });

  it('explains that Keep yours cannot reopen a connection ended elsewhere', async () => {
    const opening = relationship(41, 1, { start_date: { year: 2024, month: 3 }, notes: 'Opening note' });
    const ended = relationship(41, 2, { status: 'ended', start_date: { year: 2024, month: 3 }, end_date: { year: 2026, month: 7 }, notes: 'Opening note' });
    const bodies: unknown[] = [];
    let reads = 0;
    const { client, controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (request.method === 'PATCH') {
        bodies.push(await request.json());
        if (bodies.length === 1) return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
        if (bodies.length === 2) throw new TypeError('Network unavailable');
        return Response.json(ended);
      }
      if (pathOf(request) === '/api/v1/person-relationships/41') {
        const data = ++reads === 1 ? opening : ended;
        return Response.json(data, { headers: { ETag: `"relationship-41-r${reads}"` } });
      }
      if (pathOf(request) === '/api/v1/people/7/relationships') {
        return Response.json({ relationships: [view(41, 'outgoing', 'is mentored by', 'Synthetic Learner', { relationship: ended })] });
      }
      throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
    });
    controller.relationships = [view(41, 'outgoing', 'is mentored by', 'Synthetic Learner', { relationship: opening })];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await rowAction('Edit…');
    await fireEvent.input(screen.getByRole('textbox', { name: 'Notes' }), { target: { value: 'Keep this note' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await screen.findByRole('button', { name: 'Keep yours' });

    expect(screen.getByText('To reopen it, add a new connection.')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Keep yours' })).toHaveProperty('disabled', true);
    await fireEvent.input(screen.getByRole('textbox', { name: 'End date' }), { target: { value: '2026-07' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Keep yours' }));
    await screen.findByText("Couldn't save this connection.");
    await fireEvent.input(screen.getByRole('textbox', { name: 'End date' }), { target: { value: '' } });
    expect(screen.getByRole('button', { name: 'Save' })).toHaveProperty('disabled', true);
    expect(screen.getByText('To reopen it, add a new connection.')).toBeDefined();
    await fireEvent.input(screen.getByRole('textbox', { name: 'End date' }), { target: { value: '2026-07' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Edit connection' })).toBeNull());
    expect(bodies).toEqual([{ notes: 'Keep this note' }, { end_date: '2026-07', notes: 'Keep this note' }, { end_date: '2026-07', notes: 'Keep this note' }]);
  });

  it('deletes an incoming connection using its underlying edge', async () => {
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
    controller.relationships = [view(41, 'incoming', 'mentors', 'Synthetic Guide')];
    render(RelationshipsTab, { client, controller, personID: 7 });

    await rowAction('Delete…');
    await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: /^Delete connection with/  })).toBeNull());
    const deletion = requests.find((request) => request.method === 'DELETE')!;
    expect(pathOf(deletion)).toBe('/api/v1/person-relationships/41');
    expect(controller.relationships).toEqual([]);
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

    expect(screen.queryByRole('button', { name: 'Delete locked relationship type' })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Edit locked relationship type' }));

    expect(await screen.findByText('Symmetry cannot be changed after creation.')).toBeDefined();
    expect(screen.getByText('Symmetric')).toBeDefined();
  });

});


it.each(['lost response', 'failed refresh'])('checks again after a %s', async failure => {
  let posts = 0, reads = 0;
  const { client, controller } = controllerWith(async input => {
    const request = requestOf(input), path = pathOf(request);
    if (path === '/api/v1/person-relationships' && request.method === 'POST') { posts++; if (failure === 'lost response') throw new TypeError('response lost'); return Response.json(relationship()); }
    if (path === '/api/v1/people/directory') return Response.json({ people: [directoryPerson(8)] });
    if (path === '/api/v1/people/7/relationships') return failure === 'failed refresh' && ++reads === 1 ? Response.json({ message: 'List unavailable' }, { status: 500 }) : Response.json({ relationships: posts ? [view(41, 'outgoing', 'is mentored by', 'Synthetic Two')] : [] });
    throw new Error(`unexpected ${request.method} ${path}`);
  });
  controller.relationshipTypes = [relationshipType()];
  render(RelationshipsTab, { client, controller, personID: 7 });
  await fireEvent.click(screen.getByRole('button', { name: 'Add connection…' }));
  await openTypeahead('Person');
  await fireEvent.mouseDown(await screen.findByRole('option', { name: 'Synthetic Two' }));
  await openTypeahead('Connection type');
  await fireEvent.mouseDown(screen.getByRole('treeitem', { name: 'Mentors' }));
  await fireEvent.click(screen.getAllByRole('button', { name: /^Add connection/ }).at(-1)!);
  if (failure === 'failed refresh') expect(await screen.findByText('The relationship was saved, but the list could not be refreshed.')).toBeDefined();
  await fireEvent.click(await screen.findByRole('button', { name: 'Check again' }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  expect(posts).toBe(1);
});

it.each(['connection', 'type'])('shows the saved %s after a delete conflict', async kind => {
  let reads = 0;
  const { client, controller } = controllerWith(async input => {
    const request = requestOf(input), path = pathOf(request);
    if (request.method === 'GET' && (path === '/api/v1/person-relationships/41' || path === '/api/v1/relationship-types/31')) {
      reads++;
      const item = kind === 'connection' ? relationship(41, reads, { notes: reads === 1 ? 'Old note' : 'New saved note' }) : relationshipType({ revision: reads, forward_label: reads === 1 ? 'mentors' : 'New saved label' });
      return Response.json(item, { headers: { ETag: `"${kind === 'connection' ? 'relationship-41' : 'relationship-type-31'}-r${reads}"` } });
    }
    if (request.method === 'DELETE') return Response.json({ error: 'relationship_revision_conflict', message: 'Changed' }, { status: 409 });
    if (path === '/api/v1/people/7/relationships') return Response.json({ relationships: [view(41, 'incoming', 'mentors', 'Synthetic Guide', { relationship: relationship(41, 2, { notes: 'New saved note' }) })] });
    throw new Error(`unexpected ${request.method} ${path}`);
  });
  controller.relationships = [view(41, 'incoming', 'mentors', 'Synthetic Guide')];
  controller.relationshipTypes = [relationshipType()];
  render(RelationshipsTab, { client, controller, personID: 7, catalogOnly: kind === 'type' });
  if (kind === 'connection') await rowAction('Delete…');
  else await fireEvent.click(screen.getByRole('button', { name: 'Delete mentor relationship type' }));
  if (kind === 'type') expect(screen.getByRole('dialog', { name: 'Delete mentors / is mentored by connection type?' })).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
  expect(await screen.findByText(kind === 'connection' ? 'New saved note' : 'New saved label')).toBeDefined();
  if (kind === 'connection') await waitFor(() => expect(controller.relationships[0].relationship.notes).toBe('New saved note'));
});
