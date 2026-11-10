import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
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

async function addPrimaryChoice(controller: DirectoryEntityController, check: boolean, onDone = () => undefined): Promise<void> {
  render(EmploymentEditor, { controller, personID: 7, onDone });
  const checkbox = screen.queryByRole('checkbox', { name: 'Primary job' }) as HTMLInputElement | null;
  if (checkbox) {
    expect(checkbox.checked).toBe(false);
    if (check) await fireEvent.click(checkbox);
  }
  await fireEvent.keyDown(screen.getByRole('button', { name: /^Organization:/ }), { key: 'ArrowDown' });
  await fireEvent.mouseDown(await screen.findByRole('option', { name: /Synthetic Org/ }));
  await fireEvent.click(screen.getByRole('button', { name: 'Add job' }));
}

it.each([
  { label: 'leaves a new job unmarked when unchecked', currentPrimary: false, selected: false, expected: false },
  { label: 'creates a primary job in one write when selected', currentPrimary: false, selected: true, expected: true },
  { label: 'omits the choice when a current job is already primary', currentPrimary: true, selected: false, expected: undefined },
])('$label', async ({ currentPrimary, selected, expected }) => {
  let body: Record<string, unknown> | undefined;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'POST') { body = await request.json(); return Response.json(employment()); }
    return Response.json({ employments: [], organizations: [organization()] });
  });
  controller.organizations = [organization()];
  controller.employments = [employment(12, 1, { is_primary: currentPrimary })];
  await addPrimaryChoice(controller, selected);
  await waitFor(() => expect(body).toBeDefined());
  if (expected === undefined) expect(body).not.toHaveProperty('is_primary');
  else expect(body).toHaveProperty('is_primary', expected);
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
    expect(screen.getByRole('textbox', { name: 'Name' })).toHaveProperty('value', 'New Example');
    expect(screen.getByRole('button', { name: 'Organization: New organization' })).toHaveProperty('disabled', true);
    await chooseSelectOption(screen.getByRole('combobox', { name: 'Organization kind: Company' }), 'School');
    await fireEvent.input(screen.getByRole('textbox', { name: 'Domain' }), { target: { value: 'example.org' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Job title' }), { target: { value: 'Draft role' } });
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Current' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'End date' }), { target: { value: '2024-13' } });
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Current' }));
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

it.each(['none', 'same name'] as const)('reconciles an inline organization create after its response is lost (%s)', async match => {
    let posts = 0;
    let jobBody: Record<string, unknown> | undefined;
    const { controller } = controllerWith(async input => {
      const request = requestOf(input), path = pathOf(request);
      if (path === '/api/v1/organizations' && request.method === 'POST') {
        posts++;
        if (match === 'none') return Response.json({ message: 'Unavailable' }, { status: 503 });
        throw new TypeError('response lost');
      }
      if (path === '/api/v1/organizations') return Response.json({ organizations: posts && match !== 'none' ? [organization(22, 1, 'Example company')] : [], total: posts && match !== 'none' ? 1 : 0 });
      if (path === '/api/v1/employments') { jobBody = await request.json(); return Response.json(employment(11, 1, { organization_id: 22 })); }
      if (path === '/api/v1/people/7/employments') return collection([]);
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    await openInlineEmployer(controller, 'Example company');
    await fireEvent.click(screen.getByRole('button', { name: 'Add job' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Check again' }));
    await waitFor(() => expect(screen.queryByRole('alert')).toBeNull());
    if (match === 'none') {
      expect(screen.getByRole('textbox', { name: 'Name' })).toHaveProperty('value', 'Example company');
      expect(screen.getAllByRole('button', { name: /^Add job/ }).at(-1)!).toHaveProperty('disabled', false);
    } else {
      await waitFor(() => expect(screen.queryByRole('textbox', { name: 'Name' })).toBeNull());
      expect(screen.getByRole('button', { name: 'Add job' })).toHaveProperty('disabled', true);
      await openTypeahead('Organization');
      await fireEvent.mouseDown(await screen.findByRole('option', { name: /Example company/ }));
      await fireEvent.click(screen.getByRole('button', { name: 'Add job' }));
      await waitFor(() => expect(jobBody).toMatchObject({ organization_id: 22 }));
    }
    expect(posts).toBe(1);
  });

  it('keeps Add job disabled while the year-less date is invalid', async () => {
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
  });

  it('shows the exact employer in the existing job context', async () => {
    const { controller } = controllerWith(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11') return new Response(JSON.stringify(employment()), { headers: { ETag: '"employment-11-r1"' } });
      if (path === '/api/v1/organizations/21') return new Response(JSON.stringify(organizationProfile(2, 'Exact Employment Org')), { headers: { ETag: '"organization-21-r2"' } });
      if (path === '/api/v1/people/7/employments') return collection([employment()]);
      throw new Error(`unexpected ${request.method} ${path}`);
    });
    controller.employments = [employment()];
    render(EmploymentEditor, { controller, personID: 7, employment: employment() });
    await screen.findByText('At Exact Employment Org', { selector: 'p.context' });
  });

it('checks again after a lost job create and keeps the draft', async () => {
  let posts = 0;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input), path = pathOf(request);
    if (path === '/api/v1/organizations') return Response.json({ organizations: [organization(21, 1, 'Lumen Grove')], total: 1 });
    if (path === '/api/v1/employments') { posts++; throw new TypeError('response lost'); }
    if (path === '/api/v1/people/7/employments') return collection([]);
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  const onDone = vi.fn();
  render(EmploymentEditor, { controller, personID: 7, onDone });
  await openTypeahead('Organization');
  await fireEvent.mouseDown(await screen.findByRole('option', { name: /Lumen Grove/ }));
  await fireEvent.input(screen.getByRole('textbox', { name: 'Job title' }), { target: { value: 'Draft role' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Add job' }));
  await fireEvent.click(await screen.findByRole('button', { name: 'Check again' }));
  await waitFor(() => expect(screen.getByRole('button', { name: 'Add job' })).toHaveProperty('disabled', false));
  expect(screen.getByRole('textbox', { name: 'Job title' })).toHaveProperty('value', 'Draft role');
  expect(onDone).not.toHaveBeenCalled();
  expect(posts).toBe(1);
});

it.each(['Keep yours', 'Use saved'] as const)('shows the saved job and resolves a conflict with %s', async resolution => {
  const opening = { department: 'Sales' };
  const saved = { department: 'Ops' };
  let reads = 0;
  const bodies: Record<string, unknown>[] = [];
  const job = employment(11, 1, opening);
  const directoryChanged = vi.fn();
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'PATCH') {
      bodies.push(await request.json());
      return bodies.length === 1 ? Response.json({ message: 'Changed elsewhere' }, { status: 412 }) : Response.json(job);
    }
    if (pathOf(request).includes('/organizations/')) return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    if (pathOf(request) === '/api/v1/people/7/employments') return Response.json({ employments: [employment(11, 2, { ...opening, ...saved })] });
    return Response.json(employment(11, ++reads, reads === 1 ? opening : { ...opening, ...saved }), { headers: { ETag: `"employment-11-r${reads}"` } });
  }, 7, { onDirectoryChange: directoryChanged });
  const onClose = vi.fn();
  render(EmploymentEditor, { controller, personID: 7, employment: job, onClose });
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Job title' }), { target: { value: 'Lead' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  const record = await screen.findByRole('region', { name: 'Saved job' });
  expect(record.textContent).toContain('DepartmentOps');
  expect(bodies).toHaveLength(1);
  await fireEvent.click(screen.getByRole('button', { name: resolution }));
  if (resolution === 'Keep yours') {
    await waitFor(() => expect(bodies).toHaveLength(2));
    expect(bodies[1]).toMatchObject({ title: 'Lead', department: 'Sales' });
  } else {
    await waitFor(() => expect(onClose).toHaveBeenCalledOnce());
    await waitFor(() => expect(directoryChanged).toHaveBeenCalledOnce());
    expect(controller.employments[0].department).toBe('Ops');
    expect(bodies).toHaveLength(1);
  }
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
        return new Response(JSON.stringify(employment(12, 1, { organization_id: employerID, title: 'Architect' })), { status: 201, headers: { ETag: '"employment-12-r1"' } });
      }
      if (path === '/api/v1/people/7/employments') {
        return Response.json({ employments: [employment(11, 2), employment(12, 1, { organization_id: employerID, title: 'Architect' })] });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.employments = [employment(11, 1)];
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
    expect(postedBody).toMatchObject({ person_id: 7, organization_id: employerID, title: 'Architect' });
    expect(postedBody.source_ref).toBeUndefined();
  });
