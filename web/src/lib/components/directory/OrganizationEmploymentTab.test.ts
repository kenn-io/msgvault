import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import { appShortcuts } from '@kenn-io/kit-ui';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { DirectoryEntityController } from '../../directory/entity-controller.svelte';
import { chooseSelectOption, openTypeahead } from '../../../test/kit-ui';
import { requestOf, pathOf, organization, organizationProfile, employment, collection, controllerWith } from '../../directory/entity-fixtures.test-support';
import OrganizationEmploymentTab from './OrganizationEmploymentTab.svelte';

async function openInlineEmployer(controller: DirectoryEntityController, name: string): Promise<void> {
  render(OrganizationEmploymentTab, { controller, personID: 7 });
  await fireEvent.click(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!);
  await screen.findByRole('dialog', {name:'Add job'});
  const query = await openTypeahead('Organization');
  await fireEvent.input(query, { target: { value: name } });
  await fireEvent.mouseDown(await screen.findByRole('option', { name: `Create organization “${name}”` }));
}

async function rowAction(label: string): Promise<void> {
  await fireEvent.click(screen.getAllByRole('button', { name: /^Actions for /  })[0]);
  await fireEvent.click(await screen.findByRole('menuitem', { name: label }));
}

describe('OrganizationEmploymentTab', () => {
  it('creates an employer inline and reuses it after employment creation fails', async () => {
    let employers = 0;
    let jobs = 0;
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/organizations' && request.method === 'POST') {
        employers++;
        expect(await request.json()).toMatchObject({ name: 'New Example', primary_domain: 'example.org', kind: 'school' });
        return Response.json(organization(22, 1, 'New Example'), { headers: { ETag: '"organization-22-r1"' } });
      }
      if (path === '/api/v1/organizations') return Response.json({ organizations: [], total: 0 });
      if (path === '/api/v1/employments') {
        jobs++;
        const body = await request.json();
        expect(body).toMatchObject({ organization_id: 22, title: 'Draft role', is_current: true });
        expect(body).not.toHaveProperty('end_date');
        if (jobs === 1) return Response.json({ message: 'Invalid employment' }, { status: 400 });
        return Response.json(employment(11, 1, { organization_id: 22 }), { headers: { ETag: '"employment-11-r1"' } });
      }
      if (path === '/api/v1/people/7/employments') return collection([]);
      throw new Error(`unexpected ${request.method} ${path}`);
    });
    await openInlineEmployer(controller, 'New Example');
    expect(screen.getAllByRole('dialog')).toHaveLength(1);
    await chooseSelectOption(screen.getByRole('combobox', { name: 'Organization kind: Company' }), 'School');
    await fireEvent.input(screen.getByRole('textbox', { name: 'Domain' }), { target: { value: 'example.org' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Job title' }), { target: { value: 'Draft role' } });
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Current' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'End date' }), { target: { value: '2024-13' } });
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Current' }));
    expect(screen.getByRole('textbox', { name: 'End date' })).toHaveProperty('disabled', true);
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!);
    await screen.findByText('Invalid employment');
    expect(screen.getByRole('textbox', { name: 'Job title' })).toHaveProperty('value', 'Draft role');
    expect(screen.getByRole('button', { name: /^Organization(:|$)/ }).textContent).toContain('New Example');
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!);
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(employers).toBe(1);
    expect(jobs).toBe(2);
  });

  it('offers to create an organization only once there is a name to create', async () => {
    const { controller } = controllerWith(async input => {
      const path = pathOf(requestOf(input));
      if (path === '/api/v1/organizations') return Response.json({ organizations: [organization(21, 1, 'Lumen Grove')], total: 1 });
      if (path === '/api/v1/people/7/employments') return collection([]);
      throw new Error(`unexpected ${path}`);
    });
    render(OrganizationEmploymentTab, { controller, personID: 7 });
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!);
    await screen.findByRole('dialog', { name: 'Add job' });
    await openTypeahead('Organization');
    await screen.findByRole('option', { name: /Lumen Grove/ });
    expect(screen.queryByRole('option', { name: /^Create organization/ })).toBeNull();
    await fireEvent.mouseDown(screen.getByRole('option', { name: /Lumen Grove/ }));
    const nextQuery = await openTypeahead('Organization');
    await fireEvent.input(nextQuery, { target: { value: 'Fern Hollow' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: 'Create organization “Fern Hollow”' }));
    expect(screen.getByRole('textbox', { name: 'Name' })).toHaveProperty('value', 'Fern Hollow');
    // The Name field holds the new name, so the select says only that it's new.
    const dialog = screen.getByRole('dialog', { name: 'Add job' });
    expect(within(dialog).getByRole('button', { name: 'Organization: New organization' })).toHaveProperty('disabled', true);
    expect(dialog.textContent).not.toContain('Create organization');
    await fireEvent.click(screen.getByRole('button', { name: 'Choose an existing organization instead' }));
    expect(screen.queryByRole('textbox', { name: 'Name' })).toBeNull();
  });

  it('rejects an end date before the start before creating an employer', async () => {
    const requests: Request[] = [];
    const { controller } = controllerWith(async input => {
      const request = requestOf(input); requests.push(request);
      return Response.json({ organizations: [], total: 0 });
    });
    await openInlineEmployer(controller, 'Example School');
    await fireEvent.input(screen.getByRole('textbox', { name: 'Start date' }), { target: { value: '2025-06' } });
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Current' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'End date' }), { target: { value: '2024-06' } });
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!);
    expect(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!).toHaveProperty('disabled', true);
    expect(requests.filter(request => request.method === 'POST')).toHaveLength(0);
  });

  it('blocks an unknown inline employer create until organizations are refreshed', async () => {
    let employers = 0;
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (request.method === 'POST') { employers++; return Response.json({ message: 'Unavailable' }, { status: 503 }); }
      return Response.json({ organizations: [], total: 0 });
    });
    await openInlineEmployer(controller, 'Example company');
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!);
    await screen.findByRole('alert');
    expect(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!).toHaveProperty('disabled', true);
    await fireEvent.click(screen.getByRole('button', { name: 'Check again' }));
    await waitFor(() => expect(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!).toHaveProperty('disabled', false));
    expect(employers).toBe(1);
    expect(screen.getByRole('textbox', { name: 'Name' })).toHaveProperty('value', 'Example company');
  });

  it('prefills the local day across UTC midnight and retains an End conflict', async () => {
    vi.stubEnv('TZ', 'America/New_York');
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(new Date('2026-11-05T04:30:00Z'));
    let reads = 0;
    const opening = employment(11, 1, { start_date: { year: 2024, month: 3 } });
    const saved = employment(11, 2, { start_date: { year: 2024, month: 3 }, title: 'Current saved role', organization_id: 22 });
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      if (request.method === 'POST') return Response.json({ message: 'Concurrent end', error: 'revision_conflict' }, { status: 412 });
      if (pathOf(request) === '/api/v1/organizations/22') {
        return Response.json({ organization: organization(22, 1, 'Current Employer'), names: [], categories: [], contact_points: [], addresses: [], identifiers: [], media: [] }, { headers: { ETag: '"organization-22-r1"' } });
      }
      if (pathOf(request) === '/api/v1/people/7/employments') return collection([saved]);
      reads++;
      return Response.json(reads === 1 ? opening : saved, { headers: { ETag: `"employment-11-r${reads}"` } });
    });
    controller.employments = [opening];
    const view = render(OrganizationEmploymentTab, { controller, personID: 7 });
    try {
      await rowAction('End…');
      const field = await screen.findByRole('textbox', { name: 'End date' });
      await waitFor(() => expect(field).toHaveProperty('disabled', false));
      expect(field).toHaveProperty('value', '2026-11-04');
      expect(screen.getByRole('dialog', { name: 'End job' })).toBeDefined();
      expect(screen.getByText(/ at .+ · since Mar 2024$/)).toBeDefined();
      await fireEvent.input(field, { target: { value: '2026' } });
      await fireEvent.click(screen.getByRole('button', { name: 'End' }));
      // Only End date is on this form; the title changed elsewhere is summarized, its value in Details.
      const changes = await screen.findByRole('table', { name: 'What changed' });
      expect(within(changes).getByRole('rowheader', { name: 'End date' })).toBeDefined();
      expect(changes.textContent).toContain('Present');
      expect(screen.getByText('Other details also changed')).toBeDefined();
      expect(screen.getByText('Job title', { selector: 'dt' }).nextElementSibling?.textContent).toBe('Current saved role');
      expect(field).toHaveProperty('value', '2026');
      expect(reads).toBe(2);
      await fireEvent.keyDown(document, { key: 'Escape' });
      await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
      expect(screen.getByRole('button', { name: /^Actions for /  })).toBeDefined();
      await fireEvent.mouseDown(document.body);
      await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    } finally { vi.useRealTimers(); vi.unstubAllEnvs(); view.unmount(); controller.destroy(); }
  });

  it.each([true, false])('explains permanent deletion for current=%s employment', async current => {
    const { controller } = controllerWith(vi.fn<typeof fetch>());
    controller.employments = [employment(11, 1, { is_current: current })];
    render(OrganizationEmploymentTab, { controller, personID: 7 });
    if (!controller.employments.some(job => job.is_current)) await fireEvent.click(screen.getByText(/^\d+ past jobs?$/));
    await rowAction('Delete…');
    const dialog = screen.getByRole('dialog', { name: /^Delete .*\?/  });
    expect(dialog.textContent).toContain('This permanently removes the job.');
    expect(dialog.textContent?.includes('End it instead')).toBe(current);
    expect(within(dialog).getByRole('button', { name: 'Close' })).toBeDefined();
  });

  it('keeps the delete step active while its write is pending', async () => {
    let release!: (response: Response) => void;
    const pending = new Promise<Response>(resolve => { release = resolve; });
    const { controller } = controllerWith(async input => {
      const request = requestOf(input), path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') return Response.json(employment(), { headers: { ETag: '"employment-11-r1"' } });
      if (path === '/api/v1/employments/11' && request.method === 'DELETE') return pending;
      if (path === '/api/v1/people/7/employments') return collection([]);
      throw new Error(`unexpected ${request.method} ${path}`);
    });
    controller.employments = [employment()];
    const rendered = render(OrganizationEmploymentTab, { controller, personID: 7 });
    try {
      await rowAction('Delete…');
      await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
      const alternative = screen.getByRole('button', { name: 'End it instead…' });
      expect(alternative).toHaveProperty('disabled', true);
      expect(screen.getAllByRole('dialog')).toHaveLength(1);
      expect(screen.queryByRole('dialog', { name: 'End job' })).toBeNull();
      release(new Response(null, { status: 204 }));
      await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
      expect(controller.employments).toEqual([]);
    } finally { release(new Response(null, { status: 204 })); rendered.unmount(); controller.destroy(); }
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

  it('leads a blocked organization delete to the job blocking it, without returning to the organization', async () => {
    let jobs = [employment()];
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/organizations/21') return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
      if (path === '/api/v1/organizations/21/employments') return Response.json({ employments: jobs });
      if (path === '/api/v1/people/7') return Response.json({ display_name: 'Example Person' });
      if (path === '/api/v1/employments/11' && request.method === 'GET') return Response.json(employment(), { headers: { ETag: '"employment-11-r1"' } });
      if (path === '/api/v1/employments/11' && request.method === 'DELETE') { jobs = []; return new Response(null, { status: 204 }); }
      if (path === '/api/v1/people/7/employments') return collection(jobs);
      throw new Error(`unexpected ${request.method} ${path}`);
    });
    controller.employments = [employment()];
    const view = render(OrganizationEmploymentTab, { controller, personID: 7 });
    try {
      await fireEvent.click(screen.getByRole('button', { name: 'this organization' }));
      await fireEvent.click(await screen.findByRole('button', { name: 'Delete organization…' }));
      await screen.findByText("Delete its job first. Ending it isn't enough.");
      expect(screen.queryByText(/has 1 job/)).toBeNull();
      await fireEvent.click(screen.getByRole('button', { name: 'Delete job…' }));
      const confirm = await screen.findByRole('dialog', { name: 'Delete Engineer at Synthetic Org?' });
      await fireEvent.click(within(confirm).getByRole('button', { name: 'Delete' }));
      await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
      expect(controller.employments).toEqual([]);
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

    await fireEvent.click(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!);
    await screen.findByRole('button', { name: /^Organization(:|$)/ });
    const input = await openTypeahead('Organization');
    if (search) await fireEvent.input(input, { target: { value: 'Zebra' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: new RegExp(employerName) }));
    if (search) {
      await screen.findByRole('button', { name: `Organization: ${employerName}` });
      await fireEvent.input(await openTypeahead('Organization'), { target: { value: 'Example' } });
      await waitFor(() => expect(queries).toContain('Example'));
      await fireEvent.keyDown(screen.getByRole('combobox', { name: 'Organization' }), { key: 'Escape' });
      expect(screen.getByRole('button', { name: `Organization: ${employerName}` })).toBeDefined();
    }
    await fireEvent.input(screen.getByRole('textbox', { name: 'Job title' }), { target: { value: 'Architect' } });
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!);

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Add job' })).toBeNull());
    const post = requests.find((request) => pathOf(request) === '/api/v1/employments' && request.method === 'POST')!;
    const postedBody = await post.json();
    expect(postedBody).toMatchObject({ person_id: 7, organization_id: employerID, source: 'user', title: 'Architect' });
    expect(postedBody.source_ref).toBeUndefined();
    expect(employmentReads).toBe(1);
    expect(screen.getByText(/Architect/).closest('.detail-row')?.textContent).toContain('Primary');
    expect(screen.getByText(/Engineer/).closest('.detail-row')?.textContent).not.toContain('Primary');
  });

  it('rejects a year-less employment date and keeps Create disabled until it is fixed', async () => {
    const requests: Request[] = [];
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input); requests.push(request);
      if (pathOf(request) === '/api/v1/organizations') return Response.json({ organizations: [organization()], total: 1, limit: 50, offset: 0 });
      throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
    }));
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await fireEvent.click(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!);
    await screen.findByRole('button', { name: /^Organization(:|$)/ });
    await openTypeahead('Organization');
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Synthetic Org/ }));
    const create = screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!;
    expect(create).toHaveProperty('disabled', false);

    const start = screen.getByRole('textbox', { name: 'Start date' });
    await fireEvent.input(start, { target: { value: '--04-12' } });
    expect(create).toHaveProperty('disabled', true);
    await fireEvent.click(create);
    expect(requests.filter(request => request.method === 'POST')).toHaveLength(0);

    await fireEvent.input(start, { target: { value: '20190412' } });
    expect(create).toHaveProperty('disabled', false);

    await fireEvent.click(screen.getByRole('checkbox', { name: 'Current' }));
    const end = screen.getByRole('textbox', { name: 'End date' });
    await fireEvent.input(end, { target: { value: '2019-13' } });
    expect(create).toHaveProperty('disabled', true);
    await fireEvent.click(create);
    expect(requests.filter(request => request.method === 'POST')).toHaveLength(0);
  });

  it.each(['keep mine', 'use saved'])('preserves fields from the saved job when resolving with %s', async (resolution) => {
    const requests: Request[] = [];
    let reads = 0;
    let patches = 0;
    const hiddenByRead = [
      { person_id: 7, organization_id: 21, source: 'user', source_ref: 'modal-old', address_id: 11, confidence: 0.11 },
      { person_id: 8, organization_id: 22, source: 'vcard_import', source_ref: 'first-fresh', address_id: 22, confidence: 0.22 },
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
      if (path === '/api/v1/organizations/22' && request.method === 'GET') {
        return new Response(JSON.stringify({ ...organizationProfile(6, 'Concurrent Employment Org'), organization: organization(22, 6, 'Concurrent Employment Org') }), { headers: { ETag: '"organization-22-r6"' } });
      }
      if (path === '/api/v1/employments/11' && request.method === 'PATCH') {
        patches += 1;
        if (patches === 1) return Response.json({ error: 'revision_conflict', message: 'stale revision' }, { status: 412 });
        return new Response(JSON.stringify(employment(11, 4, { ...hiddenByRead[1], title: 'Draft title' })), { headers: { ETag: '"employment-11-r4"' } });
      }
      if (path === '/api/v1/people/7/employments') return collection([]);
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.employments = [employment()];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await rowAction('Edit…');
    const title = await screen.findByRole('textbox', { name: 'Job title' }) as HTMLInputElement;
    expect(screen.queryByRole('button', { name: 'New organization' })).toBeNull();
    await fireEvent.input(title, { target: { value: 'Draft title' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    const alert = (await screen.findByRole('alert')).closest('.inline-notice')!;
    expect(alert.textContent).toContain('This job changed since you opened it.');
    expect(title.value).toBe('Draft title');
    expect(patches).toBe(1);
    const firstPatch = requests.find((request) => request.method === 'PATCH')!;
    expect(await firstPatch.clone().json()).toMatchObject({ ...hiddenByRead[0], title: 'Draft title' });

    expect(screen.getByRole('button', { name: 'Use saved' })).toBeDefined();
    if (resolution === 'use saved') {
      await fireEvent.click(screen.getByRole('button', { name: 'Use saved' }));
      await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
      expect(patches).toBe(1);
      expect(requests.some((request) => pathOf(request) === '/api/v1/people/7/employments')).toBe(true);
      return;
    }
    await waitFor(() => expect(screen.getByRole('button', { name: 'Keep yours' })).toHaveProperty('disabled', false));
    await fireEvent.click(screen.getByRole('button', { name: 'Keep yours' }));
    await waitFor(() => expect(patches).toBe(2));
    const retryPatch = requests.filter((request) => request.method === 'PATCH')[1]!;
    expect(await retryPatch.clone().json()).toMatchObject({ ...hiddenByRead[1], title: 'Draft title' });
  });

  it('shows the exact employer in the existing job context', async () => {
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

    await rowAction('Edit…');
    const context = await screen.findByText('At Exact Employment Org', { selector: 'p.context' });
    expect(context.textContent).toBe('At Exact Employment Org');
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(requests.some((request) => request.method === 'PATCH')).toBe(true));
    expect(await requests.find((request) => request.method === 'PATCH')!.json()).toMatchObject({ organization_id: 21 });
  });

  it('folds past jobs and keeps row actions beside linked employers', async () => {
    const { controller } = controllerWith(vi.fn<typeof fetch>());
    controller.organizations = [organization()];
    controller.employments = [employment(11, 1, { title: 'Engineer', department: 'Platform', start_date: { year: 2024, month: 3 }, is_current: true }), employment(12, 1, { is_current: false, title: 'Historical role' })];
    render(OrganizationEmploymentTab, { controller, personID: 7 });
    const row = screen.getByText(/Engineer at/).closest('.detail-row')!;
    expect(row.textContent).toContain('Mar 2024 – present · Platform');
    expect(within(row as HTMLElement).getByRole('button', { name: 'Synthetic Org' })).toBeDefined();
    await fireEvent.click(screen.getByText('1 past job'));
    expect(screen.getByText(/Historical role/)).toBeDefined();
    await rowAction('Edit…');
    expect(screen.getByRole('dialog', {name:'Edit job'})).toBeDefined();
  });

  it('adds the role to a titled job and lists past jobs most recent first', async () => {
    const { controller } = controllerWith(async () => Response.json({ organizations: [], total: 0 }));
    controller.employments = [
      employment(1, 1, { title: 'Garden designer', role: 'Design lead', department: 'Community gardens', start_date: { year: 2024, month: 3 } }),
      employment(2, 1, { title: 'Intern', is_current: false, start_date: { year: 2018 }, end_date: { year: 2019 } }),
      employment(3, 1, { title: 'Project assistant', is_current: false, start_date: { year: 2021 }, end_date: { year: 2023 } }),
    ];
    render(OrganizationEmploymentTab, { controller, personID: 7 });
    expect(screen.getByText('Garden designer', { exact: false }).closest('.detail-row')?.textContent).toContain('Mar 2024 – present · Design lead · Community gardens');
    await fireEvent.click(screen.getByText('2 past jobs'));
    const past = screen.getAllByText(/^(Intern|Project assistant) at/).map(row => row.textContent?.split(' at ')[0]);
    expect(past).toEqual(['Project assistant', 'Intern']);
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
    controller.employments = [employment()];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await rowAction('Make primary');
    await waitFor(() => expect(screen.getByText(/Engineer/).closest('.detail-row')?.textContent).toContain('Primary'));
    expect(requests.find((request) => pathOf(request) === '/api/v1/employments/11/primary')?.headers.get('If-Match')).toBe('"employment-11-r1"');
  });

  it('retains a make-primary conflict and renders the complete refreshed employment', async () => {
    let reads = 0;
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') {
        reads += 1;
        return new Response(JSON.stringify(employment(11, reads, reads === 1 ? {} : {
          organization_id: 22,
          role: 'Concurrent role',
          department: 'Concurrent primary department',
          address_id: 66,
          confidence: 0.66,
          source: 'archive_observation',
          source_ref: 'concurrent-primary'
        })), { headers: { ETag: `"employment-11-r${reads}"` } });
      }
      if (path === '/api/v1/organizations/22') return Response.json({ organization: organization(22, 1, 'Current Employer'), names: [], categories: [], contact_points: [], addresses: [], identifiers: [], media: [] }, { headers: { ETag: '"organization-22-r1"' } });
      if (path === '/api/v1/employments/11/primary') {
        return Response.json({ error: 'revision_conflict', message: 'primary changed elsewhere' }, { status: 412 });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.organizations = [organization()];
    controller.employments = [employment(), employment(12, 1, { organization_id: 23, title: 'Other role' })];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await rowAction('Make primary');

    const alert = (await screen.findByRole('alert')).closest('.inline-notice')!;
    expect(alert.textContent).toContain('This job changed since you opened it.');
    expect(alert.textContent).not.toContain('Synthetic Org (22)');
    expect(within(alert as HTMLElement).getByText('Role on the team', { selector: 'dt' }).nextElementSibling?.textContent).toBe('Concurrent role');
    expect(within(alert as HTMLElement).getByText('Department', { selector: 'dt' }).nextElementSibling?.textContent).toBe('Concurrent primary department');
    expect(within(alert as HTMLElement).getByText('Confidence').nextElementSibling?.textContent).toBe('66%');
    expect(within(alert as HTMLElement).getByText('Source').nextElementSibling?.textContent).toBe('From your messages');
    expect(within(alert as HTMLElement).getByText('Source reference').nextElementSibling?.textContent).toBe('concurrent-primary');
    expect(reads).toBe(2);
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
    controller.employments = [employment(11, 1, { start_date: { year: 2024, month: 3 } })];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await rowAction('End…');
    const endDate = await screen.findByRole('textbox', { name: 'End date' });
    await waitFor(() => expect(endDate).toHaveProperty('disabled', false));
    await fireEvent.input(endDate, { target: { value: '2024-02' } });
    expect(screen.getByText('End date must be on or after start date.')).toBeDefined();
    expect(screen.getByRole('button', { name: 'End' })).toHaveProperty('disabled', true);
    await fireEvent.input(endDate, { target: { value: '2026-08' } });
    await fireEvent.click(screen.getByRole('button', { name: 'End' }));
    await waitFor(() => expect(requests.some((request) => pathOf(request) === '/api/v1/employments/11/end')).toBe(true));
    expect((await requests.find((request) => pathOf(request) === '/api/v1/employments/11/end')!.json())).toEqual({ end_date: '2026-08' });
    await waitFor(() => expect(screen.queryByRole('dialog', { name: /^End /  })).toBeNull());

    if (!controller.employments.some(job => job.is_current)) await fireEvent.click(screen.getByText(/^\d+ past jobs?$/));
    await rowAction('Delete…');
    await fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(screen.getByText('No work yet')).toBeDefined());
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
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

    await fireEvent.click(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!);
    await screen.findByRole('button', { name: /^Organization(:|$)/ });
    await openTypeahead('Organization');
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Synthetic Org/ }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Job title' }), { target: { value: 'Draft role' } });
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!);
    expect((await screen.findByRole('alert')).textContent).toContain("Couldn't confirm the job was saved.");
    expect((screen.getByRole('textbox', { name: 'Job title' }) as HTMLInputElement).value).toBe('Draft role');
    expect(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!).toHaveProperty('disabled', true);
    expect(posts).toBe(1);

    await fireEvent.click(screen.getByRole('button', { name: 'Check again' }));
    await waitFor(() => expect(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!).toHaveProperty('disabled', false));
    expect(posts).toBe(1);
  });

  it('keeps a conflicted employment delete open with complete current data and owns modal shortcuts', async () => {
    let reads = 0;
    const deletes: Request[] = [];
    const rootShortcut = vi.fn();
    const unregister = appShortcuts.register('x', rootShortcut);
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') {
        reads += 1;
        return new Response(JSON.stringify(employment(11, reads, reads === 1 ? {} : {
          department: 'Concurrent department', location: 'Concurrent office', description: 'Concurrent description', address_id: 88, confidence: 0.88
        })), { headers: { ETag: `"employment-11-r${reads}"` } });
      }
      if (path === '/api/v1/employments/11' && request.method === 'DELETE') {
        deletes.push(request);
        return deletes.length === 1 ? Response.json({ error: 'revision_conflict', message: 'stale employment' }, { status: 412 }) : new Response(null, { status: 204 });
      }
      if (path === '/api/v1/people/7/employments') return collection([employment(11, 2, { department: 'Concurrent department', location: 'Concurrent office', description: 'Concurrent description', address_id: 88, confidence: 0.88 })]);
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.employments = [employment()];
    const view = render(OrganizationEmploymentTab, { controller, personID: 7 });
    try {
      await rowAction('Delete…');
      expect(appShortcuts.activeScope()).toBe('directory-employment-delete');
      appShortcuts.handleKeydown(new KeyboardEvent('keydown', { key: 'x', cancelable: true }));
      expect(rootShortcut).not.toHaveBeenCalled();
      await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));

      const dialog = await screen.findByRole('dialog', { name: /^Delete .*\?/  });
      const alert = (await within(dialog).findByRole('alert')).closest('.inline-notice')!;
      const changes = within(alert as HTMLElement).getByRole('table', { name: 'What changed' });
      expect(changes.textContent).toContain('Concurrent department');
      expect(changes.textContent).toContain('Concurrent office');
      expect(changes.textContent).toContain('Concurrent description');
      expect(within(changes).getByRole('rowheader', { name: 'Confidence' }).parentElement?.textContent).toContain('88%');
      expect(deletes.map(request => request.headers.get('If-Match'))).toEqual(['"employment-11-r1"']);
      expect(reads).toBe(2);

      window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));
      await waitFor(() => expect(screen.queryByRole('dialog', { name: /^Delete .*\?/  })).toBeNull());
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

it('keeps the Make primary conflict until Reload succeeds', async () => {
  let refreshes = 0;
  const saved = employment(11, 2, { title: 'Concurrent title' });
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (pathOf(request) === '/api/v1/people/7/employments') return ++refreshes === 1
      ? Response.json({ message: 'Refresh unavailable' }, { status: 503 })
      : collection([saved]);
    if (pathOf(request).startsWith('/api/v1/organizations/')) return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    if (request.method === 'GET') return Response.json(saved, { headers: { ETag: '"employment-11-r2"' } });
    return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
  });
  controller.employments = [employment()];
  render(OrganizationEmploymentTab, { controller, personID: 7 });
  await rowAction('Make primary');
  await fireEvent.click(await screen.findByRole('button', { name: 'Reload' }));
  await waitFor(() => expect(controller.employmentsLoading).toBe(false));
  await tick();
  expect(controller.errors.employments).toBe('Refresh unavailable');
  expect(screen.getByText('This job changed since you opened it.')).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Reload' }));
  await waitFor(() => expect(screen.queryByText('This job changed since you opened it.')).toBeNull());
  expect(refreshes).toBe(2);
});

it('uses the selected job version when Make primary moves to another row', async () => {
  const tags: (string | null)[] = [];
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (pathOf(request).endsWith('/primary')) {
      tags.push(request.headers.get('If-Match'));
      return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
    }
    const id = Number(pathOf(request).split('/').at(-1));
    const version = tags.length === 0 ? 1 : id === 12 && tags.length === 1 ? 2 : 3;
    return Response.json(employment(id, version), { headers: { ETag: `"employment-${id}-r${version}"` } });
  });
  controller.employments = [employment(), employment(12, 2)];
  render(OrganizationEmploymentTab, { controller, personID: 7 });
  await fireEvent.click(screen.getAllByRole('button', { name: /^Actions for / })[0]);
  await fireEvent.click(screen.getByRole('menuitem', { name: 'Make primary' }));
  await screen.findByText('This job changed since you opened it.');
  await fireEvent.click(screen.getAllByRole('button', { name: /^Actions for / })[1]);
  await fireEvent.click(screen.getByRole('menuitem', { name: 'Make primary' }));
  await waitFor(() => expect(tags).toHaveLength(2));
  expect(tags).toEqual(['"employment-11-r1"', '"employment-12-r2"']);
});

it('deletes the saved version a job delete conflict shows', async () => {
  const tags: (string | null)[] = [];
  const saved = employment(11, 2, { title: 'Concurrent title' });
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (pathOf(request) === '/api/v1/people/7/employments') return collection([]);
    if (pathOf(request).startsWith('/api/v1/organizations/')) return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    if (request.method === 'GET') return Response.json(tags.length ? saved : employment(), { headers: { ETag: tags.length ? '"employment-11-r2"' : '"employment-11-r1"' } });
    tags.push(request.headers.get('If-Match'));
    return tags.length === 1 ? Response.json({ message: 'Changed elsewhere' }, { status: 412 }) : new Response(null, { status: 204 });
  });
  controller.employments = [employment()];
  render(OrganizationEmploymentTab, { controller, personID: 7 });
  await rowAction('Delete…');
  await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
  await screen.findByText('This job changed since you opened it.');
  expect(screen.getByText('Concurrent title')).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  expect(tags).toEqual(['"employment-11-r1"', '"employment-11-r2"']);
});
