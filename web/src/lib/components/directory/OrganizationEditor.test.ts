import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { valueProvenance } from './history-format';
import { requestOf, pathOf, organization, employment, organizationProfile, envelope, richOrganizationProfile, controllerWith } from '../../directory/entity-fixtures.test-support';
import OrganizationEditor from './OrganizationEditor.svelte';

describe('OrganizationEditor', () => {
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
    // The value shows once, its facts under its row; Email's heading already names its kind, an address's kind adds to it.
    expect(screen.getAllByText('concurrent@example.test')).toHaveLength(1);
    expect(within(address).getByText('Kind').nextElementSibling?.textContent).toBe('Work');
    expect(within(screen.getByRole('region', { name: 'Email concurrent@example.test' })).queryByText('Kind')).toBeNull();
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

  it('keeps each contact timestamp when their short provenance matches', async () => {
    const profile = richOrganizationProfile(1);
    profile.addresses = [];
    profile.contact_points[0].envelope = envelope(40, { updated_at: '2026-08-01T10:00:00Z' }) as typeof profile.contact_points[0]['envelope'];
    profile.contact_points.push({ ...profile.contact_points[0], original_value: 'second@example.test', envelope: envelope(47, { updated_at: '2026-08-01T15:00:00Z' }) as typeof profile.contact_points[0]['envelope'] });
    profile.contact_points.push({ ...profile.contact_points[0], original_value: 'third@example.test' });
    const { controller } = controllerWith(async () => Response.json(profile, { headers: { ETag: '"organization-21-r1"' } }));
    render(OrganizationEditor, { controller, organization: organization() });
    await screen.findByText('second@example.test');
    const first = valueProvenance(profile.contact_points[0].envelope);
    expect(screen.getByText(first.text, { selector: '.profile-heading .provenance' }).getAttribute('title')).toBe(first.title);
    const row = screen.getByText('second@example.test').closest('.detail-row') as HTMLElement;
    expect(row.querySelector('.description')?.getAttribute('title')).toBe(valueProvenance(profile.contact_points[1].envelope).title);
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
    await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
    await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    expect(writes.map(request => request.method)).toEqual(['PATCH']);
    const body = await writes[0].json();
    expect(body).toMatchObject({ name: 'Renamed Synthetic Org' });
    expect(body.retired).toBeUndefined();
  });

  it.each([1, 200])('blocks deletion for %s past employment records and limits named people', async count => {
    const requests: Request[] = [];
    const { controller } = controllerWith(async input => {
      const request = requestOf(input); requests.push(request);
      const path = pathOf(request);
      if (path.endsWith('/employments')) return Response.json({ employments: Array.from({ length: count }, (_, i) => employment(i + 1, 1, { person_id: Math.max(1, i), is_current: false })) });
      if (path.startsWith('/api/v1/people/')) return Response.json({ display_name: `Example Person ${path.split('/').at(-1)}` });
      return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    });
    const onOpenPerson = vi.fn(), onClose = vi.fn();
    render(OrganizationEditor, { controller, organization: organization(), onOpenPerson, onClose });
    await fireEvent.click(await screen.findByRole('button', { name: 'Delete organization…' }));
    // One sentence carries the count and the action, followed by the people holding the jobs.
    const first = await screen.findByText(count === 1 ? "Delete its job first. Ending it isn't enough." : "Delete its 200 or more jobs first. Ending them isn't enough.");
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
        return Response.json({ employments: checks > 2 ? [employment(11, 1, { person_id: 7, is_current: false })] : [] });
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
    expect(screen.getByRole('button', { name: 'Save organization' })).toBeDefined();
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
    if (path.endsWith('/employments')) return Response.json({ employments: [employment(11, 1, { person_id: 8, is_current: false })] });
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
  await fireEvent.click(action);
  expect(onOpenPerson).toHaveBeenCalledWith(8);
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

it('retries a failed alias save with the profile write alone', async () => {
  const methods: string[] = [];
  const onDone = vi.fn();
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'GET') return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    methods.push(request.method);
    return methods.length === 1 ? Response.json({ message: 'Profile unavailable' }, { status: 503 }) : Response.json(organizationProfile(2), { headers: { ETag: '"organization-21-r2"' } });
  });
  render(OrganizationEditor, { controller, organization: organization(), onDone });
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization aliases' }), { target: { value: 'Draft alias' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save aliases and categories' }));
  await screen.findByText("Couldn't save the aliases and categories.");
  expect(onDone).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
  expect(methods).toEqual(['PUT', 'PUT']);
});

it('preserves the edit baseline after Back from a conflicted delete', async () => {
  let reads = 0;
  const opening = organizationProfile();
  const newer = { ...opening, organization: { ...opening.organization, revision: 2, name: 'New name', primary_domain: 'concurrent.example' } };
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (pathOf(request).endsWith('/employments')) return Response.json({ employments: [] });
    if (request.method === 'DELETE') return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
    if (request.method === 'PATCH') {
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
  expect(screen.getByText('New name')).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: /^Back to / }));
  await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
  expect(await screen.findByRole('button', { name: 'Keep yours' })).toBeDefined();
});

it.each(['conflicted', 'attempt', 'profileAttempt', 'removal'] as const)('refreshes directory views on close after a %s conflict', async source => {
  let gets = 0;
  let patches = 0;
  let puts = 0;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (pathOf(request).endsWith('/employments')) return Response.json({ employments: [] });
    if (request.method === 'DELETE') return source === 'removal'
      ? Response.json({ message: 'Changed elsewhere' }, { status: 412 })
      : new Response(null, { status: 204 });
    if (request.method === 'PATCH') {
      patches++;
      if (source === 'conflicted') return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
      if (source === 'attempt') return patches === 1
        ? Response.json({ message: 'Changed elsewhere' }, { status: 412 })
        : Response.json(organization(), { headers: { ETag: '"organization-21-r2"' } });
      return Response.json({ message: 'Invalid domain' }, { status: 400 });
    }
    if (request.method === 'PUT') {
      puts++;
      if (source === 'attempt') return puts === 1
        ? Response.json({ message: 'Unavailable' }, { status: 503 })
        : Response.json(organizationProfile(3), { headers: { ETag: '"organization-21-r3"' } });
      if (source === 'profileAttempt') return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
    }
    return Response.json(organizationProfile(++gets), { headers: { ETag: `"organization-21-r${gets}"` } });
  });
  const refresh = vi.spyOn(controller, 'refreshDirectoryViews').mockResolvedValue();
  const onClose = vi.fn();
  render(OrganizationEditor, { controller, organization: organization(), onClose });

  if (source === 'conflicted') {
    await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization name' }), { target: { value: 'Draft name' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
    await screen.findByRole('button', { name: 'Keep yours' });
    await fireEvent.click(screen.getByRole('button', { name: 'Delete organization…' }));
    await fireEvent.click(await screen.findByRole('button', { name: /^Back to / }));
    await fireEvent.click(screen.getByRole('button', { name: 'Use saved' }));
  } else if (source === 'attempt') {
    await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization aliases' }), { target: { value: 'Draft alias' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization name' }), { target: { value: 'Draft name' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
    await screen.findByRole('button', { name: 'Use saved' });
    await fireEvent.click(screen.getByRole('button', { name: 'Keep yours' }));
    await waitFor(() => expect(patches).toBe(2));
    await waitFor(() => expect(document.querySelector('.editor')?.getAttribute('aria-busy')).toBe('false'));
    await fireEvent.click(screen.getByRole('button', { name: 'Save aliases and categories' }));
    await waitFor(() => expect(puts).toBe(1));
    await screen.findByText("Couldn't save the aliases and categories.");
    await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
  } else if (source === 'profileAttempt') {
    await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization aliases' }), { target: { value: 'Draft alias' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save aliases and categories' }));
    await screen.findByRole('button', { name: 'Use saved' });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization domain' }), { target: { value: 'bad domain' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
    await screen.findByText("Couldn't save this organization.");
    await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
  } else {
    await fireEvent.click(await screen.findByRole('button', { name: 'Delete organization…' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
    await screen.findByText('This organization changed since you opened it.');
    await fireEvent.click(screen.getByRole('button', { name: /^Back to / }));
    await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
  }

  expect(onClose).toHaveBeenCalledOnce();
  expect(refresh).toHaveBeenCalled();
});

it('drops an earlier jobs check when a new check starts', async () => {
  let finishFirst!: (response: Response) => void;
  let checks = 0;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (pathOf(request).endsWith('/employments')) return ++checks === 1
      ? new Promise<Response>(resolve => finishFirst = resolve)
      : Response.json({ employments: [] });
    return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
  });
  render(OrganizationEditor, { controller, organization: organization() });
  await fireEvent.click(await screen.findByRole('button', { name: 'Delete organization…' }));
  await waitFor(() => expect(checks).toBe(1));
  await fireEvent.click(screen.getByRole('button', { name: /^Back to / }));
  await fireEvent.click(await screen.findByRole('button', { name: 'Delete organization…' }));
  await waitFor(() => expect(screen.getByRole('button', { name: 'Delete' })).toHaveProperty('disabled', false));
  finishFirst(Response.json({ message: 'Check failed' }, { status: 400 }));
  await new Promise(resolve => setTimeout(resolve));
  expect(screen.queryByText("Couldn't check jobs.")).toBeNull();
  expect(screen.getByRole('button', { name: 'Delete' })).toHaveProperty('disabled', false);
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

it('handles conflicts after either organization or alias saves', async () => {
  {
  const onDone = vi.fn();
  let revision = 1;
  const contacts = richOrganizationProfile().contact_points;
  let savedContacts: string[] = [];
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    const tag = `"organization-21-r${revision}"`;
    if (request.method === 'GET') return Response.json({ ...organizationProfile(revision), contact_points: revision > 1 ? contacts : [] }, { headers: { ETag: tag } });
    if (request.headers.get('If-Match') !== tag) return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
    const body = await request.json();
    revision++;
    if (request.method === 'PATCH') return Response.json({ ...organization(21, revision), ...body }, { headers: { ETag: `"organization-21-r${revision}"` } });
    savedContacts = body.contact_points.map((item: { original_value: string }) => item.original_value);
    return Response.json({ ...organizationProfile(revision), ...body }, { headers: { ETag: `"organization-21-r${revision}"` } });
  });
  const firstView = render(OrganizationEditor, { controller, organization: organization(), onDone });
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization aliases' }), { target: { value: 'Synthetic Works, New alias' } });
  await fireEvent.input(screen.getByRole('textbox', { name: 'Organization name' }), { target: { value: 'New name' } });
  revision++;
  await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
  const conflict = (await screen.findByText('This organization changed since you opened it.')).closest('.kit-notice') as HTMLElement;
  expect(conflict.getAttribute('data-tone')).toBe('warning');
  await fireEvent.click(screen.getByRole('button', { name: 'Keep yours' }));
  expect(await screen.findByText('concurrent@example.test')).toBeDefined();
  await waitFor(() => expect(document.querySelector('.editor')?.getAttribute('aria-busy')).toBe('false'));
  await fireEvent.click(screen.getByRole('button', { name: 'Save aliases and categories' }));
  // The conflict version has a contact that the opening profile did not.
  const alert = (await screen.findByText('This organization changed since you opened it.')).closest('.inline-notice') as HTMLElement;
  expect(within(alert).getByText('concurrent@example.test')).toBeDefined();
  await fireEvent.click(within(alert).getByRole('button', { name: 'Keep yours' }));
  await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
  expect(savedContacts).toEqual(['concurrent@example.test']);
  firstView.unmount();
  controller.destroy();
  }
  {
  const onDone = vi.fn();
  let revision = 1;
  const writes: { method: string; body: { primary_domain?: string; name?: string; names?: { name: string; name_kind: string; source: string }[] } }[] = [];
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    const tag = `"organization-21-r${revision}"`;
    // Elsewhere, at r2: a new domain and a new alias.
    const elsewhere = revision > 1
      ? { organization: { ...organization(21, revision), primary_domain: 'other.example' }, names: [{ ...organizationProfile().names[0], envelope: envelope(31, { source: 'vcard_import', source_ref: 'resource-current' }) }, { organization_id: 21, name_kind: 'alias', name: 'Other alias', name_normalized: 'other alias', envelope: envelope(33) }] }
      : {};
    if (request.method === 'GET') return Response.json({ ...organizationProfile(revision), ...elsewhere }, { headers: { ETag: tag } });
    if (request.headers.get('If-Match') !== tag) return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
    const body = await request.json();
    writes.push({ method: request.method, body });
    revision++;
    if (request.method === 'PATCH') return Response.json({ ...organization(21, revision), ...body }, { headers: { ETag: `"organization-21-r${revision}"` } });
    return Response.json({ ...organizationProfile(revision), ...elsewhere, organization: { ...organization(21, revision), primary_domain: 'other.example' }, names: body.names.map((item: object, index: number) => ({ ...item, organization_id: 21, envelope: envelope(40 + index) })) }, { headers: { ETag: `"organization-21-r${revision}"` } });
  });
  const secondView = render(OrganizationEditor, { controller, organization: organization(), onDone });
  const aliases = await screen.findByRole('textbox', { name: 'Organization aliases' });
  await fireEvent.input(aliases, { target: { value: 'Synthetic Works, Draft alias' } });
  await fireEvent.input(screen.getByRole('textbox', { name: 'Organization name' }), { target: { value: 'New name' } });
  revision++;
  await fireEvent.click(screen.getByRole('button', { name: 'Save aliases and categories' }));
  let alert = (await screen.findByText('This organization changed since you opened it.')).closest('.inline-notice') as HTMLElement;
  expect(within(alert).getByText('Other alias')).toBeDefined();
  await fireEvent.click(within(alert).getByRole('button', { name: 'Keep yours' }));
  await waitFor(() => expect(writes).toHaveLength(1));
  await waitFor(() => expect(document.querySelector('.editor')?.getAttribute('aria-busy')).toBe('false'));
  // The aliases the form showed, with the saved one keeping its provenance; the other writer's alias is gone.
  expect(writes[0].body.names?.filter(item => item.name_kind === 'alias')).toEqual([expect.objectContaining({ name: 'Synthetic Works', source: 'vcard_import' }), expect.objectContaining({ name: 'Draft alias', source: 'user' })]);
  expect(aliases).toHaveProperty('value', 'Synthetic Works, Draft alias');
  expect(onDone).not.toHaveBeenCalled();
  await fireEvent.input(aliases, { target: { value: 'Synthetic Works, Draft alias, Second alias' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save aliases and categories' }));
  await waitFor(() => expect(writes).toHaveLength(2));
  await waitFor(() => expect(document.querySelector('.editor')?.getAttribute('aria-busy')).toBe('false'));
  // The untouched domain still reads as opened, so the organization save shows the newer one before writing.
  expect(screen.getByRole('textbox', { name: 'Organization domain' })).toHaveProperty('value', 'synthetic.example');
  await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
  alert = (await screen.findByText('This organization changed since you opened it.')).closest('.inline-notice') as HTMLElement;
  expect(within(alert).getByText('Domain', { selector: 'dt' }).nextElementSibling?.textContent).toBe('other.example');
  expect(writes).toHaveLength(2);
  await fireEvent.click(screen.getByRole('button', { name: 'Keep yours' }));
  await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
  expect(writes[2]).toMatchObject({ method: 'PATCH', body: { name: 'New name', primary_domain: 'synthetic.example' } });
  secondView.unmount();
  controller.destroy();
  }
});

it('keeps the dialog open while the other section still has unsaved edits', async () => {
  const writes: { method: string; tag: string | null; body: { primary_domain?: string; name?: string; names?: { name: string }[] } }[] = [];
  const onDone = vi.fn();
  let stored = organization();
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'GET') return Response.json({ ...organizationProfile(writes.length + 1), organization: stored }, { headers: { ETag: `"organization-21-r${writes.length + 1}"` } });
    const body = await request.json();
    writes.push({ method: request.method, tag: request.headers.get('If-Match'), body });
    const tag = `"organization-21-r${writes.length + 1}"`;
    stored = { ...stored, revision: writes.length + 1, ...(request.method === 'PATCH' ? body : {}) };
    if (request.method === 'PATCH') return Response.json(stored, { headers: { ETag: tag } });
    return Response.json({ ...organizationProfile(writes.length + 1), organization: stored, names: body.names.map((item: object, index: number) => ({ ...item, organization_id: 21, envelope: envelope(40 + index) })) }, { headers: { ETag: tag } });
  });
  render(OrganizationEditor, { controller, organization: organization(), onDone });
  const aliases = await screen.findByRole('textbox', { name: 'Organization aliases' });
  await fireEvent.input(screen.getByRole('textbox', { name: 'Organization domain' }), { target: { value: 'lumengrove.io' } });
  await fireEvent.input(aliases, { target: { value: 'Synthetic Works, Lumen' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
  const settled = () => waitFor(() => expect(document.querySelector('.editor')?.getAttribute('aria-busy')).toBe('false'));
  await waitFor(() => expect(writes.map(write => write.method)).toEqual(['PATCH']));
  await settled();
  expect(onDone).not.toHaveBeenCalled();
  expect(aliases).toHaveProperty('value', 'Synthetic Works, Lumen');
  await fireEvent.input(screen.getByRole('textbox', { name: 'Organization name' }), { target: { value: 'Lumen Grove' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save aliases and categories' }));
  await waitFor(() => expect(writes.map(write => write.method)).toEqual(['PATCH', 'PUT']));
  await settled();
  expect(onDone).not.toHaveBeenCalled();
  expect(screen.getByRole('textbox', { name: 'Organization name' })).toHaveProperty('value', 'Lumen Grove');
  await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
  await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
  // Each write sends the version the one before it returned.
  expect(writes.map(write => [write.method, write.tag])).toEqual([['PATCH', '"organization-21-r1"'], ['PUT', '"organization-21-r2"'], ['PATCH', '"organization-21-r3"']]);
  expect(writes[1].body.names?.map(item => item.name)).toEqual(['Synthetic Works', 'Lumen']);
  expect(writes[2].body).toMatchObject({ name: 'Lumen Grove', primary_domain: 'lumengrove.io' });
});

it.each(['organization save', 'aliases save'] as const)('shows the latest notice after a failed %s', async firstFailure => {
  const methods: string[] = [];
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
      ? Response.json({ message: 'Profile unavailable' }, { status: 503 })
      : Response.json({ message: 'primary domain "bad domain" is not a valid domain' }, { status: 400 });
  });
  render(OrganizationEditor, { controller, organization: organization() });
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
    await fireEvent.input(domain, { target: { value: 'bad domain' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
    await screen.findByText("Couldn't save this organization.");
    expect(screen.queryByText("Couldn't save the aliases and categories.")).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(methods).toEqual(['PUT', 'PATCH', 'PATCH']));
  }
});

it('waits to save the organization until one of its fields changes', async () => {
  const { controller } = controllerWith(async () => Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } }));
  render(OrganizationEditor, { controller, organization: organization() });
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization aliases' }), { target: { value: 'Draft alias' } });
  expect(screen.getByRole('button', { name: 'Save organization' })).toHaveProperty('disabled', true);
  await fireEvent.input(screen.getByRole('textbox', { name: 'Organization domain' }), { target: { value: 'new.example' } });
  expect(screen.getByRole('button', { name: 'Save organization' })).toHaveProperty('disabled', false);
});

it('retries a failed Keep yours after the fields were changed back', async () => {
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
  const opening = domain.value;
  await fireEvent.input(domain, { target: { value: 'new.example' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
  await fireEvent.input(domain, { target: { value: opening } });
  await fireEvent.click(await screen.findByRole('button', { name: 'Keep yours' }));
  await screen.findByText("Couldn't save this organization.");
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  await waitFor(() => expect(tags).toEqual(['"organization-21-r1"', '"organization-21-r2"', '"organization-21-r2"']));
});

it('requires review of a concurrent domain on delete after a core save', async () => {
  const action = 'delete' as const;
  let revision = 1;
  let stored = organization();
  const writes: { method: string; tag: string | null }[] = [];
  const onDone = vi.fn();
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (pathOf(request).endsWith('/employments')) return Response.json({ employments: [] });
    const tag = `"organization-21-r${revision}"`;
    if (request.method === 'GET') return Response.json({ ...organizationProfile(revision), organization: stored }, { headers: { ETag: tag } });
    writes.push({ method: request.method, tag: request.headers.get('If-Match') });
    if (request.headers.get('If-Match') !== tag) return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
    if (request.method === 'DELETE') return new Response(null, { status: 204 });
    stored = { ...stored, ...await request.json(), revision: ++revision };
    return Response.json(stored, { headers: { ETag: `"organization-21-r${revision}"` } });
  });
  render(OrganizationEditor, { controller, organization: organization(), onDone });
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization aliases' }), { target: { value: 'Synthetic Works, Draft alias' } });
  await fireEvent.input(screen.getByRole('textbox', { name: 'Organization name' }), { target: { value: 'First name' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
  await waitFor(() => expect(writes).toHaveLength(1));
  await waitFor(() => expect(document.querySelector('.editor')?.getAttribute('aria-busy')).toBe('false'));
  stored = { ...stored, primary_domain: 'concurrent.example', revision: ++revision };
  await fireEvent.click(screen.getByRole('button', { name: 'Delete organization…' }));
  await fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
  expect(await screen.findByText('concurrent.example')).toBeDefined();
  expect(writes[1]).toEqual({ method: 'DELETE', tag: '"organization-21-r2"' });
  expect(onDone).not.toHaveBeenCalled();
});

it('refreshes normalized fields before saving pending aliases', async () => {
  const onDone = vi.fn();
  const profile = organizationProfile();
  const normalized = { ...profile.organization, primary_domain: 'example.com' };
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'PATCH') return Response.json(normalized, { headers: { ETag: '"organization-21-r2"' } });
    if (request.method === 'PUT') return Response.json({ ...profile, organization: normalized }, { headers: { ETag: '"organization-21-r3"' } });
    return Response.json(profile, { headers: { ETag: '"organization-21-r1"' } });
  });
  render(OrganizationEditor, { controller, organization: organization(), onDone });
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization aliases' }), { target: { value: 'New alias' } });
  await fireEvent.input(screen.getByRole('textbox', { name: 'Organization domain' }), { target: { value: 'Example.COM' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
  await waitFor(() => expect(screen.getByRole('textbox', { name: 'Organization domain' })).toHaveProperty('value', 'example.com'));
  await fireEvent.click(screen.getByRole('button', { name: 'Save aliases and categories' }));
  await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
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
