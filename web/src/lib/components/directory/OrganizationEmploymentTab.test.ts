import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { openTypeahead } from '../../../test/kit-ui';
import { requestOf, pathOf, organization, organizationProfile, employment, collection, controllerWith } from '../../directory/entity-fixtures.test-support';
import OrganizationEmploymentTab from './OrganizationEmploymentTab.svelte';

async function rowAction(label: string): Promise<void> {
  await fireEvent.click(screen.getAllByRole('button', { name: /^Actions for /  })[0]);
  await fireEvent.click(await screen.findByRole('menuitem', { name: label }));
  if (label === 'Delete…') await waitFor(() => expect(screen.getByRole('button', { name: 'Delete' })).toHaveProperty('disabled', false));
}

describe('OrganizationEmploymentTab', () => {
  it.each([true, false])('explains permanent deletion for current=%s employment', async current => {
    let release!: (response: Response) => void;
    const pending = new Promise<Response>(resolve => { release = resolve; });
    const { controller } = controllerWith(async input => {
      const request = requestOf(input), path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') return Response.json(employment(11, 1, { is_current: current }), { headers: { ETag: '"employment-11-r1"' } });
      if (path === '/api/v1/employments/11' && request.method === 'DELETE') return pending;
      if (path === '/api/v1/people/7/employments') return collection([]);
      throw new Error(`unexpected ${request.method} ${path}`);
    });
    controller.employments = [employment(11, 1, { is_current: current })];
    const view = render(OrganizationEmploymentTab, { controller, personID: 7 });
    try {
      if (!current) await fireEvent.click(screen.getByText(/^\d+ past jobs?$/));
      await rowAction('Delete…');
      const dialog = screen.getByRole('dialog', { name: /^Delete .*\?/ });
      expect(dialog.textContent).toContain('This permanently removes the job.');
      expect(dialog.textContent?.includes('End it instead')).toBe(current);
      if (current) {
        await fireEvent.click(within(dialog).getByRole('button', { name: 'Delete' }));
        expect(screen.getByRole('button', { name: 'End it instead…' })).toHaveProperty('disabled', true);
        expect(screen.getAllByRole('dialog')).toHaveLength(1);
        release(new Response(null, { status: 204 }));
        await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
      }
    } finally { release(new Response(null, { status: 204 })); view.unmount(); controller.destroy(); }
  });

  it('shows editable Work while an employer name is still loading', async () => {
    let release!: (response: Response) => void;
    const pending = new Promise<Response>(resolve => { release = resolve; });
    const { controller } = controllerWith(async input => {
      const path = pathOf(requestOf(input));
      if (path === '/api/v1/people/7/employments') return Response.json({ employments: [employment()] });
      if (path === '/api/v1/organizations/21') return pending;
      throw new Error(`unexpected ${path}`);
    });
    const view = render(OrganizationEmploymentTab, { controller, personID: 7 });
    const opening = controller.refreshEmployments();
    try {
      expect(await screen.findByRole('button', { name: /^Actions for /  })).toBeDefined();
      expect(screen.getByRole('button', { name: 'this organization' })).toBeDefined();
      release(Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } }));
      expect(await screen.findByRole('button', { name: 'Synthetic Org' })).toBeDefined();
    } finally { release(Response.json(organizationProfile())); await opening; view.unmount(); controller.destroy(); }
  });

  it('opens an employer with one fresh organization read', async () => {
    let reads = 0;
    const { controller } = controllerWith(async input => {
      const path = pathOf(requestOf(input));
      if (path === '/api/v1/organizations/21') { reads += 1; return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } }); }
      throw new Error(`unexpected ${path}`);
    });
    controller.employments = [employment()];
    const view = render(OrganizationEmploymentTab, { controller, personID: 7 });
    try {
      await fireEvent.click(screen.getByRole('button', { name: 'this organization' }));
      const name = await screen.findByRole('textbox', { name: 'Organization name' });
      await waitFor(() => expect(name).toHaveProperty('disabled', false));
      expect(name).toHaveProperty('value', 'Synthetic Org');
      expect(screen.getByRole('dialog', { name: 'Synthetic Org' })).toBeDefined();
      expect(reads).toBe(1);
    } finally { view.unmount(); controller.destroy(); }
  });

  it('keeps local employment history actionable beside a reconciliation warning and recovers', async () => {
    let reads = 0;
    let resolveRefresh!: (response: Response) => void;
    const pendingRefresh = new Promise<Response>((resolve) => { resolveRefresh = resolve; });
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (pathOf(request) === '/api/v1/people/7/employments') {
        reads += 1;
        return pendingRefresh;
      }
      throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
    }));
    controller.employments = [employment(11, 2, { title: 'Locally updated role' })];
    controller.errors.employments = 'reconciliation failed';
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    expect(screen.getByRole('alert').textContent).toContain("Couldn't load jobs.");
    expect(screen.getByText(/Locally updated role/)).toBeDefined();
    expect(screen.getByRole('button', { name: /^Actions for /  })).toBeDefined();
    const refresh = screen.getByRole('button', { name: 'Retry' });
    await fireEvent.click(refresh);

    await waitFor(() => expect(reads).toBe(1));
    expect(screen.getByRole('status').textContent).toContain('Loading jobs');
    expect(screen.getByText(/Locally updated role/)).toBeDefined();
    expect(screen.getByRole('button', { name: /^Actions for /  })).toBeDefined();

    resolveRefresh(collection([employment(12, 2, { title: 'Recovered role' })], undefined));
    expect(screen.queryByRole('alert')).toBeNull();
    expect(await screen.findByText(/Recovered role/)).toBeDefined();
  });

  it('creates work for the selected person', async () => {
    let body: Record<string, unknown> | undefined;
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (request.method === 'POST') { body = await request.json(); return Response.json(employment()); }
      if (pathOf(request) === '/api/v1/organizations') return Response.json({ organizations: [organization()], total: 1 });
      return collection([]);
    });
    render(OrganizationEmploymentTab, { controller, personID: 7 });
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!);
    await screen.findByRole('button', { name: /^Organization(:|$)/ });
    await openTypeahead('Organization');
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Synthetic Org/ }));
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!);
    await waitFor(() => expect(body).toHaveProperty('person_id', 7));
  });

  it('shows work dates and linked employers', async () => {
    const { controller } = controllerWith(vi.fn<typeof fetch>());
    controller.organizations = [organization()];
    controller.employments = [employment(11, 1, { title: 'Engineer', department: 'Platform', start_date: { year: 2024, month: 3 }, is_current: true }), employment(12, 1, { is_current: false, title: 'Historical role' })];
    render(OrganizationEmploymentTab, { controller, personID: 7 });
    const row = screen.getByText(/Engineer at/).closest('.detail-row')!;
    expect(row.textContent).toContain('Mar 2024 – present · Platform');
    expect(within(row as HTMLElement).getByRole('button', { name: 'Synthetic Org' })).toBeDefined();
    await fireEvent.click(screen.getByText('1 past job'));
    expect(screen.getByText(/Historical role/)).toBeDefined();
  });

  it('locks Make primary while its preparation read is pending', async () => {
    let release!: (response: Response) => void;
    const pending = new Promise<Response>(resolve => { release = resolve; });
    let reads = 0;
    let writes = 0;
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') {
        reads++;
        return reads === 1 ? pending : Response.json(employment(11, reads), { headers: { ETag: `"employment-11-r${reads}"` } });
      }
      if (path === '/api/v1/employments/11/primary') {
        writes++;
        return writes === 1 ? Response.json({ message: 'Temporary failure' }, { status: 500 }) : Response.json(employment(11, 3, { is_primary: true }));
      }
      if (path === '/api/v1/people/7/employments') return collection([employment(11, 2, { is_primary: true }), employment(12, 2, { title: 'Other role', is_primary: false })]);
      throw new Error(`unexpected ${request.method} ${path}`);
    });
    controller.employments = [employment(), employment(12, 1, { title: 'Other role', is_primary: false })];
    const view = render(OrganizationEmploymentTab, { controller, personID: 7 });
    try {
      await rowAction('Make primary');
      await waitFor(() => expect(reads).toBe(1));
      await fireEvent.click(screen.getAllByRole('button', { name: /^Actions for / })[0]);
      await fireEvent.click(await screen.findByRole('menuitem', { name: 'Make primary' }));
      expect(reads).toBe(1);
      release(new Response(JSON.stringify(employment(11, 2, { is_primary: true })), { headers: { ETag: '"employment-11-r2"' } }));
      await waitFor(() => expect(writes).toBe(1));
      await screen.findByText("Couldn't make this job primary.");
      await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
      await waitFor(() => expect(writes).toBe(2));
      await waitFor(() => expect(screen.getByText(/Engineer/).closest('.detail-row')?.textContent).toContain('Primary'));
    } finally { release(Response.json(employment(), { headers: { ETag: '"employment-11-r1"' } })); view.unmount(); controller.destroy(); }
  });

  it('moves an ended job to Past', async () => {
    const requests: Request[] = [];
    let revision = 1;
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') {
        return new Response(JSON.stringify(employment(11, revision, { start_date: { year: 2024, month: 3 } })), { headers: { ETag: `"employment-11-r${revision}"` } });
      }
      if (path === '/api/v1/employments/11/end') {
        revision += 1;
        return new Response(JSON.stringify(employment(11, revision, { is_current: false, end_date: { year: 2026, month: 8 } })), { headers: { ETag: `"employment-11-r${revision}"` } });
      }
      if (path === '/api/v1/people/7/employments') {
        return Response.json({ employments: [employment(11, revision, { is_current: false })] });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.employments = [employment(11, 1, { start_date: { year: 2024, month: 3 } })];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await rowAction('End…');
    const endDate = await screen.findByRole('textbox', { name: 'End date' });
    await waitFor(() => expect(endDate).toHaveProperty('disabled', false));
    await fireEvent.input(endDate, { target: { value: '2026-08' } });
    await waitFor(() => expect(screen.getByRole('button', { name: 'End' })).toHaveProperty('disabled', false));
    await fireEvent.click(screen.getByRole('button', { name: 'End' }));
    await waitFor(() => expect(requests.some((request) => pathOf(request) === '/api/v1/employments/11/end')).toBe(true));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: /^End /  })).toBeNull());

    expect(await screen.findByText('1 past job')).toBeDefined();
  });

  it('uses retained employer records for Work row names after organization search', async () => {
    const { controller } = controllerWith(async input => {
      const path = pathOf(requestOf(input));
      if (path.startsWith('/api/v1/organizations')) return Response.json({ organizations: [organization(99, 1, 'Search Result')], total: 1, limit: 50, offset: 0 });
      throw new Error(`unexpected ${path}`);
    });
    controller.employments = [employment(11, 1, { is_primary: true }), employment(12, 1, { organization_id: 22, title: 'Advisor' })];
    controller.organizationRecords.set(21, organizationProfile(1, 'Primary Co'));
    controller.organizationRecords.set(22, { ...organizationProfile(), organization: organization(22, 1, 'Second Co') });
    const view = render(OrganizationEmploymentTab, { controller, personID: 7 });
    try {
      expect(screen.getByText(/Advisor at/).closest('.detail-row')?.textContent).toContain('Advisor at Second Co');
      await controller.refreshOrganizations('Search');
      expect(screen.getByText(/Advisor at/).closest('.detail-row')?.textContent).toContain('Advisor at Second Co');
    } finally { view.unmount(); controller.destroy(); }
  });
});
