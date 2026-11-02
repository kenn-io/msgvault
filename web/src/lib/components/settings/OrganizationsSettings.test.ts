import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { requestOf, pathOf, organization, organizationProfile, controllerWith } from '../../directory/entity-fixtures.test-support';
import OrganizationsSettings from './OrganizationsSettings.svelte';

describe('OrganizationsSettings', () => {
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

    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search organizations' }), { target: { value: 'synthetic again' } });
    expect(await screen.findByRole('button', { name: 'Edit Synthetic Org' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Add organization' }));
    expect(screen.getByRole('dialog', { name: 'Create organization' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Close organization editor' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Create organization' })).toBeNull());
    await fireEvent.click(screen.getByRole('button', { name: 'Edit Synthetic Org' }));
    expect(await screen.findByRole('dialog', { name: 'Edit Synthetic Org' })).toBeDefined();
  });
});
