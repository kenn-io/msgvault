import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { valueProvenance } from './history-format';
import { requestOf, pathOf, organization, employment, organizationProfile, envelope, richOrganizationProfile, controllerWith } from '../../directory/entity-fixtures.test-support';
import OrganizationEditor from './OrganizationEditor.svelte';

describe('OrganizationEditor', () => {
  it.each(['organization', 'profile', 'edit elsewhere'] as const)('keeps the other draft after saving %s and sends the version it made', async first => {
    let revision = 1;
    let reads = 0;
    let profile = organizationProfile();
    const writes: Request[] = [];
    const onDone = vi.fn();
    const etag = () => ({ ETag: `"organization-21-r${revision}"` });
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (request.method === 'GET') { reads++; return Response.json(profile, { headers: etag() }); }
      writes.push(request);
      if (request.headers.get('If-Match') !== etag().ETag) return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
      revision++;
      if (request.method === 'PATCH') profile = { ...profile, organization: organization(21, revision, 'Lumen Grove Ltd') };
      else profile = { ...profile, names: [{ ...profile.names[0], name: 'LG' }] };
      return Response.json(request.method === 'PATCH' ? profile.organization : profile, { headers: etag() });
    });
    render(OrganizationEditor, { controller, organization: organization(), onDone });
    await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization aliases' }), { target: { value: 'LG' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization name' }), { target: { value: 'Lumen Grove Ltd' } });
    await fireEvent.click(screen.getByRole('button', { name: first === 'profile' ? 'Save aliases and categories' : 'Save organization' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    await waitFor(() => expect(document.querySelector('.editor')?.getAttribute('aria-busy')).toBe('false'));
    expect(onDone).not.toHaveBeenCalled();
    expect(screen.getByRole('textbox', { name: 'Organization aliases' })).toHaveProperty('value', 'LG');
    expect(screen.getByRole('textbox', { name: 'Organization name' })).toHaveProperty('value', 'Lumen Grove Ltd');
    if (first !== 'profile') await screen.findByRole('dialog', { name: 'Lumen Grove Ltd' });
    if (first === 'edit elsewhere') revision++;
    await fireEvent.click(screen.getByRole('button', { name: first === 'profile' ? 'Save organization' : 'Save aliases and categories' }));
    await waitFor(() => expect(writes).toHaveLength(2));
    expect(writes[1].headers.get('If-Match')).toBe('"organization-21-r2"');
    if (first === 'edit elsewhere') {
      expect(await screen.findByText('This organization changed since you opened it.')).toBeDefined();
      expect(onDone).not.toHaveBeenCalled();
      return;
    }
    await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    expect(reads).toBe(1);
    expect(writes.map(request => request.method)).toEqual(first === 'organization' ? ['PATCH', 'PUT'] : ['PUT', 'PATCH']);
    const patch = writes.find(request => request.method === 'PATCH')!;
    const put = writes.find(request => request.method === 'PUT')!;
    expect(await patch.json()).toMatchObject({ name: 'Lumen Grove Ltd' });
    expect((await put.json()).names).toEqual(expect.arrayContaining([expect.objectContaining({ name: 'LG' })]));
  });

  it('can close while the organization is still loading', async () => {
    const { controller } = controllerWith(() => new Promise<Response>(() => undefined));
    const onClose = vi.fn();
    render(OrganizationEditor, { controller, organization: organization(), onClose });
    await screen.findByText('Loading organization…');
    await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onClose).toHaveBeenCalledOnce();
  });

  it('renames a retired organization with an untouched comma alias', async () => {
    const profile = organizationProfile(2);
    Object.assign(profile.organization, { retired_at: '2026-08-02T00:00:00Z' });
    profile.names[0].name = 'Example, Inc.';
    const onDone = vi.fn();
    const writes: Request[] = [];
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (request.method === 'GET') return Response.json(profile, { headers: { ETag: '"organization-21-r2"' } });
      writes.push(request);
      return Response.json({ ...organization(21, 3, 'Renamed'), retired_at: '2026-08-02T00:00:00Z' });
    });
    render(OrganizationEditor, { controller, organization: organization(), onDone });
    await screen.findByDisplayValue('Example, Inc.');
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization name' }), { target: { value: 'Renamed' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
    await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    expect(writes.map(request => request.method)).toEqual(['PATCH']);
    const body = await writes[0].json();
    expect(body).toMatchObject({ name: 'Renamed' });
    expect(body.retired).toBeUndefined();
  });

  it('shows the structured profile read-only under the fields, grouped by what each value is', async () => {
    const profile = richOrganizationProfile(1);
    profile.contact_points.push({ ...profile.contact_points[0], address_kind: 'impp', original_value: 'mira@example.test', envelope: envelope(49) });
    profile.contact_points.push({ ...profile.contact_points[0], address_kind: 'phone', original_value: '+1 555 0100', envelope: envelope(47) });
    profile.contact_points.push({ ...profile.contact_points[0], original_value: 'second@example.test', envelope: envelope(48, { source: 'vcard_import', updated_at: '2026-08-01T15:00:00Z' }) as typeof profile.contact_points[0]['envelope'] });
    profile.contact_points.push({ ...profile.contact_points[0], original_value: 'third@example.test' });
    const { controller } = controllerWith(async () => Response.json(profile, { headers: { ETag: '"organization-21-r1"' } }));
    render(OrganizationEditor, { controller, organization: organization() });
    const email = await screen.findByRole('region', { name: 'Email' });
    expect(within(email).getByText('concurrent@example.test')).toBeDefined();
    expect(await screen.findByRole('region', { name: 'Messaging' })).toBeDefined();
    expect(screen.getByText('mira@example.test')).toBeDefined();
    expect(within(email).queryByRole('textbox')).toBeNull();
    const shared = valueProvenance(profile.addresses[0].envelope);
    expect(screen.getAllByText(shared.text)).toHaveLength(1);
    expect(screen.getByText(shared.text).closest('.profile-heading p')?.textContent).toBe(`Contact details · ${shared.text}`);
    const row = (text: string) => screen.getByText(text).closest('.detail-row') as HTMLElement;
    expect(row('concurrent@example.test').querySelector('.description')).toBeNull();
    expect(row('Suite 5, 12 Example Road').querySelector('.description')).toBeNull();
    expect(row('+1 555 0100').querySelector('.description')?.textContent).toBe(valueProvenance(envelope(47)).text);
    const first = valueProvenance(profile.contact_points[0].envelope);
    expect(screen.getByText(first.text, { selector: '.profile-heading .provenance' }).getAttribute('title')).toBe(first.title);
    const second = row('second@example.test');
    expect(second.querySelector('.description')?.getAttribute('title')).toBe(valueProvenance(profile.contact_points[3].envelope).title);
    // One Details on the Contact details heading holds every row's facts, with the vCard properties last.
    expect(within(email).queryByRole('button', { name: 'Details' })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Details' }));
    const address = screen.getByRole('region', { name: 'Address Suite 5, 12 Example Road' });
    expect(within(address).getByText('City').nextElementSibling?.textContent).toBe('Exampleton');
    // The value shows once, its facts under its row; Email's heading already names its kind, an address's kind adds to it.
    expect(screen.getAllByText('concurrent@example.test')).toHaveLength(1);
    expect(within(address).getByText('Kind').nextElementSibling?.textContent).toBe('Work');
    expect(within(screen.getByRole('region', { name: 'Email concurrent@example.test' })).queryByText('Kind')).toBeNull();
    await fireEvent.click(screen.getByText('Raw values'));
    expect(screen.getAllByText('vCard property').map(term => term.nextElementSibling?.textContent)).toContain('ADR');
  });

  it('retries loading an organization from its inline error action', async () => {
    let reads = 0;
    const { controller } = controllerWith(async () => ++reads === 1
      ? Response.json({ message: 'Organization load unavailable' }, { status: 400 })
      : Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } }));
    render(OrganizationEditor, { controller, organization: organization() });
    const retry = await screen.findByRole('button', { name: 'Retry' });
    expect(screen.queryByRole('button', { name: 'Save organization' })).toBeNull();
    expect(screen.queryByRole('textbox', { name: 'Organization name' })).toBeNull();
    await fireEvent.click(retry);
    await waitFor(() => expect(screen.getByRole('textbox', { name: 'Organization aliases' })).toHaveProperty('value', 'Synthetic Works'));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization name' }), { target: { value: 'Synthetic Org 2' } });
    expect(screen.getByRole('button', { name: 'Save organization' })).toHaveProperty('disabled', false);
    expect(reads).toBe(2);
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
    // A new organization gets aliases and categories once it exists.
    expect(screen.queryByRole('button', { name: 'Add alias…' })).toBeNull();
    await fireEvent.input(name, { target: { value: 'Unknown Result Org' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Add organization' }));

    await screen.findByRole('alert');
    expect(name.value).toBe('Unknown Result Org');
    expect(screen.getByRole('button', { name: 'Add organization' })).toHaveProperty('disabled', true);
    const refresh = vi.spyOn(controller, 'refreshOrganizations');
    await fireEvent.click(screen.getByRole('button', { name: 'Check again' }));
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });

  it.each([
    { label: 'one job', count: 1 },
    { label: '200 jobs', count: 200 },
    { label: 'an unnamed holder', count: 1, personID: 8, displayName: null },
    { label: 'two jobs held by one person', count: 2, personID: 8, displayName: 'Synthetic Person', titles: ['Engineer', 'Advisor'] },
  ])('blocks deletion for $label and links holders', async scenario => {
    const { count } = scenario;
    const requests: Request[] = [];
    const { controller } = controllerWith(async input => {
      const request = requestOf(input); requests.push(request);
      const path = pathOf(request);
      if (path.endsWith('/employments')) return Response.json({ employments: scenario.titles
        ? scenario.titles.map((title, index) => employment(index + 11, 1, { person_id: scenario.personID, title, is_current: false }))
        : Array.from({ length: count }, (_, i) => employment(i + 1, 1, { person_id: scenario.personID ?? Math.max(1, i), is_current: false })) });
      if (path.startsWith('/api/v1/people/')) return Response.json({ display_name: 'displayName' in scenario ? scenario.displayName : `Example Person ${path.split('/').at(-1)}` });
      return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    });
    const onOpenPerson = vi.fn(), onClose = vi.fn();
    render(OrganizationEditor, { controller, organization: organization(), onOpenPerson, onClose });
    await fireEvent.click(await screen.findByRole('button', { name: 'Delete organization…' }));
    // One sentence carries the count and the action, followed by the people holding the jobs.
    const first = await screen.findByText(count === 1 ? "Delete its job first. Ending it isn't enough." : count === 200 ? "Delete its 200 or more jobs first. Ending them isn't enough." : "Delete its 2 jobs first. Ending them isn't enough.");
    expect(screen.getByRole('button', { name: /^Back to / })).toBeDefined();
    const holderName = scenario.displayName === null ? 'Unnamed person' : scenario.displayName ?? 'Example Person 1';
    const holders = screen.getAllByRole('button', { name: holderName });
    expect(first.compareDocumentPosition(holders[0]) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    if (count === 200) expect(screen.getByText('and at least 196 more')).toBeDefined();
    for (const title of scenario.titles ?? []) expect(screen.getByText(title)).toBeDefined();
    if (scenario.displayName === null) expect(holders[0].hasAttribute('data-link')).toBe(true);
    await fireEvent.click(holders[0]);
    expect(onClose).toHaveBeenCalled();
    expect(onOpenPerson).toHaveBeenCalledWith(scenario.personID ?? 1);
    const query = new URL(requests.find(request => pathOf(request).endsWith('/employments'))!.url).searchParams;
    expect(query.get('limit')).toBe('200');
    expect(query.has('current_only')).toBe(false);
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

it.each(['failed request', 'revision conflict'] as const)('handles a delete %s', async outcome => {
  let deletes = 0;
  let reads = 0;
  const onDone = vi.fn();
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (pathOf(request).endsWith('/employments')) return Response.json({ employments: [] });
    if (request.method === 'DELETE') {
      deletes++;
      if (outcome === 'revision conflict') return Response.json({ error: 'organization_revision_conflict', message: 'Changed' }, { status: 409 });
      return deletes === 1 ? Response.json({ message: 'Temporarily unavailable' }, { status: 503 }) : new Response(null, { status: 204 });
    }
    reads++;
    return Response.json(organizationProfile(reads, reads < 3 ? 'Synthetic Org' : 'New saved name'), { headers: { ETag: `"organization-21-r${reads}"` } });
  });
  render(OrganizationEditor, { controller, organization: organization(), onDone });
  await fireEvent.click(await screen.findByRole('button', { name: 'Delete organization…' }));
  await fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
  if (outcome === 'failed request') {
    await screen.findByText("Couldn't delete this organization.");
    expect(screen.getByRole('button', { name: 'Delete' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    expect(deletes).toBe(2);
  } else {
    expect(await screen.findByText('New saved name')).toBeDefined();
    expect(deletes).toBe(1);
  }
});

it('shows a failed holder profile read as a failed check and retries it', async () => {
  let reads = 0;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (pathOf(request).endsWith('/employments')) return Response.json({ employments: [employment(11, 1, { person_id: 7, is_current: false })] });
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

it.each(['organization save', 'aliases save'] as const)('shows the latest notice after a failed %s', async firstFailure => {
  const methods: string[] = [];
  const onDone = vi.fn();
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'GET') return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    methods.push(request.method);
    if (firstFailure === 'organization save') {
      return request.method === 'PATCH'
        ? Response.json({ message: 'Changed elsewhere' }, { status: 412 })
        : Response.json({ message: 'Disk full' }, { status: 500 });
    }
    return request.method === 'PUT'
      ? methods.filter(method => method === 'PUT').length === 1 ? Response.json({ message: 'Profile unavailable' }, { status: 503 }) : Response.json(organizationProfile(2), { headers: { ETag: '"organization-21-r2"' } })
      : Response.json({ message: 'primary domain "bad domain" is not a valid domain' }, { status: 400 });
  });
  render(OrganizationEditor, { controller, organization: organization(), onDone });
  const aliases = await screen.findByRole('textbox', { name: 'Organization aliases' });
  const domain = screen.getByRole('textbox', { name: 'Organization domain' });
  if (firstFailure === 'organization save') {
    await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization name' }), { target: { value: 'Draft name' } });
    await fireEvent.input(aliases, { target: { value: 'Synthetic Works, Draft alias' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
    await screen.findByText('This organization changed since you opened it.');
    await fireEvent.click(screen.getByRole('button', { name: 'Save aliases and categories' }));
    await screen.findByText("Couldn't save the aliases and categories.");
    expect(screen.queryByText('This organization changed since you opened it.')).toBeNull();
    expect(screen.getByRole('button', { name: 'Save organization' })).toBeDefined();
  } else {
    await fireEvent.input(aliases, { target: { value: 'Draft alias' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save aliases and categories' }));
    await screen.findByText("Couldn't save the aliases and categories.");
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    expect(methods).toEqual(['PUT', 'PUT']);
    await fireEvent.input(domain, { target: { value: 'bad domain' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
    await screen.findByText("Couldn't save this organization.");
    expect(screen.queryByText("Couldn't save the aliases and categories.")).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(methods).toEqual(['PUT', 'PUT', 'PATCH', 'PATCH']));
  }
});

it('retries a failed Keep yours with the saved version', async () => {
  const tags: (string | null)[] = [];
  let reads = 0;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'GET') { reads++; return Response.json({ ...organizationProfile(reads), organization: organization(21, reads, reads === 1 ? undefined : 'Renamed elsewhere') }, { headers: { ETag: `"organization-21-r${reads}"` } }); }
    tags.push(request.headers.get('If-Match'));
    return tags.length === 1 ? Response.json({ message: 'Changed elsewhere' }, { status: 412 }) : Response.json({ message: 'Unavailable' }, { status: 503 });
  });
  render(OrganizationEditor, { controller, organization: organization() });
  const domain = await screen.findByRole('textbox', { name: 'Organization domain' }) as HTMLInputElement;
  await fireEvent.input(domain, { target: { value: 'new.example' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
  await fireEvent.click(await screen.findByRole('button', { name: 'Keep yours' }));
  await screen.findByText("Couldn't save this organization.");
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  await waitFor(() => expect(tags).toEqual(['"organization-21-r1"', '"organization-21-r2"', '"organization-21-r2"']));
});

it.each(['retirement', 'media', 'facts', 'names', 'categories'])('shows saved %s in an organization conflict', async section => {
  let reads = 0;
  const saved = richOrganizationProfile(2);
  saved.names[0].envelope.vcard = { ...saved.names[0].envelope.vcard, altid: 'legal-name-variant' } as typeof saved.names[0]['envelope']['vcard'];
  saved.categories[0].envelope.vcard = { ...saved.categories[0].envelope.vcard, altid: 'category-variant' } as typeof saved.categories[0]['envelope']['vcard'];
  if (section === 'retirement') Object.assign(saved.organization, { retired_at: '2025-01-06T00:00:00Z' });
  if (section === 'media') Object.assign(saved.media[0], { original_value: 'https://example.org/logo.png', uri: 'https://example.org/logo.png' });
  const { controller } = controllerWith(async input => {
    if (requestOf(input).method === 'PATCH') return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
    return Response.json(++reads === 1 ? organizationProfile() : saved, { headers: { ETag: `"organization-21-r${reads}"` } });
  });
  render(OrganizationEditor, { controller, organization: organization() });
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization name' }), { target: { value: 'Draft name' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
  const alert = (await screen.findByText('This organization changed since you opened it.')).closest('.inline-notice') as HTMLElement;
  if (section === 'retirement') expect(within(alert).getByText('Status', { selector: 'dt' }).nextElementSibling?.textContent).toContain('Jan 6, 2025');
  if (section === 'media') expect(within(alert).getByRole('region', { name: 'Media' }).textContent!.split('https://example.org/logo.png')).toHaveLength(2);
  if (section === 'facts') {
    expect(within(alert).queryByRole('button', { name: 'Details' })).toBeNull();
    expect(within(alert).getByText('concurrent@example.test')).toBeDefined();
  }
  if (section === 'names' || section === 'categories') {
    // A saved value shows once, in its group, with its own details.
    expect(within(alert).getAllByText(section === 'names' ? 'Synthetic Legal Ltd' : 'Research')).toHaveLength(1);
    const value = within(alert).getByRole('region', { name: section === 'names' ? 'Name Synthetic Legal Ltd' : 'Category Research' });
    expect(within(value).getByText('Source reference').nextElementSibling?.textContent).toBe('card-42');
    expect(within(alert).getByText(section === 'names' ? 'legal-name-variant' : 'category-variant')).toBeDefined();
  }
});
