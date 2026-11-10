import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { controllerWith, employment, organizationProfile, requestOf, pathOf } from '../../directory/entity-fixtures.test-support';
import OrganizationEmploymentTab from './OrganizationEmploymentTab.svelte';
import { DirectoryEntityController } from '../../directory/entity-controller.svelte';

it('asks again when a job changes after the delete conflict is shown', async () => {
  let revision = 1;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    const path = pathOf(request);
    if (path === '/api/v1/employments/11' && request.method === 'GET') {
      const readRevision = revision;
      if (revision === 1) revision = 2;
      return Response.json(employment(11, readRevision, { title: `Title ${readRevision}` }), { headers: { ETag: `"employment-11-r${readRevision}"` } });
    }
    if (path === '/api/v1/employments/11' && request.method === 'DELETE') {
      return request.headers.get('If-Match') === `"employment-11-r${revision}"`
        ? new Response(null, { status: 204 })
        : Response.json({ error: 'employment_revision_conflict', message: 'Changed' }, { status: 409 });
    }
    if (path === '/api/v1/people/7/employments') return Response.json({ employments: [] });
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  controller.employments = [employment()];
  render(OrganizationEmploymentTab, { controller, personID: 7 });
  await fireEvent.click(within(screen.getByRole('region', { name: 'Work' })).getByRole('button', { name: /^Actions for/ }));
  await fireEvent.click(await screen.findByRole('menuitem', { name: 'Delete…' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
  await screen.findByText('Title 2');
  revision = 3;
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  expect(await screen.findByText('Title 3')).toBeDefined();
});

it('retries a delete whose conflict refresh failed with the version it sent', async () => {
  let reads = 0;
  const tags: (string | null)[] = [];
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    const path = pathOf(request);
    if (path === '/api/v1/employments/11' && request.method === 'GET') {
      reads++;
      if (reads === 2) throw new TypeError('offline');
      return Response.json(employment(11, reads, { title: `Title ${reads}` }), { headers: { ETag: `"employment-11-r${reads}"` } });
    }
    if (path === '/api/v1/employments/11' && request.method === 'DELETE') {
      tags.push(request.headers.get('If-Match'));
      return Response.json({ error: 'employment_revision_conflict', message: 'Changed' }, { status: 409 });
    }
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  controller.employments = [employment()];
  render(OrganizationEmploymentTab, { controller, personID: 7 });
  await fireEvent.click(within(screen.getByRole('region', { name: 'Work' })).getByRole('button', { name: /^Actions for/ }));
  await fireEvent.click(await screen.findByRole('menuitem', { name: 'Delete…' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
  await waitFor(() => expect(reads).toBe(2));
  await fireEvent.click(await screen.findByRole('button', { name: 'Retry' }));
  expect(await screen.findByText('Title 3')).toBeDefined();
  expect(tags).toEqual(['"employment-11-r1"', '"employment-11-r1"']);
});

it('refreshes Work when accepting a job ended elsewhere', async () => {
  let endedElsewhere = false;
  const { client } = controllerWith(async input => {
    const request = requestOf(input);
    const path = pathOf(request);
    const job = employment(11, endedElsewhere ? 2 : 1, endedElsewhere ? { is_current: false, end_date: { year: 2026, month: 10 } } : {});
    if (path === '/api/v1/employments/11' && request.method === 'GET') return Response.json(job, { headers: { ETag: `"employment-11-r${job.revision}"` } });
    if (path === '/api/v1/employments/11' && request.method === 'PATCH') return Response.json({ error: 'employment_revision_conflict', message: 'Employment changed; reload and retry' }, { status: 409 });
    if (path === '/api/v1/organizations/21') return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    if (path === '/api/v1/people/7/employments') return Response.json({ employments: [job] });
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  const directoryChanged = vi.fn();
  const controller = new DirectoryEntityController(client, 7, { onDirectoryChange: directoryChanged });
  controller.employments = [employment()];
  controller.organizationRecords.set(21, organizationProfile());
  render(OrganizationEmploymentTab, { controller, personID: 7 });
  const refresh = vi.spyOn(controller, 'refreshEmployments');
  await fireEvent.click(within(screen.getByRole('region', { name: 'Work' })).getByRole('button', { name: /^Actions for/ }));
  await fireEvent.click(await screen.findByRole('menuitem', { name: 'Edit…' }));
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Job title' }), { target: { value: 'Draft title' } });
  endedElsewhere = true;
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  await screen.findByRole('button', { name: 'Keep yours' });
  await fireEvent.click(screen.getByRole('button', { name: 'Use saved' }));
  await waitFor(() => expect(controller.employments[0].is_current).toBe(false));
  await waitFor(() => expect(directoryChanged).toHaveBeenCalledOnce());
  expect(refresh).toHaveBeenCalledOnce();
});

