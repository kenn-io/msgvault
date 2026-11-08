import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { valueProvenance } from './history-format';
import { requestOf, pathOf, organization, organizationProfile, envelope, richOrganizationProfile, controllerWith } from '../../directory/entity-fixtures.test-support';
import OrganizationEditor from './OrganizationEditor.svelte';

describe('OrganizationEditor', () => {
  it('retains the editor when a profile write conflicts after saving details', async () => {
    let revision = 1;
    const onDone = vi.fn();
    const profile = organizationProfile();
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (request.method === 'PATCH') {
        revision = 3;
        return Response.json({ ...profile.organization, name: 'Draft' }, { headers: { ETag: '"organization-21-r2"' } });
      }
      if (request.method === 'PUT') {
        expect(request.headers.get('If-Match')).toBe('"organization-21-r2"');
        return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
      }
      return Response.json(profile, { headers: { ETag: `"organization-21-r${revision}"` } });
    });
    render(OrganizationEditor, { controller, organization: organization(), onDone });
    await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization name' }), { target: { value: 'Draft' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization aliases' }), { target: { value: 'Draft alias' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByRole('button', { name: 'Keep yours' })).toBeDefined();
    expect(onDone).not.toHaveBeenCalled();
  });

  it('shows a changed organization before deleting its newer revision', async () => {
    let reads = 0;
    const tags: (string | null)[] = [];
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (pathOf(request).endsWith('/employments')) return Response.json({ employments: [] });
      if (request.method === 'DELETE') {
        tags.push(request.headers.get('If-Match'));
        return tags.length === 1 ? Response.json({ message: 'Changed elsewhere' }, { status: 412 }) : new Response(null, { status: 204 });
      }
      const profile = organizationProfile();
      return Response.json(++reads === 1 ? profile : { ...profile, organization: { ...profile.organization, name: 'New name' } }, { headers: { ETag: `"organization-21-r${reads}"` } });
    });
    render(OrganizationEditor, { controller, organization: organization() });
    await fireEvent.click(await screen.findByRole('button', { name: 'Delete organization…' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
    expect(await screen.findByText('New name')).toBeDefined();
    expect(tags).toHaveLength(1);
    await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(tags).toEqual(['"organization-21-r1"', '"organization-21-r2"']));
  });
  it('saves details, aliases, and categories with one action and retries only unsaved changes', async () => {
    let saved = organizationProfile();
    let patches = 0;
    let puts = 0;
    const onDone = vi.fn();
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (request.method === 'GET') return Response.json(saved, { headers: { ETag: '"organization-21-r1"' } });
      if (request.method === 'PATCH') {
        patches++;
        saved = { ...saved, organization: { ...saved.organization, name: 'Draft company' } };
        return Response.json(saved.organization, { headers: { ETag: '"organization-21-r2"' } });
      }
      if (request.method === 'PUT') {
        puts++;
        const body = await request.json();
        expect(body.names).toEqual(expect.arrayContaining([expect.objectContaining({ name: 'Draft alias' })]));
        expect(body.categories).toEqual(expect.arrayContaining([expect.objectContaining({ category: 'Research' })]));
        if (puts === 1) return Response.json({ message: 'Profile unavailable' }, { status: 400 });
        return Response.json(saved, { headers: { ETag: '"organization-21-r3"' } });
      }
      throw new Error(`unexpected ${request.method}`);
    });
    render(OrganizationEditor, { controller, organization: organization(), onDone });
    await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization name' }), { target: { value: 'Draft company' } });
    // Saved aliases show their field; empty categories wait behind their add link.
    expect(screen.queryByRole('button', { name: 'Add alias…' })).toBeNull();
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization aliases' }), { target: { value: 'Draft alias' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Add category…' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization categories' }), { target: { value: 'Research' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(puts).toBe(1));
    expect(onDone).not.toHaveBeenCalled();
    expect((await screen.findByRole('alert')).textContent).toContain("Couldn't save the aliases and categories.");
    expect(screen.getByRole('textbox', { name: 'Organization name' })).toHaveProperty('value', 'Draft company');
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    expect(patches).toBe(1);
    expect(puts).toBe(2);
  });

  it('shows the structured profile read-only under the fields, grouped by what each value is', async () => {
    const { controller } = controllerWith(async () => Response.json(richOrganizationProfile(1), { headers: { ETag: '"organization-21-r1"' } }));
    render(OrganizationEditor, { controller, organization: organization() });
    const email = await screen.findByRole('region', { name: 'Email' });
    expect(within(email).getByText('concurrent@example.test')).toBeDefined();
    expect(within(email).queryByRole('textbox')).toBeNull();
    // One Details on the Contact details heading holds every row's facts, with the vCard properties last.
    expect(within(email).queryByRole('button', { name: 'Details' })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Details' }));
    const address = screen.getByRole('region', { name: 'Address Suite 5, 12 Example Road' });
    expect(within(address).getByText('City').nextElementSibling?.textContent).toBe('Exampleton');
    await fireEvent.click(screen.getByText('Raw values'));
    expect(screen.getAllByText('vCard property').map(term => term.nextElementSibling?.textContent)).toContain('ADR');
  });

  it('says a provenance the rows share once, above them, and keeps a row\'s own only when it differs', async () => {
    const profile = richOrganizationProfile(1);
    profile.contact_points.push({ ...profile.contact_points[0], address_kind: 'phone', original_value: '+1 555 0100', envelope: envelope(47) });
    const { controller } = controllerWith(async () => Response.json(profile, { headers: { ETag: '"organization-21-r1"' } }));
    render(OrganizationEditor, { controller, organization: organization() });
    await screen.findByRole('region', { name: 'Email' });
    const shared = valueProvenance(profile.addresses[0].envelope);
    expect(screen.getAllByText(shared.text)).toHaveLength(1);
    expect(screen.getByText(shared.text).closest('.profile-heading p')?.textContent).toBe(`Contact details · ${shared.text}`);
    const row = (text: string) => screen.getByText(text).closest('.detail-row') as HTMLElement;
    expect(row('concurrent@example.test').querySelector('.description')).toBeNull();
    expect(row('Suite 5, 12 Example Road').querySelector('.description')).toBeNull();
    expect(row('+1 555 0100').querySelector('.description')?.textContent).toBe(valueProvenance(envelope(47)).text);
  });

  it('retries loading an organization from its inline error action', async () => {
    let reads = 0;
    const { controller } = controllerWith(async () => ++reads === 1
      ? Response.json({ message: 'Organization load unavailable' }, { status: 400 })
      : Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } }));
    render(OrganizationEditor, { controller, organization: organization() });
    const retry = await screen.findByRole('button', { name: 'Retry' });
    expect(screen.queryByRole('button', { name: 'Save' })).toBeNull();
    expect(screen.queryByRole('textbox', { name: 'Organization name' })).toBeNull();
    await fireEvent.click(retry);
    await waitFor(() => expect(screen.getByRole('textbox', { name: 'Organization aliases' })).toHaveProperty('value', 'Synthetic Works'));
    expect(screen.getByRole('button', { name: 'Save' })).toHaveProperty('disabled', false);
    expect(reads).toBe(2);
  });

  it('creates aliases and categories with the organization and reuses it after a profile failure', async () => {
    let creates = 0;
    let puts = 0;
    const created = { ...organization(), name: 'New org', primary_domain: null, description: null };
    const onDone = vi.fn();
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (request.method === 'POST') {
        creates++;
        expect(await request.json()).toMatchObject({ name: 'New org' });
        return Response.json(created, { status: 201, headers: { ETag: '"organization-21-r1"' } });
      }
      if (request.method === 'GET') return Response.json({ ...organizationProfile(), organization: created }, { headers: { ETag: '"organization-21-r1"' } });
      if (request.method === 'PUT') {
        puts++;
        const body = await request.json();
        expect(body.names).toContainEqual(expect.objectContaining({ name: 'New alias' }));
        expect(body.categories).toContainEqual(expect.objectContaining({ category: 'Research' }));
        if (puts === 1) return Response.json({ message: 'Profile unavailable' }, { status: 400 });
        return Response.json({ ...organizationProfile(), organization: created }, { headers: { ETag: '"organization-21-r2"' } });
      }
      throw new Error(`unexpected ${request.method}`);
    });
    render(OrganizationEditor, { controller, onDone });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization name' }), { target: { value: 'New org' } });
    // A new organization has neither yet, so each field waits behind its add link.
    expect(screen.queryByRole('textbox', { name: 'Organization aliases' })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Add alias…' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization aliases' }), { target: { value: 'New alias' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Add category…' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization categories' }), { target: { value: 'Research' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Add organization' }));
    await screen.findByText("Couldn't save the aliases and categories.", {exact:true});
    expect(onDone).not.toHaveBeenCalled();
    await fireEvent.click(screen.getByRole('button', { name: 'Add organization' }));
    await waitFor(() => expect(onDone).toHaveBeenCalledWith(created));
    expect(creates).toBe(1);
    expect(puts).toBe(2);
  });

  it.each(['details', 'aliases', 'categories'])('preserves untouched comma values when saving %s', async field => {
    const saved = {
      ...organizationProfile(),
      names: [{ ...organizationProfile().names[0], name: 'Example, Inc.' }],
      categories: [{ organization_id: 21, category: 'Research, development', category_normalized: 'research, development', envelope: envelope(40) }]
    };
    const onDone = vi.fn();
    const writes: Request[] = [];
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (request.method === 'GET') return Response.json(saved, { headers: { ETag: '"organization-21-r1"' } });
      writes.push(request.clone());
      return Response.json(request.method === 'PATCH' ? { ...saved.organization, name: 'Renamed' } : saved, { headers: { ETag: '"organization-21-r2"' } });
    });
    render(OrganizationEditor, { controller, organization: organization(), onDone });
    const name = await screen.findByRole('textbox', { name: 'Organization name' });
    await fireEvent.input(field === 'details' ? name : screen.getByRole('textbox', { name: `Organization ${field}` }), { target: { value: field === 'details' ? 'Renamed' : 'Changed' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    if (field === 'details') {
      expect(writes.map(request => request.method)).toEqual(['PATCH']);
    } else {
      const body = await writes.find(request => request.method === 'PUT')!.json();
      if (field === 'categories') expect(body.names).toEqual([expect.objectContaining({ name: 'Example, Inc.' })]);
      if (field === 'aliases') expect(body.categories).toEqual([expect.objectContaining({ category: 'Research, development' })]);
    }
  });

  it('keeps a saved comma alias whole when another alias is appended', async () => {
    const saved = { ...organizationProfile(), names: [{ ...organizationProfile().names[0], name: 'Example, Inc.' }] };
    const onDone = vi.fn();
    const puts: Request[] = [];
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (request.method === 'PUT') puts.push(request.clone());
      return Response.json(saved, { headers: { ETag: '"organization-21-r1"' } });
    });
    render(OrganizationEditor, { controller, organization: organization(), onDone });
    const aliases = await screen.findByRole('textbox', { name: 'Organization aliases' });
    await fireEvent.input(aliases, { target: { value: 'Example, Inc., Second' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    const body = await puts[0].json();
    expect(body.names.filter((item: { name_kind: string }) => item.name_kind === 'alias').map((item: { name: string }) => item.name)).toEqual(['Example, Inc.', 'Second']);
  });

  it('retains the organization draft and routes blocked creation refresh to its controller', async () => {
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/organizations' && request.method === 'POST') {
        return Response.json({ error: 'unavailable', message: 'response unavailable' }, { status: 503 });
      }
      if (path === '/api/v1/organizations') return Response.json({ organizations: [], total: 0, limit: 50, offset: 0 });
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    render(OrganizationEditor, { controller });

    const name = screen.getByRole('textbox', { name: 'Organization name' }) as HTMLInputElement;
    await fireEvent.input(name, { target: { value: 'Unknown Result Org' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Add organization' }));

    await screen.findByRole('alert');
    expect(name.value).toBe('Unknown Result Org');
    expect(screen.getByRole('button', { name: 'Add organization' })).toHaveProperty('disabled', true);
    const refresh = vi.spyOn(controller, 'refreshOrganizations');
    await fireEvent.click(screen.getByRole('button', { name: 'Check again' }));
    await waitFor(() => expect(refresh).toHaveBeenCalledOnce());
  });

  it('renames a retired organization without changing its status', async () => {
    const writes: Request[] = [];
    const onDone = vi.fn();
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (request.method === 'GET') return Response.json({ ...organizationProfile(2), organization: { ...organization(21, 2), retired_at: '2026-08-02T00:00:00Z' } }, { headers: { ETag: '"organization-21-r2"' } });
      writes.push(request);
      return Response.json({ ...organization(21, 3, 'Renamed Synthetic Org'), retired_at: '2026-08-02T00:00:00Z' }, { headers: { ETag: '"organization-21-r3"' } });
    });
    render(OrganizationEditor, { controller, organization: organization(), onDone });
    await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization name' }), { target: { value: 'Renamed Synthetic Org' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    expect(writes.map(request => request.method)).toEqual(['PATCH']);
    const body = await writes[0].json();
    expect(body).toMatchObject({ name: 'Renamed Synthetic Org' });
    expect(body.retired).toBeUndefined();
  });

  it('keeps read-only profile details and untouched lists from the saved conflict while writing the draft aliases', async () => {
    let reads = 0, puts = 0;
    const saved = richOrganizationProfile(2);
    const onDone = vi.fn();
    const bodies: Record<string, unknown>[] = [];
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (request.method === 'PATCH') return Response.json(saved.organization, { headers: { ETag: '"organization-21-r3"' } });
      if (request.method === 'PUT') {
        bodies.push(await request.json());
        return ++puts === 1 ? Response.json({ message: 'Changed elsewhere' }, { status: 412 }) : Response.json(saved);
      }
      return Response.json(++reads === 1 ? organizationProfile() : saved, { headers: { ETag: `"organization-21-r${reads}"` } });
    });
    render(OrganizationEditor, { controller, organization: organization(), onDone });
    await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization aliases' }), { target: { value: 'Draft alias' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Keep yours' }));
    await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    expect(bodies[1].names).toEqual(expect.arrayContaining([
      expect.objectContaining({ name: 'Synthetic Legal Ltd', name_kind: 'legal', source_ref: 'card-42', vcard_property: 'FN' }),
      expect.objectContaining({ name: 'Draft alias', name_kind: 'alias', source: 'user' }),
    ]));
    expect(bodies[1].categories).toEqual([expect.objectContaining({ category: 'Research', source_ref: 'card-42' })]);
    expect(bodies[1].contact_points).toEqual([expect.objectContaining({ original_value: 'concurrent@example.test', source_ref: 'card-42' })]);
    expect(bodies[1].media).toEqual([expect.objectContaining({ original_value: 'Concurrent logo', content_hash: 'sha256:concurrent' })]);
  });

  it.each([1, 200])('blocks deletion for %s past employment records and limits named people', async count => {
    const requests: Request[] = [];
    const { controller } = controllerWith(async input => {
      const request = requestOf(input); requests.push(request);
      const path = pathOf(request);
      if (path.endsWith('/employments')) return Response.json({ employments: Array.from({ length: count }, (_, i) => ({ person_id: Math.max(1, i), is_current: false })) });
      if (path.startsWith('/api/v1/people/')) return Response.json({ display_name: `Example Person ${path.split('/').at(-1)}` });
      return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    });
    const onOpenPerson = vi.fn(), onClose = vi.fn();
    render(OrganizationEditor, { controller, organization: organization(), onOpenPerson, onClose });
    await fireEvent.click(await screen.findByRole('button', { name: 'Delete organization…' }));
    // One sentence carries the count and the action, followed by the people holding the jobs.
    const first = await screen.findByText(count === 1 ? "Delete its job first. Ending it isn't enough." : "Delete its 200 or more jobs first. Ending them isn't enough.");
    expect(screen.queryByText(/^(1 job|At least 200 jobs)$/)).toBeNull();
    expect(screen.getByRole('button', { name: /^Back to / })).toBeDefined();
    expect(first.compareDocumentPosition(screen.getByText('Example Person 1')) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    if (count === 200) expect(screen.getByText('and at least 196 more')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Example Person 1' }));
    expect(onClose).toHaveBeenCalled();
    expect(onOpenPerson).toHaveBeenCalledWith(1);
    const query = new URL(requests.find(request => pathOf(request).endsWith('/employments'))!.url).searchParams;
    expect(query.get('limit')).toBe('200');
    expect(query.has('current_only')).toBe(false);
    expect(requests.filter(request => pathOf(request).startsWith('/api/v1/people/'))).toHaveLength(Math.min(count, 3));
  });

  it('retains the draft through failed deletion checks, retry, and a server refusal', async () => {
    let checks = 0;
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (pathOf(request).endsWith('/employments')) {
        if (++checks === 1) return Response.json({ message: 'Check failed' }, { status: 400 });
        return Response.json({ employments: checks > 2 ? [{ person_id: 7, is_current: false }] : [] });
      }
      if (pathOf(request) === '/api/v1/people/7') return Response.json({ display_name: 'Example Person' });
      if (request.method === 'DELETE') return Response.json({ error: 'organization_has_employments', message: 'Employment added elsewhere' }, { status: 409 });
      return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    });
    render(OrganizationEditor, { controller, organization: organization() });
    await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization name' }), { target: { value: 'Kept draft' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Delete organization…' }));
    await screen.findByText("Couldn't check jobs.");
    expect(screen.getByRole('button', { name: /^Back to / })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Check again' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Delete' })).toHaveProperty('disabled', false));
    await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    await screen.findByText("Delete its job first. Ending it isn't enough.");
    expect(checks).toBe(3);
    expect(screen.queryByRole('region', { name: 'Current saved organization' })).toBeNull();
    expect(screen.getByRole('button', { name: /^Back to / })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: /^Back to / }));
    expect(screen.getByRole('textbox', { name: 'Organization name' })).toHaveProperty('value', 'Kept draft');
    expect(screen.getByRole('button', { name: 'Save' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Keep yours' })).toBeNull();
  });

  it('keeps an editor open during submission', async () => {
    let resolveCreate!: (response: Response) => void;
    const pending = new Promise<Response>((resolve) => { resolveCreate = resolve; });
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (pathOf(request) === '/api/v1/organizations' && request.method === 'POST') return pending;
      throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
    }));
    const onDone = vi.fn();
    const onClose = vi.fn();
    const view = render(OrganizationEditor, { controller, onDone, onClose });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization name' }), { target: { value: 'Pending Org' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Add organization' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(screen.getByRole('dialog', { name: 'Add organization' })).toBeDefined();
    expect(onClose).not.toHaveBeenCalled();
    resolveCreate(new Response(JSON.stringify(organization(22, 1, 'Pending Org')), { status: 201, headers: { ETag: '"organization-22-r1"' } }));
    await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    view.unmount();
  });
});

it('keeps unnamed holders openable as links in a blocked deletion', async () => {
  const onOpenPerson = vi.fn();
  const { controller } = controllerWith(async input => {
    const path = pathOf(requestOf(input));
    if (path.endsWith('/employments')) return Response.json({ employments: [{ person_id: 8, is_current: false }] });
    if (path === '/api/v1/people/8') return Response.json({ display_name: null });
    if (path === '/api/v1/organizations/21') return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    throw new Error(`unexpected ${path}`);
  });
  render(OrganizationEditor, { controller, organization: organization(), onOpenPerson });
  await fireEvent.click(await screen.findByRole('button', { name: 'Delete organization…' }));
  const open = await screen.findByRole('button', { name: 'Unnamed person' });
  expect(open.hasAttribute('data-link')).toBe(true);
  expect(screen.getByText("Delete its job first. Ending it isn't enough.")).toBeDefined();
  // Their job isn't this person's, so the row's action says it opens them to delete it.
  const action = screen.getByRole('button', { name: 'Open to delete Unnamed person’s job' });
  expect(action.textContent).toContain('Open to delete…');
  await fireEvent.click(open);
  expect(onOpenPerson).toHaveBeenCalledWith(8);
  await fireEvent.click(action);
  expect(onOpenPerson).toHaveBeenCalledTimes(2);
});

it.each(['legal name', 'retirement'])('retains the saved %s in organization conflicts', async fact => {
  let reads = 0;
  const saved = richOrganizationProfile(2);
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'PATCH') return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
    return Response.json(++reads === 1 ? organizationProfile() : { ...saved, organization: { ...saved.organization, retired_at: '2026-11-01T00:00:00Z' } }, { headers: { ETag: `"organization-21-r${reads}"` } });
  });
  render(OrganizationEditor, { controller, organization: organization() });
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization name' }), { target: { value: 'Draft name' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  const alert = (await screen.findByRole('alert')).closest('.inline-notice') as HTMLElement;
  expect(within(alert).getByText(fact === 'legal name' ? 'Legal names' : 'Status', { selector: 'dt' }).nextElementSibling?.textContent).toContain(fact === 'legal name' ? 'Synthetic Legal Ltd' : 'Retired');
});


it.each(['names', 'categories'])('keeps saved %s metadata inspectable after a conflict', async section => {
  let reads = 0;
  const saved = richOrganizationProfile(2);
  saved.names[0].envelope.vcard = { ...saved.names[0].envelope.vcard, altid: 'legal-name-variant' } as typeof saved.names[0]['envelope']['vcard'];
  saved.categories[0].envelope.vcard = { ...saved.categories[0].envelope.vcard, altid: 'category-variant' } as typeof saved.categories[0]['envelope']['vcard'];
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'PATCH') return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
    return Response.json(++reads === 1 ? organizationProfile() : saved, { headers: { ETag: `"organization-21-r${reads}"` } });
  });
  render(OrganizationEditor, { controller, organization: organization() });
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization name' }), { target: { value: 'Draft name' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  const alert = (await screen.findByRole('alert')).closest('.inline-notice') as HTMLElement;
  const value = within(alert).getByRole('region', { name: section === 'names' ? 'Name Synthetic Legal Ltd' : 'Category Research' });
  expect(within(value).getByText('Source reference').nextElementSibling?.textContent).toBe('card-42');
  expect(within(value).getByText('Confidence').nextElementSibling?.textContent).toBe('87%');
  expect(within(alert).getByText(section === 'names' ? 'legal-name-variant' : 'category-variant')).toBeDefined();
});

it('retries a failed DELETE directly and keeps Delete available', async () => {
  let deletes = 0;
  const onDone = vi.fn();
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (pathOf(request).endsWith('/employments')) return Response.json({ employments: [] });
    if (request.method === 'DELETE') return ++deletes === 1
      ? Response.json({ message: 'Temporarily unavailable' }, { status: 503 })
      : new Response(null, { status: 204 });
    return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
  });
  render(OrganizationEditor, { controller, organization: organization(), onDone });
  await fireEvent.click(await screen.findByRole('button', { name: 'Delete organization…' }));
  await fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
  await screen.findByText("Couldn't delete this organization.");
  expect(screen.getByRole('button', { name: 'Delete' })).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
  expect(deletes).toBe(2);
});

it('Keep yours writes the changed aliases whole and keeps a concurrent rename', async () => {
  let reads = 0;
  let puts = 0;
  const bodies: { names: { name: string }[] }[] = [];
  const onDone = vi.fn();
  const patches: unknown[] = [];
  const opening = organizationProfile();
  const newer = { ...opening, organization: organization(21, 2, 'Concurrent name'), names: [...opening.names, { ...opening.names[0], name: 'Concurrent alias' }] };
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'PATCH') { patches.push(await request.json()); return Response.json(opening.organization, { headers: { ETag: '"organization-21-r3"' } }); }
    if (request.method === 'PUT') {
      if (++puts === 1) return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
      bodies.push(await request.json());
      return Response.json(opening, { headers: { ETag: '"organization-21-r4"' } });
    }
    return Response.json(++reads === 1 ? opening : newer, { headers: { ETag: `"organization-21-r${reads === 1 ? 1 : 2}"` } });
  });
  render(OrganizationEditor, { controller, organization: organization(), onDone });
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization aliases' }), { target: { value: 'Draft alias' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  await fireEvent.click(await screen.findByRole('button', { name: 'Keep yours' }));
  await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
  expect(bodies[0].names.map((item: { name: string }) => item.name)).toEqual(['Draft alias']);
  expect(patches).toEqual([]);
});

it('Keep yours keeps fields and value metadata the person did not change', async () => {
  let reads = 0;
  const writes: { method: string; tag: string | null; body: Record<string, unknown> }[] = [];
  const onDone = vi.fn();
  const opening = organizationProfile();
  // Elsewhere: a new domain, and the alias the person keeps re-imported with new provenance.
  const newer = {
    ...opening, organization: { ...opening.organization, revision: 2, primary_domain: 'concurrent.example' },
    names: [{ ...opening.names[0], envelope: envelope(31, { source: 'vcard_import', source_ref: 'resource-current' }) }],
  };
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'PATCH' || request.method === 'PUT') {
      writes.push({ method: request.method, tag: request.headers.get('If-Match'), body: await request.json() });
      if (request.headers.get('If-Match') === '"organization-21-r1"') return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
      return request.method === 'PATCH' ? Response.json(newer.organization, { headers: { ETag: '"organization-21-r3"' } }) : Response.json(newer);
    }
    return Response.json(++reads === 1 ? opening : newer, { headers: { ETag: `"organization-21-r${reads === 1 ? 1 : 2}"` } });
  });
  render(OrganizationEditor, { controller, organization: organization(), onDone });
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization name' }), { target: { value: 'Draft name' } });
  await fireEvent.input(screen.getByRole('textbox', { name: 'Organization aliases' }), { target: { value: 'Synthetic Works, Draft alias' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  const alert = (await screen.findByRole('alert')).closest('.inline-notice') as HTMLElement;
  const table = within(alert).getByRole('table', { name: 'What changed' });
  expect(within(table).getAllByRole('rowheader').map(cell => cell.textContent)).toEqual(['Name', 'Aliases']);
  await fireEvent.click(screen.getByRole('button', { name: 'Keep yours' }));
  await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
  const [patch, put] = writes.slice(1);
  expect(patch).toMatchObject({ method: 'PATCH', tag: '"organization-21-r2"', body: { name: 'Draft name', primary_domain: 'concurrent.example' } });
  expect(put).toMatchObject({ method: 'PUT', tag: '"organization-21-r3"' });
  expect(put.body.names).toEqual([
    expect.objectContaining({ name: 'Synthetic Works', source: 'vcard_import', source_ref: 'resource-current' }),
    expect.objectContaining({ name: 'Draft alias', source: 'user' }),
  ]);
});

it('retries the alias replacement accepted with Keep yours after the profile write fails', async () => {
  let reads = 0;
  const puts: { tag: string | null; names: string[] }[] = [];
  const onDone = vi.fn();
  const opening = organizationProfile();
  const newer = { ...opening, organization: organization(21, 2, 'Concurrent name'), names: [...opening.names, { ...opening.names[0], name: 'Concurrent alias' }] };
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'PATCH') return Response.json(opening.organization, { headers: { ETag: '"organization-21-r3"' } });
    if (request.method === 'PUT') {
      const body = await request.json() as { names: { name: string; name_kind: string }[] };
      puts.push({ tag: request.headers.get('If-Match'), names: body.names.filter(item => item.name_kind === 'alias').map(item => item.name) });
      if (puts.length === 1) return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
      if (puts.length === 2) return Response.json({ message: 'Profile unavailable' }, { status: 503 });
      return Response.json(opening, { headers: { ETag: '"organization-21-r4"' } });
    }
    return Response.json(++reads === 1 ? opening : newer, { headers: { ETag: `"organization-21-r${reads === 1 ? 1 : 2}"` } });
  });
  render(OrganizationEditor, { controller, organization: organization(), onDone });
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization aliases' }), { target: { value: 'Draft alias' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  await fireEvent.click(await screen.findByRole('button', { name: 'Keep yours' }));
  await screen.findByText("Couldn't save the aliases and categories.");
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
  expect(puts).toEqual([
    { tag: '"organization-21-r1"', names: ['Draft alias'] },
    { tag: '"organization-21-r2"', names: ['Draft alias'] },
    { tag: '"organization-21-r2"', names: ['Draft alias'] },
  ]);
});

it('preserves the edit baseline after Back from a conflicted delete', async () => {
  let reads = 0;
  const tags: (string | null)[] = [];
  const opening = organizationProfile();
  const newer = { ...opening, organization: { ...opening.organization, revision: 2, primary_domain: 'concurrent.example' } };
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (pathOf(request).endsWith('/employments')) return Response.json({ employments: [] });
    if (request.method === 'DELETE') return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
    if (request.method === 'PATCH') {
      tags.push(request.headers.get('If-Match'));
      return request.headers.get('If-Match') === '"organization-21-r1"'
        ? Response.json({ message: 'Changed elsewhere' }, { status: 412 })
        : Response.json({ ...newer.organization, ...await request.json() });
    }
    return Response.json(++reads === 1 ? opening : newer, { headers: { ETag: reads === 1 ? '"organization-21-r1"' : '"organization-21-r2"' } });
  });
  render(OrganizationEditor, { controller, organization: organization() });
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization name' }), { target: { value: 'Draft name' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Delete organization…' }));
  await fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
  await waitFor(() => expect(reads).toBe(2));
  await screen.findByText('This organization changed since you opened it.');
  await fireEvent.click(screen.getByRole('button', { name: /^Back to / }));
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  await waitFor(() => expect(tags).toHaveLength(1));
  expect(tags).toEqual(['"organization-21-r1"']);
  expect(await screen.findByRole('button', { name: 'Keep yours' })).toBeDefined();
});

it('shows a failed holder profile read as a failed check and retries it', async () => {
  let reads = 0;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (pathOf(request).endsWith('/employments')) return Response.json({ employments: [{ person_id: 7, is_current: false }] });
    if (pathOf(request) === '/api/v1/people/7') return ++reads === 1
      ? Response.json({ message: 'Profile unavailable' }, { status: 503 })
      : Response.json({ display_name: 'Synthetic Person' });
    return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
  });
  render(OrganizationEditor, { controller, organization: organization() });
  await fireEvent.click(await screen.findByRole('button', { name: 'Delete organization…' }));
  await screen.findByText("Couldn't check jobs.");
  expect(screen.queryByText('Unnamed person')).toBeNull();
  expect(screen.queryByRole('button', { name: 'Delete' })).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Check again' }));
  expect(await screen.findByText('Synthetic Person')).toBeDefined();
});

it.each(['failure', 'conflict'])('keeps the concurrent domain after a partial save and another %s', async outcome => {
  let reads = 0;
  let puts = 0;
  const patches: Record<string, unknown>[] = [];
  const opening = organizationProfile();
  const newer = { ...opening, organization: { ...opening.organization, revision: 2, primary_domain: 'concurrent.example' } };
  const saved = { ...newer, organization: { ...newer.organization, revision: 3, name: 'Draft name' } };
  const onDone = vi.fn();
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'PATCH') {
      const body = await request.json();
      patches.push(body);
      if (patches.length === 1) return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
      return Response.json({ ...saved.organization, ...body }, { headers: { ETag: '"organization-21-r3"' } });
    }
    if (request.method === 'PUT') {
      if (++puts === 1) return Response.json({ message: 'Try again' }, { status: outcome === 'failure' ? 503 : 412 });
      return Response.json(saved, { headers: { ETag: '"organization-21-r4"' } });
    }
    const profile = ++reads === 1 ? opening : reads === 2 ? newer : saved;
    return Response.json(profile, { headers: { ETag: `"organization-21-r${reads}"` } });
  });
  render(OrganizationEditor, { controller, organization: organization(), onDone });
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization name' }), { target: { value: 'Draft name' } });
  await fireEvent.input(screen.getByRole('textbox', { name: 'Organization aliases' }), { target: { value: 'Draft alias' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  await fireEvent.click(await screen.findByRole('button', { name: 'Keep yours' }));
  await fireEvent.click(await screen.findByRole('button', { name: outcome === 'failure' ? 'Retry' : 'Keep yours' }));
  await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
  expect(patches.every((body, index) => index === 0 || body.primary_domain === 'concurrent.example')).toBe(true);
});

it('shows the retirement date in a conflict', async () => {
  let reads = 0;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'PATCH') return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
    const profile = organizationProfile(++reads);
    return Response.json(reads === 1 ? profile : { ...profile, organization: { ...profile.organization, retired_at: '2025-01-06T00:00:00Z' } }, { headers: { ETag: `"organization-21-r${reads}"` } });
  });
  render(OrganizationEditor, { controller, organization: organization() });
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization name' }), { target: { value: 'Draft name' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  const alert = (await screen.findByRole('alert')).closest('.inline-notice') as HTMLElement;
  expect(within(alert).getByText('Status', { selector: 'dt' }).nextElementSibling?.textContent).toContain('Jan 6, 2025');
});
