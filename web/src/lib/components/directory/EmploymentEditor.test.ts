import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { controllerWith, organization, organizationProfile, employment, collection, pathOf, requestOf } from '../../directory/entity-fixtures.test-support';
import EmploymentEditor from './EmploymentEditor.svelte';
import { chooseSelectOption, openTypeahead } from '../../../test/kit-ui';
import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';

async function openInlineEmployer(controller: DirectoryEntityController, name: string, onDone = () => undefined): Promise<void> {
  render(EmploymentEditor, { controller, personID: 7, onDone });
  const query = await openTypeahead('Organization');
  await fireEvent.input(query, { target: { value: name } });
  await fireEvent.mouseDown(await screen.findByRole('option', { name: `Create organization “${name}”` }));
}


it('passes organization search loading and failures to the employer picker', async () => {
  let finish!: (response: Response) => void;
  const pending = new Promise<Response>(resolve => { finish = resolve; });
  const { controller } = controllerWith(async input => {
    const query = new URL((input as Request).url).searchParams.get('q');
    if (!query) return pending;
    if (query === 'failed') return Response.json({ message: 'Organization search unavailable' }, { status: 500 });
    throw new Error(`Unexpected organization query: ${query}`);
  });
  controller.organizations = [organization()];
  const view = render(EmploymentEditor, { controller, personID: 7 });
  try {
    await fireEvent.keyDown(screen.getByRole('button', { name: /^Organization:/ }), { key: 'ArrowDown' });
    expect(await screen.findByText('Loading…')).toBeDefined();
    finish(Response.json({ organizations: [], total: 0, limit: 50, offset: 0 }));
    const search = screen.getByRole('combobox', { name: 'Organization' });
    await fireEvent.input(search, { target: { value: 'failed' } });
    expect((await screen.findByRole('alert')).textContent).toContain("Couldn't load organizations.");
    await fireEvent.click(screen.getByText('Details'));
    expect(screen.getByText('Organization search unavailable')).toBeDefined();
  } finally {
    finish(Response.json({ organizations: [], total: 0, limit: 50, offset: 0 }));
    view.unmount();
    controller.destroy();
  }
});

it.each(['first job', 'existing primary', 'no primary'])('sends primary only when offered for %s', async scenario => {
  let body: Record<string, unknown> | undefined;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'POST') { body = await request.json(); return Response.json(employment()); }
    return Response.json({ employments: [], organizations: [organization()] });
  });
  controller.organizations = [organization()];
  controller.employments = scenario === 'first job' ? [] : [employment(12, 1, { is_primary: scenario === 'existing primary' })];
  render(EmploymentEditor, { controller, personID: 7 });
  const checkbox = screen.queryByRole('checkbox', { name: 'Primary job' });
  if (scenario === 'no primary') {
    expect(checkbox).not.toBeNull();
    await fireEvent.click(checkbox!);
  } else expect(checkbox).toBeNull();
  await fireEvent.keyDown(screen.getByRole('button', { name: /^Organization:/ }), { key: 'ArrowDown' });
  await fireEvent.mouseDown(await screen.findByRole('option', { name: /Synthetic Org/ }));
  await fireEvent.click(screen.getByRole('button', { name: 'Add job' }));
  await waitFor(() => expect(body).toBeDefined());
  if (scenario === 'no primary') expect(body).toHaveProperty('is_primary', true);
  else expect(body).not.toHaveProperty('is_primary');
});

it('ends a primary job from Edit without overriding the stored primary rule', async () => {
  let body: Record<string, unknown> | undefined;
  const job = employment(11, 1, { is_primary: true });
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'PATCH') { body = await request.json(); return Response.json({ ...job, is_current: false, is_primary: false }); }
    if (request.url.endsWith('/organizations/21')) return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    if (request.url.endsWith('/employments/11')) return Response.json(job, { headers: { ETag: '"employment-11-r1"' } });
    return Response.json({ employments: [] });
  });
  render(EmploymentEditor, { controller, personID: 7, employment: job });
  const current = await screen.findByRole('checkbox', { name: 'Current' });
  await fireEvent.click(current);
  await fireEvent.input(screen.getByRole('textbox', { name: 'End date' }), { target: { value: '2026-11' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  await waitFor(() => expect(body).toBeDefined());
  expect(body).toHaveProperty('is_current', false);
  expect(body).not.toHaveProperty('is_primary');
});


it('allows editing a job after an unrelated organization create became uncertain', async () => {
  const { controller } = controllerWith(async input => {
    const path = new URL(requestOf(input).url).pathname;
    return path.includes('organizations')
      ? Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } })
      : Response.json(employment(), { headers: { ETag: '"employment-11-r1"' } });
  });
  controller.createBlocked.organizations = true;
  render(EmploymentEditor, { controller, personID: 7, employment: employment() });
  await screen.findByRole('textbox', { name: 'Job title' });
  expect(screen.getByRole('button', { name: 'Save' })).toHaveProperty('disabled', false);
  expect(screen.queryByText("Couldn't confirm the organization was saved.")).toBeNull();
});

it('saves against the opened job version and retries against the conflict version', async () => {
  let reads = 0;
  const tags: (string | null)[] = [];
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'PATCH') {
      tags.push(request.headers.get('If-Match'));
      return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
    }
    if (pathOf(request).includes('/organizations/')) return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    return Response.json(employment(11, ++reads), { headers: { ETag: `"employment-11-r${reads}"` } });
  });
  render(EmploymentEditor, { controller, personID: 7, employment: employment() });
  await screen.findByRole('textbox', { name: 'Job title' });
  await controller.prepareEmploymentMutation(11);
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  await fireEvent.click(await screen.findByRole('button', { name: 'Keep yours' }));
  await waitFor(() => expect(tags).toHaveLength(2));
  expect(tags).toEqual(['"employment-11-r1"', '"employment-11-r3"']);
});

it('describes a job moved to an unloaded person', async () => {
  let reads = 0;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'PATCH') return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
    if (pathOf(request).includes('/organizations/')) return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    const job = employment(11, ++reads, { person_id: reads === 1 ? 7 : 8 });
    return Response.json(job, { headers: { ETag: `"employment-11-r${reads}"` } });
  });
  render(EmploymentEditor, { controller, personID: 7, employment: employment() });
  await screen.findByRole('textbox', { name: 'Job title' });
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  await screen.findByRole('button', { name: 'Keep yours' });
  await fireEvent.click(screen.getByText('Details'));
  expect(screen.getByText('Person', { selector: 'dt' }).nextElementSibling?.textContent).toBe('another person');
});

it('hides the job form and Save after a failed load until Retry succeeds', async () => {
  let reads = 0;
  const writes: string[] = [];
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method !== 'GET') { writes.push(request.method); return Response.json(employment()); }
    if (pathOf(request).endsWith('/organizations/21')) return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    return ++reads === 1 ? Response.json({ message: 'Load unavailable' }, { status: 503 })
      : Response.json(employment(11, 2, { title: 'Saved title' }), { headers: { ETag: '"employment-11-r2"' } });
  });
  render(EmploymentEditor, { controller, personID: 7, employment: employment() });
  await screen.findByText("Couldn't load this job.");
  expect(screen.queryByRole('textbox', { name: 'Job title' })).toBeNull();
  expect(screen.queryByRole('button', { name: 'Save' })).toBeNull();
  expect(writes).toEqual([]);
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  await waitFor(() => expect(screen.getByRole('textbox', { name: 'Job title' })).toHaveProperty('value', 'Saved title'));
  expect(screen.getByRole('button', { name: 'Save' })).toHaveProperty('disabled', false);
});

it('allows closing while the job loads', async () => {
  let finish!: (response: Response) => void;
  const pending = new Promise<Response>(resolve => { finish = resolve; });
  const { controller } = controllerWith(() => pending);
  let closed = false;
  const view = render(EmploymentEditor, { controller, personID: 7, employment: employment(), onClose: () => { closed = true; } });
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(closed).toBe(true);
  } finally { view.unmount(); finish(Response.json(employment(), { headers: { ETag: '"employment-11-r1"' } })); controller.destroy(); }
});

  it('creates an employer inline and reuses it after employment creation fails', async () => {
    let employers = 0;
    let jobs = 0;
    const employerBodies: unknown[] = [];
    const jobBodies: Record<string, unknown>[] = [];
    const { controller } = controllerWith(async input => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/organizations' && request.method === 'POST') {
        employers++;
        employerBodies.push(await request.json());
        return Response.json(organization(22, 1, 'New Example'), { headers: { ETag: '"organization-22-r1"' } });
      }
      if (path === '/api/v1/organizations') return Response.json({ organizations: [], total: 0 });
      if (path === '/api/v1/employments') {
        jobs++;
        jobBodies.push(await request.json());
        if (jobs === 1) return Response.json({ message: 'Invalid employment' }, { status: 400 });
        return Response.json(employment(11, 1, { organization_id: 22 }), { headers: { ETag: '"employment-11-r1"' } });
      }
      if (path === '/api/v1/people/7/employments') return collection([]);
      throw new Error(`unexpected ${request.method} ${path}`);
    });
    let done = false;
    await openInlineEmployer(controller, 'New Example', () => { done = true; });
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
    await waitFor(() => expect(done).toBe(true));
    expect(employers).toBe(1);
    expect(jobs).toBe(2);
    expect(employerBodies).toEqual([expect.objectContaining({ name: 'New Example', primary_domain: 'example.org', kind: 'school' })]);
    for (const body of jobBodies) {
      expect(body).toMatchObject({ organization_id: 22, title: 'Draft role', is_current: true });
      expect(body).not.toHaveProperty('end_date');
    }
  });

  it('offers to create an organization only once there is a name to create', async () => {
    const { controller } = controllerWith(async input => {
      const path = pathOf(requestOf(input));
      if (path === '/api/v1/organizations') return Response.json({ organizations: [organization(21, 1, 'Lumen Grove')], total: 1 });
      if (path === '/api/v1/people/7/employments') return collection([]);
      throw new Error(`unexpected ${path}`);
    });
    render(EmploymentEditor, { controller, personID: 7 });
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
    const idQuery = await openTypeahead('Organization');
    await fireEvent.input(idQuery, { target: { value: '21' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: 'Create organization “21”' }));
    expect(screen.getByRole('textbox', { name: 'Name' })).toHaveProperty('value', '21');
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


  it('rejects a year-less employment date and keeps Create disabled until it is fixed', async () => {
    const requests: Request[] = [];
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input); requests.push(request);
      if (pathOf(request) === '/api/v1/organizations') return Response.json({ organizations: [organization()], total: 1, limit: 50, offset: 0 });
      throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
    }));
    render(EmploymentEditor, { controller, personID: 7 });
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
    const employerQuery = await openTypeahead('Organization');
    await fireEvent.input(employerQuery, { target: { value: 'Example School' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: 'Create organization “Example School”' }));
    await fireEvent.input(start, { target: { value: '2025-06' } });
    await fireEvent.input(end, { target: { value: '2024-06' } });
    expect(create).toHaveProperty('disabled', true);
    await fireEvent.click(create);
    expect(requests.filter(request => request.method === 'POST')).toHaveLength(0);
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
    render(EmploymentEditor, { controller, personID: 7, employment: employment() });
    const context = await screen.findByText('At Exact Employment Org', { selector: 'p.context' });
    expect(context.textContent).toBe('At Exact Employment Org');
  });


  it('preserves fields from the saved job when resolving with Keep yours', async () => {
    const requests: Request[] = [];
    let reads = 0;
    let patches = 0;
    const hiddenByRead = [
      { person_id: 7, organization_id: 21, source: 'extraction', source_ref: 'modal-old', address_id: 11, confidence: 0.11 },
      { person_id: 8, organization_id: 22, source: 'extraction', source_ref: 'first-fresh', address_id: 22, confidence: 0.22 },
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
    const onClose = vi.fn();
    render(EmploymentEditor, { controller, personID: 7, employment: employment(), onClose });
    const title = await screen.findByRole('textbox', { name: 'Job title' }) as HTMLInputElement;
    await fireEvent.input(title, { target: { value: 'Draft title' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    const alert = (await screen.findByRole('alert')).closest('.inline-notice')!;
    expect(alert.textContent).toContain('This job changed since you opened it.');
    expect(title.value).toBe('Draft title');
    expect(patches).toBe(1);
    const firstPatch = requests.find((request) => request.method === 'PATCH')!;
    expect(await firstPatch.clone().json()).toMatchObject({ ...hiddenByRead[0], title: 'Draft title' });

    expect(screen.getByRole('button', { name: 'Use saved' })).toBeDefined();
    await waitFor(() => expect(screen.getByRole('button', { name: 'Keep yours' })).toHaveProperty('disabled', false));
    await fireEvent.click(screen.getByRole('button', { name: 'Keep yours' }));
    await waitFor(() => expect(patches).toBe(2));
    const retryPatch = requests.filter((request) => request.method === 'PATCH')[1]!;
    expect(await retryPatch.clone().json()).toMatchObject({ ...hiddenByRead[1], title: 'Draft title' });
  });

  it('closes and refreshes without a second PATCH when using the saved job', async () => {
    const requests: Request[] = [];
    let reads = 0;
    let patches = 0;
    const hiddenByRead = [
      { person_id: 7, organization_id: 21, source: 'extraction', source_ref: 'modal-old', address_id: 11, confidence: 0.11 },
      { person_id: 8, organization_id: 22, source: 'extraction', source_ref: 'first-fresh', address_id: 22, confidence: 0.22 },
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
    const onClose = vi.fn();
    render(EmploymentEditor, { controller, personID: 7, employment: employment(), onClose });
    const title = await screen.findByRole('textbox', { name: 'Job title' }) as HTMLInputElement;
    await fireEvent.input(title, { target: { value: 'Draft title' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    const alert = (await screen.findByRole('alert')).closest('.inline-notice')!;
    expect(alert.textContent).toContain('This job changed since you opened it.');
    expect(title.value).toBe('Draft title');
    expect(patches).toBe(1);
    const firstPatch = requests.find((request) => request.method === 'PATCH')!;
    expect(await firstPatch.clone().json()).toMatchObject({ ...hiddenByRead[0], title: 'Draft title' });

    await fireEvent.click(screen.getByRole('button', { name: 'Use saved' }));
    await waitFor(() => expect(onClose).toHaveBeenCalledOnce());
    expect(patches).toBe(1);
    expect(requests.some((request) => pathOf(request) === '/api/v1/people/7/employments')).toBe(true);
  });

it('saves a date-shaped description as text', async () => {
  let body: Record<string, unknown> | undefined;
  const job = employment(11, 1, { description: '20240305' });
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'PATCH') { body = await request.json(); return Response.json(job); }
    if (pathOf(request).includes('/organizations/')) return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    return Response.json(job, { headers: { ETag: '"employment-11-r1"' } });
  });
  render(EmploymentEditor, { controller, personID: 7, employment: job });
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Description' }), { target: { value: '2024-03-05' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  await waitFor(() => expect(body).toMatchObject({ description: '2024-03-05' }));
});



it.each([
  { name: 'a department changed elsewhere', opening: { department: 'Sales' }, saved: { department: 'Ops' }, edit: ['Job title', 'Lead'], rows: ['Job title', 'Department'], expected: { title: 'Lead', department: 'Sales' } },
  { name: 'a current job ended elsewhere', opening: { start_date: { year: 2020 } }, saved: { start_date: { year: 2020 }, is_current: false, end_date: { year: 2022 } }, edit: ['Start date', '2024'], rows: ['Start date', 'End date'], expected: { start_date: '2024', is_current: true } },
])('Keep yours writes the form and lists every field it overwrites, with $name', async ({ opening, saved, edit, rows, expected }) => {
  let reads = 0;
  const bodies: Record<string, unknown>[] = [];
  const job = employment(11, 1, opening);
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'PATCH') {
      bodies.push(await request.json());
      return bodies.length === 1 ? Response.json({ message: 'Changed elsewhere' }, { status: 412 }) : Response.json(job);
    }
    if (pathOf(request).includes('/organizations/')) return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    return Response.json(employment(11, ++reads, reads === 1 ? opening : { ...opening, ...saved }), { headers: { ETag: `"employment-11-r${reads}"` } });
  });
  render(EmploymentEditor, { controller, personID: 7, employment: job });
  await fireEvent.input(await screen.findByRole('textbox', { name: edit[0] }), { target: { value: edit[1] } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  const table = await screen.findByRole('table', { name: 'What changed' });
  expect(within(table).getAllByRole('rowheader').map(cell => cell.textContent)).toEqual(rows);
  const keep = screen.getByRole('button', { name: 'Keep yours' });
  expect(keep).toHaveProperty('disabled', false);
  await fireEvent.click(keep);
  await waitFor(() => expect(bodies).toHaveLength(2));
  expect(bodies[1]).toMatchObject(expected);
});

  it('retains the selected employer across searches beyond fifty results', async () => {
    const requests: Request[] = [];
    const queries: string[] = [];
    const employerID = 51;
    const employerName = 'Zebra Example';
    const { controller } = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/organizations') {
        const query = new URL(request.url).searchParams.get('q') ?? ''; queries.push(query);
        return Response.json({ organizations: query !== 'Zebra' ? Array.from({ length: 50 }, (_, index) => organization(index + 1, 1, `Example ${index + 1}`)) : [organization(employerID, 1, employerName)], total: 51, limit: 50, offset: 0 });
      }
      if (path === '/api/v1/employments' && request.method === 'POST') {
        return new Response(JSON.stringify(employment(12, 1, { organization_id: employerID, title: 'Architect', is_primary: true })), { status: 201, headers: { ETag: '"employment-12-r1"' } });
      }
      if (path === '/api/v1/people/7/employments') {
        return collection([employment(11, 2, { is_primary: false }), employment(12, 1, { organization_id: employerID, title: 'Architect', is_primary: true })], {
          employment_id: 12, organization_id: employerID, organization_name: employerName, title: 'Architect', vcard: {}
        });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.employments = [employment(11, 1, { is_primary: true })];
    const onDone = vi.fn();
    render(EmploymentEditor, { controller, personID: 7, onDone });
    await screen.findByRole('button', { name: /^Organization(:|$)/ });
    const input = await openTypeahead('Organization');
    await fireEvent.input(input, { target: { value: 'Zebra' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: new RegExp(employerName) }));
    await screen.findByRole('button', { name: `Organization: ${employerName}` });
    await fireEvent.input(await openTypeahead('Organization'), { target: { value: 'Example' } });
    await waitFor(() => expect(queries).toContain('Example'));
    await fireEvent.keyDown(screen.getByRole('combobox', { name: 'Organization' }), { key: 'Escape' });
    expect(screen.getByRole('button', { name: `Organization: ${employerName}` })).toBeDefined();
    await fireEvent.input(screen.getByRole('textbox', { name: 'Job title' }), { target: { value: 'Architect' } });
    await fireEvent.click(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!);

    await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    const post = requests.find((request) => pathOf(request) === '/api/v1/employments' && request.method === 'POST')!;
    const postedBody = await post.json();
    expect(postedBody).toMatchObject({ person_id: 7, organization_id: employerID, source: 'user', title: 'Architect' });
    expect(postedBody.source_ref).toBeUndefined();
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
    render(EmploymentEditor, { controller, personID: 7 });
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
