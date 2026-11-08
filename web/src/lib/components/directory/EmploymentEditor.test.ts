import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import { controllerWith, organization, organizationProfile, employment, pathOf, requestOf } from '../../directory/entity-fixtures.test-support';
import EmploymentEditor from './EmploymentEditor.svelte';

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

it('Keep yours writes only the fields the person changed and keeps a department changed elsewhere', async () => {
  let reads = 0;
  const bodies: Record<string, unknown>[] = [];
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'PATCH') {
      bodies.push(await request.json());
      return bodies.length === 1 ? Response.json({ message: 'Changed elsewhere' }, { status: 412 }) : Response.json(employment(11, 3));
    }
    if (pathOf(request).includes('/organizations/')) return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    return Response.json(employment(11, ++reads, { department: reads === 1 ? 'Sales' : 'Ops' }), { headers: { ETag: `"employment-11-r${reads}"` } });
  });
  render(EmploymentEditor, { controller, personID: 7, employment: employment() });
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Job title' }), { target: { value: 'Lead' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  await fireEvent.click(await screen.findByRole('button', { name: 'Keep yours' }));
  await waitFor(() => expect(bodies).toHaveLength(2));
  expect(bodies[0]).toMatchObject({ title: 'Lead', department: 'Sales' });
  expect(bodies[1]).toMatchObject({ title: 'Lead', department: 'Ops' });
});

it('lists under Yours only the fields the person changed', async () => {
  let reads = 0;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    if (request.method === 'PATCH') return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
    if (pathOf(request).includes('/organizations/')) return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    return Response.json(employment(11, ++reads, { department: reads === 1 ? 'Sales' : 'Ops' }), { headers: { ETag: `"employment-11-r${reads}"` } });
  });
  render(EmploymentEditor, { controller, personID: 7, employment: employment() });
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Job title' }), { target: { value: 'Lead' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  const table = await screen.findByRole('table', { name: 'What changed' });
  expect(within(table).getAllByRole('rowheader').map(cell => cell.textContent)).toEqual(['Job title']);
  expect(screen.getByText('Other details also changed')).toBeDefined();
});

it('describes a job moved to an unloaded person without showing their ID', async () => {
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
