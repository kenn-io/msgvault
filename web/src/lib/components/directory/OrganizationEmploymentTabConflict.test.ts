import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { controllerWith, employment, organizationProfile, requestOf, pathOf } from '../../directory/entity-fixtures.test-support';
import OrganizationEmploymentTab from './OrganizationEmploymentTab.svelte';

it('names the new employer when a job moves before deletion', async () => {
  let reads = 0;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    const path = pathOf(request);
    if (path === '/api/v1/employments/11' && request.method === 'GET') {
      reads++;
      return Response.json(employment(11, reads, { organization_id: reads === 1 ? 21 : 22 }), { headers: { ETag: `"employment-11-r${reads}"` } });
    }
    if (path === '/api/v1/employments/11' && request.method === 'DELETE') return Response.json({ error: 'employment_revision_conflict', message: 'Changed' }, { status: 409 });
    if (path === '/api/v1/organizations/22') return Response.json({ ...organizationProfile(), organization: { ...organizationProfile().organization, id: 22, name: 'New employer' } }, { headers: { ETag: '"organization-22-r1"' } });
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  controller.employments = [employment()];
  controller.organizationRecords.set(21, organizationProfile());
  render(OrganizationEmploymentTab, { controller, personID: 7 });
  await fireEvent.click(within(screen.getByRole('region', { name: 'Work' })).getByRole('button', { name: /^Actions for/ }));
  await fireEvent.click(await screen.findByRole('menuitem', { name: 'Delete…' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
  await screen.findByRole('dialog', { name: /at New employer\?/ });
});

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

it('shows a moved job before retrying Make primary', async () => {
  let revision = 1;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    const path = pathOf(request);
    if (path === '/api/v1/employments/11' && request.method === 'GET') {
      const readRevision = revision;
      if (revision === 1) revision = 2;
      return Response.json(employment(11, readRevision, { person_id: readRevision === 3 ? 8 : 7, title: `Title ${readRevision}` }), { headers: { ETag: `"employment-11-r${readRevision}"` } });
    }
    if (path === '/api/v1/employments/11/primary' && request.method === 'POST') {
      return request.headers.get('If-Match') === `"employment-11-r${revision}"`
        ? Response.json(employment(11, revision, { person_id: 8, is_primary: true }))
        : Response.json({ error: 'employment_revision_conflict', message: 'Changed' }, { status: 409 });
    }
    if (path === '/api/v1/people/7/employments') return Response.json({ employments: [] });
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  controller.employments = [employment(11, 1, { is_primary: false })];
  render(OrganizationEmploymentTab, { controller, personID: 7 });
  await fireEvent.click(within(screen.getByRole('region', { name: 'Work' })).getByRole('button', { name: /^Actions for/ }));
  await fireEvent.click(await screen.findByRole('menuitem', { name: 'Make primary' }));
  await screen.findByText('Title 2');
  revision = 3;
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  expect(await screen.findByText('Someone else')).toBeDefined();
});

it('uses the selected job version after a different job conflicts', async () => {
  let reads = 0;
  let secondTag: string | null = null;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    const path = pathOf(request);
    if (path === '/api/v1/employments/11' && request.method === 'GET') {
      reads++;
      return Response.json(employment(11, reads, { title: 'First job' }), { headers: { ETag: `"employment-11-r${reads}"` } });
    }
    if (path === '/api/v1/employments/11/primary') return Response.json({ error: 'employment_revision_conflict', message: 'Changed' }, { status: 409 });
    if (path === '/api/v1/employments/12' && request.method === 'GET') return Response.json(employment(12, 1, { title: 'Second job' }), { headers: { ETag: '"employment-12-r1"' } });
    if (path === '/api/v1/employments/12/primary') {
      secondTag = request.headers.get('If-Match');
      return Response.json(employment(12, 1, { is_primary: true }));
    }
    if (path === '/api/v1/people/7/employments') return Response.json({ employments: [] });
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  controller.employments = [employment(11, 1, { title: 'First job', is_primary: false }), employment(12, 1, { title: 'Second job', is_primary: false })];
  render(OrganizationEmploymentTab, { controller, personID: 7 });
  await fireEvent.click(screen.getByRole('button', { name: /Actions for First job/ }));
  await fireEvent.click(await screen.findByRole('menuitem', { name: 'Make primary' }));
  await screen.findByRole('button', { name: 'Retry' });
  await fireEvent.click(screen.getByRole('button', { name: /Actions for Second job/ }));
  await fireEvent.click(await screen.findByRole('menuitem', { name: 'Make primary' }));
  await waitFor(() => expect(secondTag).toBe('"employment-12-r1"'));
});
