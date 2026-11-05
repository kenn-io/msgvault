import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { appShortcuts } from '@kenn-io/kit-ui';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { DirectoryEntityController } from '../../directory/entity-controller.svelte';
import { openTypeahead } from '../../../test/kit-ui';
import { requestOf, pathOf, organization, organizationProfile, employment, collection, controllerWith } from '../../directory/entity-fixtures.test-support';
import OrganizationEmploymentTab from './OrganizationEmploymentTab.svelte';

describe('OrganizationEmploymentTab', () => {
  it('creates an employer inline and reuses it after employment creation fails', async () => {
    let employers = 0;
    let jobs = 0;
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/organizations' && request.method === 'POST') {
        employers++;
        expect(await request.json()).toMatchObject({ name: 'New Example', primary_domain: 'example.org' });
        return Response.json(organization(22, 1, 'New Example'), { headers: { ETag: '"organization-22-r1"' } });
      }
      if (path === '/api/v1/organizations') return Response.json({ organizations: [], total: 0 });
      if (path === '/api/v1/employments') {
        jobs++;
        expect(await request.json()).toMatchObject({ organization_id: 22, title: 'Draft role' });
        if (jobs === 1) return Response.json({ message: 'Invalid employment' }, { status: 400 });
        return Response.json(employment(11, 1, { organization_id: 22 }), { headers: { ETag: '"employment-11-r1"' } });
      }
      if (path === '/api/v1/people/7/employments') return collection([]);
      throw new Error(`unexpected ${request.method} ${path}`);
    });
    render(OrganizationEmploymentTab, { controller, personID: 7 });
    await fireEvent.click(screen.getByRole('button', { name: 'Add work' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'New organization' }));
    expect(screen.getAllByRole('dialog')).toHaveLength(1);
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization name' }), { target: { value: 'New Example' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization domain' }), { target: { value: 'example.org' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Employment title' }), { target: { value: 'Draft role' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create employment' }));
    await screen.findByText('Invalid employment');
    expect(screen.getByRole('textbox', { name: 'Employment title' })).toHaveProperty('value', 'Draft role');
    expect(screen.getByRole('button', { name: /^Organization(:|$)/ }).textContent).toContain('New Example');
    await fireEvent.click(screen.getByRole('button', { name: 'Create employment' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(employers).toBe(1);
    expect(jobs).toBe(2);
  });

  it('blocks an unknown inline employer create until organizations are refreshed', async () => {
    let employers = 0;
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (request.method === 'POST') { employers++; return Response.json({ message: 'Unavailable' }, { status: 503 }); }
      return Response.json({ organizations: [], total: 0 });
    });
    render(OrganizationEmploymentTab, { controller, personID: 7 });
    await fireEvent.click(screen.getByRole('button', { name: 'Add work' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'New organization' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization name' }), { target: { value: 'Example company' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create employment' }));
    await screen.findByRole('alert');
    expect(screen.getByRole('button', { name: 'Create employment' })).toHaveProperty('disabled', true);
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh organizations' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Create employment' })).toHaveProperty('disabled', false));
    expect(employers).toBe(1);
    expect(screen.getByRole('textbox', { name: 'Organization name' })).toHaveProperty('value', 'Example company');
  });

  it('prefills the local day across UTC midnight, retains an End conflict, and restores focus', async () => {
    vi.stubEnv('TZ', 'America/New_York');
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(new Date('2026-11-05T04:30:00Z'));
    let reads = 0;
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (request.method === 'POST') return Response.json({ message: 'Concurrent end', error: 'revision_conflict' }, { status: 412 });
      reads++;
      return Response.json(employment(11, reads, { title: 'Current saved role' }), { headers: { ETag: `"employment-11-r${reads}"` } });
    });
    controller.employments = [employment()];
    const view = render(OrganizationEmploymentTab, { controller, personID: 7 });
    try {
      const trigger = screen.getByRole('button', { name: 'End employment' });
      await fireEvent.click(trigger);
      const field = await screen.findByRole('textbox', { name: 'Employment end date' });
      await waitFor(() => expect(field).toHaveProperty('disabled', false));
      expect(field).toHaveProperty('value', '2026-11-04');
      await fireEvent.input(field, { target: { value: '2026-13' } });
      expect(screen.getByRole('button', { name: 'End' })).toHaveProperty('disabled', true);
      await fireEvent.input(field, { target: { value: '2026' } });
      await fireEvent.click(screen.getByRole('button', { name: 'End' }));
      expect((await screen.findByRole('alert')).textContent).toContain('Current saved role');
      expect(field).toHaveProperty('value', '2026');
      await fireEvent.keyDown(document, { key: 'Escape' });
      await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
      expect(document.activeElement).toBe(trigger);
      await fireEvent.click(trigger);
      await fireEvent.mouseDown(document.body);
      await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    } finally { vi.useRealTimers(); vi.unstubAllEnvs(); view.unmount(); controller.destroy(); }
  });

  it.each([true, false])('explains permanent deletion for current=%s employment', async current => {
    const { controller } = controllerWith(vi.fn<typeof fetch>());
    controller.employments = [employment(11, 1, { is_current: current })];
    render(OrganizationEmploymentTab, { controller, personID: 7 });
    await fireEvent.click(screen.getByRole('button', { name: 'Delete employment' }));
    const dialog = screen.getByRole('dialog', { name: 'Delete employment' });
    expect(dialog.textContent).toContain('This permanently removes the employment record.');
    expect(dialog.textContent?.includes('End employment to keep it as past work.')).toBe(current);
    expect(within(dialog).getByRole('button', { name: 'Close delete employment' })).toBeDefined();
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
      expect(await screen.findByRole('button', { name: 'Edit employment' })).toBeDefined();
      expect(screen.getByRole('button', { name: 'Organization 21' })).toBeDefined();
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
      await fireEvent.click(screen.getByRole('button', { name: 'Organization 21' }));
      const name = await screen.findByRole('textbox', { name: 'Organization name' });
      await waitFor(() => expect(name).toHaveProperty('disabled', false));
      expect(name).toHaveProperty('value', 'Synthetic Org');
      expect(screen.getByRole('dialog', { name: 'Edit Synthetic Org' })).toBeDefined();
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

    expect(screen.getByRole('alert').textContent).toContain('reconciliation failed');
    expect(screen.getByText(/Locally updated role/)).toBeDefined();
    expect(screen.getByRole('button', { name: 'Edit employment' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'End employment' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Delete employment' })).toBeDefined();
    const refresh = screen.getByRole('button', { name: 'Refresh employment records' });
    await fireEvent.click(refresh);

    await waitFor(() => expect(reads).toBe(1));
    expect(screen.getByRole('status').textContent).toContain('Loading employment records');
    expect(screen.getByText(/Locally updated role/)).toBeDefined();
    expect(screen.getByRole('button', { name: 'Edit employment' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'End employment' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Delete employment' })).toBeDefined();

    resolveRefresh(collection([employment(12, 2, { title: 'Recovered role' })], undefined));
    expect(screen.queryByRole('alert')).toBeNull();
    expect(await screen.findByText(/Recovered role/)).toBeDefined();
  });

  it.each([false, true])('creates work for the selected person and employer with user provenance (search beyond fifty=%s)', async search => {
    const requests: Request[] = [];
    const queries: string[] = [];
    const employerID = search ? 51 : 21;
    const employerName = search ? 'Zebra Example' : 'Synthetic Org';
    let employmentReads = 0;
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/organizations') {
        const query = new URL(request.url).searchParams.get('q') ?? ''; queries.push(query);
        return Response.json({ organizations: search && query !== 'Zebra' ? Array.from({ length: 50 }, (_, index) => organization(index + 1, 1, `Example ${index + 1}`)) : [organization(employerID, 1, employerName)], total: search ? 51 : 1, limit: 50, offset: 0 });
      }
      if (path === '/api/v1/employments' && request.method === 'POST') {
        return new Response(JSON.stringify(employment(12, 1, { organization_id: employerID, title: 'Architect', is_primary: true })), { status: 201, headers: { ETag: '"employment-12-r1"' } });
      }
      if (path === '/api/v1/people/7/employments') {
        employmentReads += 1;
        return collection([employment(11, 2, { is_primary: false }), employment(12, 1, { organization_id: employerID, title: 'Architect', is_primary: true })], {
          employment_id: 12, organization_id: employerID, organization_name: employerName, title: 'Architect', vcard: {}
        });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.employments = [employment(11, 1, { is_primary: true })];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Add work' }));
    await screen.findByRole('button', { name: /^Organization(:|$)/ });
    const input = await openTypeahead('Organization');
    if (search) await fireEvent.input(input, { target: { value: 'Zebra' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: employerName }));
    if (search) {
      await screen.findByRole('button', { name: `Organization: ${employerName}` });
      await fireEvent.input(await openTypeahead('Organization'), { target: { value: 'Example' } });
      await waitFor(() => expect(queries).toContain('Example'));
      await fireEvent.keyDown(screen.getByRole('combobox', { name: 'Organization' }), { key: 'Escape' });
      expect(screen.getByRole('button', { name: `Organization: ${employerName}` })).toBeDefined();
    }
    await fireEvent.input(screen.getByRole('textbox', { name: 'Employment title' }), { target: { value: 'Architect' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create employment' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Add employment' })).toBeNull());
    const post = requests.find((request) => pathOf(request) === '/api/v1/employments' && request.method === 'POST')!;
    const postedBody = await post.json();
    expect(postedBody).toMatchObject({ person_id: 7, organization_id: employerID, source: 'user', title: 'Architect' });
    expect(postedBody.source_ref).toBeUndefined();
    expect(employmentReads).toBe(1);
    expect(screen.getByText(/Architect/).closest('.detail-row')?.textContent).toContain('Primary');
    expect(screen.getByText(/Engineer/).closest('.detail-row')?.textContent).not.toContain('Primary');
  });

  it('rejects a year-less employment date and keeps Create disabled until it is fixed', async () => {
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (pathOf(request) === '/api/v1/organizations') return Response.json({ organizations: [organization()], total: 1, limit: 50, offset: 0 });
      throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
    }));
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Add work' }));
    await screen.findByRole('button', { name: /^Organization(:|$)/ });
    await openTypeahead('Organization');
    await fireEvent.mouseDown(await screen.findByRole('option', { name: 'Synthetic Org' }));
    const create = screen.getByRole('button', { name: 'Create employment' });
    expect(create).toHaveProperty('disabled', false);

    const start = screen.getByRole('textbox', { name: 'Employment start date' });
    await fireEvent.input(start, { target: { value: '--04-12' } });
    const message = screen.getByText('Use a year, year and month, or full date, like 2019, 2019-04, or 2019-04-12.');
    expect(start.getAttribute('aria-describedby')).toBe(message.id);
    expect(create).toHaveProperty('disabled', true);

    await fireEvent.input(start, { target: { value: '20190412' } });
    expect(screen.queryByText(/Use a year/)).toBeNull();
    expect(start.getAttribute('aria-describedby')).toBeNull();
    expect(create).toHaveProperty('disabled', false);

    const end = screen.getByRole('textbox', { name: 'Employment end date' });
    await fireEvent.input(end, { target: { value: '2019-13' } });
    expect(screen.getByText('Use a year, year and month, or full date, like 2019, 2019-04, or 2019-04-12.').id).toBe(end.getAttribute('aria-describedby'));
    expect(create).toHaveProperty('disabled', true);
  });

  it('uses a fresh employment ETag, retains a conflict draft, and retries only after another explicit save', async () => {
    const requests: Request[] = [];
    let reads = 0;
    let patches = 0;
    const hiddenByRead = [
      { person_id: 7, organization_id: 21, source: 'user', source_ref: 'modal-old', address_id: 11, confidence: 0.11 },
      { person_id: 8, organization_id: 22, source: 'vcard_import', source_ref: 'first-fresh', address_id: 22, confidence: 0.22 },
      { person_id: 9, organization_id: 23, source: 'extraction', source_ref: 'conflict-current', address_id: 33, confidence: 0.33 },
      { person_id: 10, organization_id: 24, source: 'enrichment', source_ref: 'retry-fresh', address_id: 44, confidence: 0.44 }
    ];
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') {
        reads += 1;
        return new Response(JSON.stringify(employment(11, reads + 1, {
          ...hiddenByRead[reads - 1],
          title: reads === 1 ? 'Current title' : 'Changed title',
          department: reads === 1 ? 'Platform' : 'Concurrent research',
          location: reads === 1 ? 'Remote' : 'Concurrent campus'
        })), {
          headers: { ETag: `"employment-11-r${reads + 1}"` }
        });
      }
      if (path === '/api/v1/organizations/21' && request.method === 'GET') {
        return new Response(JSON.stringify(organizationProfile(4, 'Exact Employment Org')), { headers: { ETag: '"organization-21-r4"' } });
      }
      if (path === '/api/v1/organizations/23' && request.method === 'GET') {
        return new Response(JSON.stringify({ ...organizationProfile(6, 'Concurrent Employment Org'), organization: organization(23, 6, 'Concurrent Employment Org') }), { headers: { ETag: '"organization-23-r6"' } });
      }
      if (path === '/api/v1/employments/11' && request.method === 'PATCH') {
        patches += 1;
        if (patches === 1) return Response.json({ error: 'revision_conflict', message: 'stale revision' }, { status: 412 });
        return new Response(JSON.stringify(employment(11, 6, { ...hiddenByRead[3], title: 'Draft title' })), { headers: { ETag: '"employment-11-r6"' } });
      }
      if (path === '/api/v1/people/7/employments') return collection([employment(11, 6, { ...hiddenByRead[3], title: 'Draft title' })], {
        employment_id: 11, organization_id: 24, organization_name: 'Retry Employment Org', title: 'Draft title', vcard: {}
      });
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.employments = [employment()];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Edit employment' }));
    const title = await screen.findByRole('textbox', { name: 'Employment title' }) as HTMLInputElement;
    expect(screen.queryByRole('button', { name: 'New organization' })).toBeNull();
    await fireEvent.input(title, { target: { value: 'Draft title' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save employment' }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('changed elsewhere');
    expect(alert.textContent).toContain('Changed title');
    expect(alert.textContent).toContain('Concurrent research');
    expect(alert.textContent).toContain('Concurrent campus');
    expect(alert.textContent).toContain('Concurrent Employment Org (23)');
    expect(alert.textContent).not.toContain('Exact Employment Org (23)');
    expect(alert.textContent).toContain('Address ID 33');
    expect(alert.textContent).toContain('0.33');
    expect(title.value).toBe('Draft title');
    expect(patches).toBe(1);
    const firstPatch = requests.find((request) => request.method === 'PATCH')!;
    expect(firstPatch.headers.get('If-Match')).toBe('"employment-11-r3"');
    expect(await firstPatch.clone().json()).toMatchObject({ ...hiddenByRead[1], title: 'Draft title' });

    await waitFor(() => expect(screen.getByRole('button', { name: 'Save employment' })).toHaveProperty('disabled', false));
    await fireEvent.click(screen.getByRole('button', { name: 'Save employment' }));
    await waitFor(() => expect(patches).toBe(2));
    const retryPatch = requests.filter((request) => request.method === 'PATCH')[1]!;
    expect(retryPatch.headers.get('If-Match')).toBe('"employment-11-r5"');
    expect(await retryPatch.clone().json()).toMatchObject({ ...hiddenByRead[3], title: 'Draft title' });
  });

  it('loads the exact organization label for a disabled existing-employment selector', async () => {
    const requests: Request[] = [];
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11') return new Response(JSON.stringify(employment()), { headers: { ETag: '"employment-11-r1"' } });
      if (path === '/api/v1/organizations/21') return new Response(JSON.stringify(organizationProfile(2, 'Exact Employment Org')), { headers: { ETag: '"organization-21-r2"' } });
      if (path === '/api/v1/people/7/employments') return collection([employment()]);
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.employments = [employment()];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Edit employment' }));
    const selector = await screen.findByRole('button', { name: /^Organization(:|$)/ });
    expect(selector.textContent).toContain('Exact Employment Org');
    expect(selector.textContent).not.toContain('Choose an organization');
    await fireEvent.click(screen.getByRole('button', { name: 'Save employment' }));
    await waitFor(() => expect(requests.some((request) => request.method === 'PATCH')).toBe(true));
    expect(await requests.find((request) => request.method === 'PATCH')!.json()).toMatchObject({ organization_id: 21 });
  });

  it('reads a row as title at organization with period, department, and ordered actions', () => {
    const { controller } = controllerWith(vi.fn<typeof fetch>());
    controller.organizations = [organization()];
    controller.employments = [employment(11, 1, { title: 'Engineer', department: 'Platform', start_date: { year: 2024, month: 3 }, is_current: true, is_primary: false }), employment(12, 1, { is_current: false, is_primary: false, title: 'Historical role' })];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    const historical = screen.getByText(/Historical role/).closest('.detail-row') as HTMLElement;
    expect(within(historical).queryByRole('button', { name: 'Make primary employment' })).toBeNull();
    const row = screen.getByText(/Engineer/).closest('.detail-row') as HTMLElement;
    expect(row.textContent).toContain('Engineer at Synthetic Org');
    expect(row.textContent).toContain('Mar 2024 – present · Platform');
    expect(within(row).queryByText('Current')).toBeNull();
    expect(within(row).getAllByRole('button').map((button) => button.getAttribute('aria-label') ?? button.textContent?.trim()).slice(-4)).toEqual(
      ['Make primary employment', 'End employment', 'Edit employment', 'Delete employment']
    );
  });

  it('makes a current employment primary', async () => {
    const requests: Request[] = [];
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') return new Response(JSON.stringify(employment()), { headers: { ETag: '"employment-11-r1"' } });
      if (path === '/api/v1/employments/11/primary') return new Response(JSON.stringify(employment(11, 2, { is_primary: true })), { headers: { ETag: '"employment-11-r2"' } });
      if (path === '/api/v1/people/7/employments') return collection([employment(11, 2, { is_primary: true }), employment(12, 2, { title: 'Other role', is_primary: false })]);
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.employments = [employment(), employment(12, 1, { title: 'Other role', is_primary: true })];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Make primary employment' }));
    await waitFor(() => expect(screen.getByText(/Engineer/).closest('.detail-row')?.textContent).toContain('Primary'));
    expect(requests.find((request) => pathOf(request) === '/api/v1/employments/11/primary')?.headers.get('If-Match')).toBe('"employment-11-r1"');
  });

  it('retains a make-primary conflict and renders the complete refreshed employment', async () => {
    let reads = 0;
    let primaryWrites = 0;
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') {
        reads += 1;
        return new Response(JSON.stringify(employment(11, reads + 1, reads === 1 ? {} : {
          organization_id: 22,
          role: 'Concurrent role',
          department: 'Concurrent primary department',
          address_id: 66,
          confidence: 0.66,
          source: 'archive_observation',
          source_ref: 'concurrent-primary'
        })), { headers: { ETag: `"employment-11-r${reads + 1}"` } });
      }
      if (path === '/api/v1/employments/11/primary') {
        primaryWrites += 1;
        return Response.json({ error: 'revision_conflict', message: 'primary changed elsewhere' }, { status: 412 });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.organizations = [organization()];
    controller.employments = [employment()];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Make primary employment' }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('primary changed elsewhere');
    expect(alert.textContent).toContain('Organization 22');
    expect(alert.textContent).not.toContain('Synthetic Org (22)');
    expect(alert.textContent).toContain('Concurrent role');
    expect(alert.textContent).toContain('Concurrent primary department');
    expect(alert.textContent).toContain('Address ID 66');
    expect(alert.textContent).toContain('0.66');
    expect(alert.textContent).toContain('archive_observation');
    expect(alert.textContent).toContain('concurrent-primary');
    expect(primaryWrites).toBe(1);
  });

  it('supports end and delete with reconciled sibling state', async () => {
    const requests: Request[] = [];
    let revision = 1;
    let deleted = false;
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') {
        return new Response(JSON.stringify(employment(11, revision)), { headers: { ETag: `"employment-11-r${revision}"` } });
      }
      if (path === '/api/v1/employments/11/end') {
        revision += 1;
        return new Response(JSON.stringify(employment(11, revision, { is_current: false, end_date: { year: 2026, month: 8 } })), { headers: { ETag: `"employment-11-r${revision}"` } });
      }
      if (path === '/api/v1/employments/11' && request.method === 'DELETE') { deleted = true; return new Response(null, { status: 204 }); }
      if (path === '/api/v1/people/7/employments') {
        if (deleted) return Response.json({ employments: [] });
        return Response.json({ employments: [employment(11, revision, { is_current: false })] });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.employments = [employment()];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'End employment' }));
    const endDate = await screen.findByRole('textbox', { name: 'Employment end date' });
    await waitFor(() => expect(endDate).toHaveProperty('disabled', false));
    await fireEvent.input(endDate, { target: { value: '2026-08' } });
    await fireEvent.click(screen.getByRole('button', { name: 'End' }));
    await waitFor(() => expect(requests.some((request) => pathOf(request) === '/api/v1/employments/11/end')).toBe(true));
    expect((await requests.find((request) => pathOf(request) === '/api/v1/employments/11/end')!.json())).toEqual({ end_date: '2026-08' });
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'End employment' })).toBeNull());

    await fireEvent.click(screen.getByRole('button', { name: 'Delete employment' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Confirm delete employment' }));
    await waitFor(() => expect(screen.getByText('No work yet')).toBeDefined());
    expect(requests.find((request) => request.method === 'DELETE')?.headers.get('If-Match')).toBe('"employment-11-r2"');
  });

  it('blocks an unknown employment create until explicit refresh and keeps the modal draft open', async () => {
    let posts = 0;
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/organizations') return Response.json({ organizations: [organization()], total: 1, limit: 50, offset: 0 });
      if (path === '/api/v1/employments') {
        posts += 1;
        throw new TypeError('response lost');
      }
      if (path === '/api/v1/people/7/employments') return collection([] , undefined);
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Add work' }));
    await screen.findByRole('button', { name: /^Organization(:|$)/ });
    await openTypeahead('Organization');
    await fireEvent.mouseDown(await screen.findByRole('option', { name: 'Synthetic Org' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Employment title' }), { target: { value: 'Draft role' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create employment' }));
    expect((await screen.findByRole('alert')).textContent).toContain('outcome is unknown');
    expect((screen.getByRole('textbox', { name: 'Employment title' }) as HTMLInputElement).value).toBe('Draft role');
    expect(screen.getByRole('button', { name: 'Create employment' })).toHaveProperty('disabled', true);
    expect(posts).toBe(1);

    await fireEvent.click(screen.getByRole('button', { name: 'Refresh employment records' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Create employment' })).toHaveProperty('disabled', false));
    expect(posts).toBe(1);
  });

  it('keeps a conflicted employment delete open with complete current data and owns modal shortcuts', async () => {
    let reads = 0;
    let deletes = 0;
    const rootShortcut = vi.fn();
    const unregister = appShortcuts.register('x', rootShortcut);
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') {
        reads += 1;
        return new Response(JSON.stringify(employment(11, reads + 1, {
          department: 'Concurrent department', location: 'Concurrent office', description: 'Concurrent description', address_id: 88, confidence: 0.88
        })), { headers: { ETag: `"employment-11-r${reads + 1}"` } });
      }
      if (path === '/api/v1/employments/11' && request.method === 'DELETE') {
        deletes += 1;
        return deletes === 1 ? Response.json({ error: 'revision_conflict', message: 'stale employment' }, { status: 412 }) : new Response(null, { status: 204 });
      }
      if (path === '/api/v1/people/7/employments') return Response.json({ employments: [] });
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.employments = [employment()];
    const view = render(OrganizationEmploymentTab, { controller, personID: 7 });
    try {
      await fireEvent.click(screen.getByRole('button', { name: 'Delete employment' }));
      expect(appShortcuts.activeScope()).toBe('directory-employment-delete');
      appShortcuts.handleKeydown(new KeyboardEvent('keydown', { key: 'x', cancelable: true }));
      expect(rootShortcut).not.toHaveBeenCalled();
      await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete employment' }));

      const dialog = await screen.findByRole('dialog', { name: 'Delete employment' });
      const alert = await within(dialog).findByRole('alert');
      expect(alert.textContent).toContain('Concurrent department');
      expect(alert.textContent).toContain('Concurrent office');
      expect(alert.textContent).toContain('Concurrent description');
      expect(alert.textContent).toContain('Address ID 88');
      expect(alert.textContent).toContain('0.88');
      expect(deletes).toBe(1);

      window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));
      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Delete employment' })).toBeNull());
      expect(appShortcuts.activeScope()).toBe('root');
    } finally {
      view.unmount();
      unregister();
    }
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
