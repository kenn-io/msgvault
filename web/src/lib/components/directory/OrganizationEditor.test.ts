import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { requestOf, pathOf, organization, organizationProfile, envelope, richOrganizationProfile, controllerWith } from '../../directory/entity-fixtures.test-support';
import OrganizationEditor from './OrganizationEditor.svelte';

describe('OrganizationEditor', () => {
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
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization aliases' }), { target: { value: 'Draft alias' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization categories' }), { target: { value: 'Research' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
    await waitFor(() => expect(puts).toBe(1));
    expect(onDone).not.toHaveBeenCalled();
    expect(screen.getByRole('alert').textContent).toContain('remain unsaved');
    expect(screen.getByRole('textbox', { name: 'Organization name' })).toHaveProperty('value', 'Draft company');
    await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
    await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    expect(patches).toBe(1);
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
    await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
    await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    if (field === 'details') {
      expect(writes.map(request => request.method)).toEqual(['PATCH']);
    } else {
      const body = await writes.find(request => request.method === 'PUT')!.json();
      if (field === 'categories') expect(body.names).toEqual([expect.objectContaining({ name: 'Example, Inc.' })]);
      if (field === 'aliases') expect(body.categories).toEqual([expect.objectContaining({ category: 'Research, development' })]);
    }
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
    await fireEvent.click(screen.getByRole('button', { name: 'Create organization' }));

    await screen.findByRole('alert');
    expect(name.value).toBe('Unknown Result Org');
    expect(screen.getByRole('button', { name: 'Create organization' })).toHaveProperty('disabled', true);
    const refresh = vi.spyOn(controller, 'refreshOrganizations');
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh organizations' }));
    await waitFor(() => expect(refresh).toHaveBeenCalledOnce());
  });

  it.each(['create', 'rename', 'delete'])('%s an unused organization', async operation => {
    const requests: Request[] = [];
    let organizations = [organization()];
    const { controller } = controllerWith(async input => {
      const request = requestOf(input); requests.push(request.clone());
      const path = pathOf(request);
      if (path === '/api/v1/organizations' && request.method === 'GET') return Response.json({ organizations, total: organizations.length, limit: 50, offset: 0 });
      if (path === '/api/v1/organizations' && request.method === 'POST') {
        const created = organization(22, 1, 'New Synthetic Org'); organizations.push(created);
        return Response.json(created, { status: 201, headers: { ETag: '"organization-22-r1"' } });
      }
      if (path === '/api/v1/organizations/21' && request.method === 'GET') return Response.json({ ...organizationProfile(2), organization: { ...organizations[0], revision: 2, retired_at: '2026-08-02T00:00:00Z' } }, { headers: { ETag: '"organization-21-r2"' } });
      if (path === '/api/v1/organizations/21' && request.method === 'PATCH') {
        organizations = [organization(21, 3, 'Renamed Synthetic Org')];
        return Response.json({ ...organizations[0], retired_at: '2026-08-02T00:00:00Z' }, { headers: { ETag: '"organization-21-r3"' } });
      }
      if (path === '/api/v1/organizations/21/employments') return Response.json({ employments: [] });
      if (path === '/api/v1/organizations/21' && request.method === 'DELETE') { organizations = []; return new Response(null, { status: 204 }); }
      throw new Error(`unexpected ${request.method} ${path}`);
    });
    controller.organizations = [...organizations];
    const onDone = vi.fn();
    const view = render(OrganizationEditor, { controller, ...(operation === 'create' ? {} : { organization: organization() }), onDone });
    try {
      if (operation === 'delete') {
        await fireEvent.click(await screen.findByRole('button', { name: 'Delete organization' }));
        await waitFor(() => expect(screen.getByRole('button', { name: 'Confirm delete organization' })).toHaveProperty('disabled', false));
        await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete organization' }));
      } else {
        const name = await screen.findByRole('textbox', { name: 'Organization name' });
        await fireEvent.input(name, { target: { value: operation === 'create' ? 'New Synthetic Org' : 'Renamed Synthetic Org' } });
        await fireEvent.click(screen.getByRole('button', { name: operation === 'create' ? 'Create organization' : 'Save organization' }));
      }
      await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
      const write = requests.find(request => request.method === (operation === 'create' ? 'POST' : operation === 'rename' ? 'PATCH' : 'DELETE'))!;
      expect(write).toBeDefined();
      if (operation === 'create') await expect(write.json()).resolves.toMatchObject({ name: 'New Synthetic Org' });
      if (operation === 'rename') {
        const body = await write.json();
        expect(body).toMatchObject({ name: 'Renamed Synthetic Org' });
        expect(body.retired).toBeUndefined();
      }
    } finally { view.unmount(); controller.destroy(); }
  });

  it.each([false, true])('preserves profile draft changes and concurrent organization data (conflict=%s)', async conflict => {
    const requests: Request[] = [];
    let reads = 0; let puts = 0;
    const baseline = {
      ...organizationProfile(2),
      names: [{ organization_id: 21, name_kind: 'alias', name: 'Baseline Alias', name_normalized: 'baseline alias', envelope: envelope(50) }],
      categories: [{ organization_id: 21, category: 'Baseline Category', category_normalized: 'baseline category', envelope: envelope(51) }]
    };
    const rich = richOrganizationProfile(3);
    const fresh = {
      ...rich,
      organization: { ...rich.organization, name: 'Concurrent Org Name', primary_domain: 'concurrent.example', description: 'Concurrent organization description' },
      media: [...baseline.media, ...rich.media],
      names: [
        ...rich.names.filter((item) => item.name_kind !== 'alias'),
        baseline.names[0],
        { organization_id: 21, name_kind: 'alias', name: 'Synthetic Works', name_normalized: 'synthetic works', envelope: envelope(52, { source: 'vcard_import', source_ref: 'concurrent-alias', source_resource_uid: 'resource-concurrent-alias', vcard: { property: 'NICKNAME' } }) }
      ],
      categories: [baseline.categories[0], ...rich.categories]
    };
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/organizations/21' && request.method === 'GET') {
        reads += 1;
        const profile = reads === 1 ? baseline : fresh;
        return new Response(JSON.stringify(profile), { headers: { ETag: `"organization-21-r${reads + 1}"` } });
      }
      if (path === '/api/v1/organizations/21/profile' && request.method === 'PUT') {
        if (++puts === 1 && conflict) return Response.json({ error: 'organization_revision_conflict', message: 'profile changed elsewhere' }, { status: 409 });
        return new Response(JSON.stringify(fresh), { headers: { ETag: '"organization-21-r4"' } });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.organizations = [organization()];
    render(OrganizationEditor, { controller, organization: organization() });
    const aliases = await screen.findByRole('textbox', { name: 'Organization aliases' });
    await fireEvent.input(aliases, { target: { value: 'User Draft Alias' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization categories' }), { target: { value: 'User Draft Category' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
    await waitFor(() => expect(requests.some((request) => request.method === 'PUT')).toBe(true));

    if (conflict) {
      const alert = await screen.findByRole('alert');
      expect(alert.textContent).toContain('Concurrent Org Name');
      expect(alert.textContent).toContain('concurrent.example');
      expect(alert.textContent).toContain('Concurrent organization description');
      expect(alert.textContent).toContain('Research');
      expect(alert.textContent).toContain('concurrent@example.test');
      expect(alert.textContent).toContain('Suite 5, 12 Example Road');
      expect(alert.textContent).toContain('REG-42');
      expect(alert.textContent).toContain('Concurrent logo');
      expect(aliases).toHaveProperty('value', 'User Draft Alias');

      await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
      await waitFor(() => expect(puts).toBe(2));
    }
    const body = await requests.filter(request => request.method === 'PUT').at(-1)!.json();
    expect(body.names).toEqual(expect.arrayContaining([
      expect.objectContaining({ name: 'Synthetic Legal Ltd', name_kind: 'legal', source_ref: 'card-42', vcard_property: 'FN', vcard_group: 'item2', vcard_prop_id: 'prop-9', vcard_pid: ['1.1', '2.1'], vcard_altid: 'alt-4' }),
      expect.objectContaining({ name: 'Synthetic Works', name_kind: 'alias', source_ref: 'concurrent-alias', source_resource_uid: 'resource-concurrent-alias', vcard_property: 'NICKNAME' }),
      expect.objectContaining({ name: 'User Draft Alias', name_kind: 'alias', source: 'user' })
    ]));
    expect(body.names).not.toEqual(expect.arrayContaining([expect.objectContaining({ name: 'Baseline Alias' })]));
    expect(body.categories).toEqual(expect.arrayContaining([
      expect.objectContaining({ category: 'Research', source_ref: 'card-42', source_resource_uid: 'resource-card-42', active_from: '2025-02-03T04:05:06Z' }),
      expect.objectContaining({ category: 'User Draft Category', source: 'user' })
    ]));
    expect(body.categories).not.toEqual(expect.arrayContaining([expect.objectContaining({ category: 'Baseline Category' })]));
    expect(body.contact_points).toEqual([expect.objectContaining({
      contact_kind: 'email', original_value: 'concurrent@example.test', service_slug: 'email', scope_kind: 'department', scope_value: 'research',
      uri: 'mailto:concurrent@example.test', ordinal: 7, pref: 2, type_label: 'work', type_tokens: ['WORK', 'INTERNET'],
      source: 'vcard_import', source_ref: 'card-42', confidence: 0.87, active_from: '2025-02-03T04:05:06Z',
      vcard_property: 'EMAIL', vcard_group: 'item2', vcard_prop_id: 'prop-9', vcard_pid: ['1.1', '2.1'], vcard_altid: 'alt-4'
    })]);
    expect(body.addresses).toEqual([expect.objectContaining({
      address_kind: 'work', original_value: 'Suite 5, 12 Example Road', post_office_box: 'PO 12', extended_address: 'Suite 5',
      street_address: '12 Example Road', locality: 'Exampleton', region: 'EX', postal_code: '12345', country_name: 'Exampleland',
      extended_components: 'Lab 3', free_text: 'Rear entrance', place_uri: 'geo:1,2', geo_uri: 'geo:1,2', label: 'HQ', timezone: 'Etc/UTC', country_code: 'EX',
      source_ref: 'card-42', vcard_property: 'ADR'
    })]);
    expect(body.identifiers).toEqual([expect.objectContaining({ identifier_kind: 'registry', identifier_value: 'REG-42', source_ref: 'card-42', vcard_property: 'X-REGISTRY' })]);
    expect(body.media).toEqual(expect.arrayContaining([expect.objectContaining({ content_hash: 'sha256:synthetic' }), expect.objectContaining({ media_kind: 'logo', media_type: 'image/png', uri: 'https://example.test/logo.png', original_value: 'Concurrent logo', content_hash: 'sha256:concurrent', source_ref: 'card-42', vcard_property: 'LOGO' })]));
    for (const media of body.media) expect(media.data).toBeUndefined();
  });

  it.each([1, 200])('blocks deletion for %s past employment records and limits named people', async count => {
    const requests: Request[] = [];
    const { controller } = controllerWith(async input => {
      const request = requestOf(input); requests.push(request);
      const path = pathOf(request);
      if (path.endsWith('/employments')) return Response.json({ employments: Array.from({ length: count }, (_, i) => ({ person_id: Math.max(1, i), is_current: false })) });
      if (path === '/api/v1/people/3') return Response.json({ message: 'Name unavailable' }, { status: 400 });
      if (path.startsWith('/api/v1/people/')) return Response.json({ display_name: `Example Person ${path.split('/').at(-1)}` });
      return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    });
    render(OrganizationEditor, { controller, organization: organization() });
    await fireEvent.click(await screen.findByRole('button', { name: 'Delete organization' }));
    await screen.findByText(count === 1 ? 'Synthetic Org has 1 employment record. Remove it first.' : 'Synthetic Org has at least 200 employment records. Remove them first.');
    expect(screen.getByRole('button', { name: 'Confirm delete organization' })).toHaveProperty('disabled', true);
    expect(screen.getByText('Example Person 1')).toBeDefined();
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
        return Response.json({ employments: checks > 2 ? [{ person_id: 7 }] : [] });
      }
      if (pathOf(request) === '/api/v1/people/7') return Response.json({ display_name: 'Example Person' });
      if (request.method === 'DELETE') return Response.json({ message: 'Employment added elsewhere' }, { status: 409 });
      return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    });
    render(OrganizationEditor, { controller, organization: organization() });
    await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization name' }), { target: { value: 'Kept draft' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Delete organization' }));
    await screen.findByText('Check failed');
    expect(screen.getByRole('button', { name: 'Confirm delete organization' })).toHaveProperty('disabled', true);
    await fireEvent.click(screen.getByRole('button', { name: 'Retry employment check' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Confirm delete organization' })).toHaveProperty('disabled', false));
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete organization' }));
    await screen.findByText(/This organization changed elsewhere. Employment added elsewhere/);
    await screen.findByText('Synthetic Org has 1 employment record. Remove it first.');
    expect(checks).toBe(3);
    expect(screen.getByRole('button', { name: 'Confirm delete organization' })).toHaveProperty('disabled', true);
    await fireEvent.click(screen.getByRole('button', { name: 'Cancel organization deletion' }));
    expect(screen.getByRole('textbox', { name: 'Organization name' })).toHaveProperty('value', 'Kept draft');
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
    await fireEvent.click(screen.getByRole('button', { name: 'Create organization' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Close organization editor' }));
    expect(screen.getByRole('dialog', { name: 'Create organization' })).toBeDefined();
    expect(onClose).not.toHaveBeenCalled();
    resolveCreate(new Response(JSON.stringify(organization(22, 1, 'Pending Org')), { status: 201, headers: { ETag: '"organization-22-r1"' } }));
    await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    view.unmount();
  });
});
