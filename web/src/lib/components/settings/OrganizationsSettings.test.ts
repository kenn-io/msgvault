import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { requestOf, pathOf, organization, organizationProfile, controllerWith } from '../../directory/entity-fixtures.test-support';
import OrganizationsSettings from './OrganizationsSettings.svelte';

describe('OrganizationsSettings', () => {
  it.each(['', 'Synthetic'])('refreshes the current query after creating during a pending catalog read (%s)', async (query) => {
    let resolveStale!: (response: Response) => void;
    const stale = new Promise<Response>((resolve) => { resolveStale = resolve; });
    let staleRequest: Request | undefined;
    let created = false;
    const queries: string[] = [];
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (pathOf(request) === '/api/v1/organizations') {
        if (request.method === 'POST') {
          expect(await request.clone().json()).toMatchObject({ name: 'Synthetic New Org' });
          created = true;
          return Response.json(organization(22, 1, 'Synthetic New Org'), { status: 201 });
        }
        const currentQuery = new URL(request.url).searchParams.get('q') ?? '';
        queries.push(currentQuery);
        if (currentQuery === query && !created) {
          staleRequest = request;
          return stale;
        }
        return Response.json({ organizations: created ? [organization(22, 1, 'Synthetic New Org')] : [organization()], total: 1, limit: 50, offset: 0 });
      }
      throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
    }), 0);
    render(OrganizationsSettings, { controller });
    if (query) {
      await screen.findByRole('button', { name: 'Edit Synthetic Org' });
      await fireEvent.input(screen.getByRole('searchbox', { name: 'Search organizations' }), { target: { value: query } });
    }
    await waitFor(() => expect(staleRequest).toBeDefined());
    await fireEvent.click(screen.getByRole('button', { name: 'Add organization' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization name' }), { target: { value: 'Synthetic New Org' } });
    await fireEvent.click(within(screen.getByRole('dialog', { name: 'Add organization' })).getByRole('button', { name: 'Add organization' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Add organization' })).toBeNull());
    await waitFor(() => expect(queries.filter((value) => value === query)).toHaveLength(2));
    expect(staleRequest?.signal.aborted).toBe(true);
    resolveStale(Response.json({ organizations: [organization()], total: 1, limit: 50, offset: 0 }));

    await waitFor(() => expect(controller.organizationsLoading).toBe(false));
    expect(await screen.findByRole('button', { name: 'Edit Synthetic New Org' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Edit Synthetic Org' })).toBeNull();
    controller.destroy();
  });

  it('recovers organization search and opens Add and Edit editors', async () => {
    let resolveSearch!: (response: Response) => void;
    const search = new Promise<Response>((resolve) => { resolveSearch = resolve; });
    let searches = 0;
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (pathOf(request) === '/api/v1/organizations/21') return Response.json(organizationProfile(1), { headers: { ETag: '"organization-21-r1"' } });
      if (pathOf(request) === '/api/v1/organizations') {
        searches += 1;
        return searches === 1 ? search : Response.json({ organizations: [organization()], total: 1, limit: 50, offset: 0 });
      }
      throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
    }), 0);
    render(OrganizationsSettings, { controller });

    expect((await screen.findByRole('status')).textContent).toContain('Loading organizations');
    resolveSearch(Response.json({ error: 'unavailable', message: 'Organization search unavailable.' }, { status: 503 }));
    expect((await screen.findByRole('alert')).textContent).toContain('Organization search unavailable.');

    await fireEvent.click(screen.getByRole('button', { name: 'Retry organizations' }));
    expect(await screen.findByRole('button', { name: 'Edit Synthetic Org' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Add organization' }));
    expect(screen.getByRole('dialog', { name: 'Add organization' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Add organization' })).toBeNull());
    await fireEvent.click(screen.getByRole('button', { name: 'Edit Synthetic Org' }));
    expect(await screen.findByRole('dialog', { name: 'Synthetic Org' })).toBeDefined();
  });
});
