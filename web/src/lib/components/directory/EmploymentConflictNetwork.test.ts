import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { controllerWith, employment, organizationProfile, requestOf, pathOf } from '../../directory/entity-fixtures.test-support';
import OrganizationEmploymentTab from './OrganizationEmploymentTab.svelte';
import PersonNetwork from './PersonNetwork.svelte';

it('refreshes Work and Network when accepting a job ended elsewhere', async () => {
  let endedElsewhere = false;
  const { controller } = controllerWith(async input => {
    const request = requestOf(input);
    const path = pathOf(request);
    const job = employment(11, endedElsewhere ? 2 : 1, endedElsewhere ? { is_current: false, end_date: { year: 2026, month: 10 } } : {});
    if (path === '/api/v1/employments/11' && request.method === 'GET') return Response.json(job, { headers: { ETag: `"employment-11-r${job.revision}"` } });
    if (path === '/api/v1/employments/11' && request.method === 'PATCH') return Response.json({ error: 'employment_revision_conflict', message: 'Employment changed; reload and retry' }, { status: 409 });
    if (path === '/api/v1/organizations/21') return Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } });
    if (path === '/api/v1/people/7/employments') return Response.json({ employments: [job] });
    if (path === '/api/v1/people/7/network') return Response.json({
      root_person_id: 7, depth: 1, truncated: false,
      nodes: [{ id: 'person:7', kind: 'person', entity_id: 7, label: 'Example Person', hop: 0 },
        ...(endedElsewhere ? [] : [{ id: 'organization:21', kind: 'organization', entity_id: 21, label: 'Synthetic Org', hop: 1 }])],
      edges: endedElsewhere ? [] : [{ id: 'employment:11', kind: 'employment', source_node_id: 'person:7', target_node_id: 'organization:21', label: 'Engineer' }],
    });
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  controller.employments = [employment()];
  controller.organizationRecords.set(21, organizationProfile());
  render(OrganizationEmploymentTab, { controller, personID: 7 });
  render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
  expect(await screen.findByRole('list', { name: 'Network connections' })).toBeDefined();
  await fireEvent.click(within(screen.getByRole('region', { name: 'Work' })).getByRole('button', { name: /^Actions for/ }));
  await fireEvent.click(await screen.findByRole('menuitem', { name: 'Edit…' }));
  await fireEvent.input(await screen.findByRole('textbox', { name: 'Job title' }), { target: { value: 'Draft title' } });
  endedElsewhere = true;
  await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  await screen.findByRole('button', { name: 'Keep yours' });
  await fireEvent.click(screen.getByRole('button', { name: 'Use saved' }));
  await waitFor(() => expect(controller.employments[0].is_current).toBe(false));
  await waitFor(() => expect(screen.queryByRole('list', { name: 'Network connections' })).toBeNull());
});
