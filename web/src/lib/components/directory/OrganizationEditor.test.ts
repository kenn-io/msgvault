import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { requestOf, pathOf, organization, organizationProfile, envelope, richOrganizationProfile, controllerWith } from '../../directory/entity-fixtures.test-support';
import OrganizationEditor from './OrganizationEditor.svelte';

describe('OrganizationEditor', () => {
  it('retains the organization draft and routes blocked creation refresh to its controller', async () => {
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
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

  it('creates, renames, and deletes an unused organization', async () => {
    const requests: Request[] = [];
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/organizations' && request.method === 'GET') return Response.json({ organizations: [organization()], total: 1, limit: 50, offset: 0 });
      if (path === '/api/v1/organizations' && request.method === 'POST') return new Response(JSON.stringify(organization(22, 1, 'New Synthetic Org')), { status: 201, headers: { ETag: '"organization-22-r1"' } });
      if (path === '/api/v1/organizations/21' && request.method === 'GET') return new Response(JSON.stringify({ ...organizationProfile(2), organization: { ...organization(21, 2), retired_at: '2026-08-02T00:00:00Z' } }), { headers: { ETag: '"organization-21-r2"' } });
      if (path === '/api/v1/organizations/21' && request.method === 'PATCH') return new Response(JSON.stringify({ ...organization(21, 3, 'Renamed Synthetic Org'), retired_at: '2026-08-02T00:00:00Z' }), { headers: { ETag: '"organization-21-r3"' } });
      if (path === '/api/v1/organizations/21' && request.method === 'DELETE') return new Response(null, { status: 204 });
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.organizations = [organization()];
    const onDone = vi.fn();
    let view = render(OrganizationEditor, { controller, onDone });

    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization name' }), { target: { value: 'New Synthetic Org' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create organization' }));
    expect(await requests.find((request) => request.method === 'POST')!.json()).toMatchObject({ name: 'New Synthetic Org' });
    await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    view.unmount();
    onDone.mockClear();
    view = render(OrganizationEditor, { controller, organization: organization(), onDone });

    const dialog = await screen.findByRole('dialog', { name: 'Edit Synthetic Org' });
    const organizationName = await within(dialog).findByRole('textbox', { name: 'Organization name' });
    await fireEvent.input(organizationName, { target: { value: 'Renamed Synthetic Org' } });
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Save organization' }));
    await waitFor(() => expect(requests.some((request) => request.method === 'PATCH')).toBe(true));
    expect(requests.find((request) => request.method === 'PATCH')!.headers.get('If-Match')).toBe('"organization-21-r2"');
    await expect(requests.find((request) => request.method === 'PATCH')!.clone().json()).resolves.toMatchObject({ name: 'Renamed Synthetic Org' });
    expect((await requests.find((request) => request.method === 'PATCH')!.clone().json()).retired).toBeUndefined();
    await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    view.unmount();
    onDone.mockClear();
    view = render(OrganizationEditor, { controller, organization: organization(21, 3, 'Renamed Synthetic Org'), onDone });

    await fireEvent.click(await screen.findByRole('button', { name: 'Delete organization' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete organization' }));
    await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    expect(requests.some(request => request.method === 'DELETE')).toBe(true);
    expect(controller.organizations).toEqual([organization(22, 1, 'New Synthetic Org')]);
    view.unmount();
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
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
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
    await fireEvent.click(screen.getByRole('button', { name: 'Save organization profile' }));
    await waitFor(() => expect(requests.some((request) => request.method === 'PUT')).toBe(true));

    const put = requests.find((request) => request.method === 'PUT')!;
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

      await fireEvent.click(screen.getByRole('button', { name: 'Save organization profile' }));
      await waitFor(() => expect(puts).toBe(2));
      return;
    }
    const body = await put.json();
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

  it('keeps an editor open during submission', async () => {
    let resolveCreate!: (response: Response) => void;
    const pending = new Promise<Response>((resolve) => { resolveCreate = resolve; });
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
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
