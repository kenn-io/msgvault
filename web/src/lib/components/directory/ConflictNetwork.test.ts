import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { controllerWith, employment, organizationProfile, relationship, requestOf, pathOf } from '../../directory/entity-fixtures.test-support';
import OrganizationEmploymentTab from './OrganizationEmploymentTab.svelte';
import PersonNetwork from './PersonNetwork.svelte';
import RelationshipsTab from './RelationshipsTab.svelte';

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

it('refreshes Connections and Network when cancelling the delete of a connection ended elsewhere', async () => {
  let endedElsewhere = false;
  const { client, controller } = controllerWith(async input => {
    const request = requestOf(input);
    const path = pathOf(request);
    const current = relationship(41, endedElsewhere ? 2 : 1, endedElsewhere ? { end_date: { year: 2026, month: 10 } } : {});
    if (path === '/api/v1/person-relationships/41' && request.method === 'GET') return Response.json(current, { headers: { ETag: `"person-relationship-41-r${current.revision}"` } });
    if (path === '/api/v1/person-relationships/41' && request.method === 'DELETE') return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
    if (path === '/api/v1/person-relationship-reviews') return Response.json({ reviews: [] });
    if (path === '/api/v1/people/7/relationships') return Response.json({ relationships: [] });
    if (path === '/api/v1/people/7/network') return Response.json({
      root_person_id: 7, depth: 1, truncated: false,
      nodes: [{ id: 'person:7', kind: 'person', entity_id: 7, label: 'Example Person', hop: 0 },
        ...(endedElsewhere ? [] : [{ id: 'person:141', kind: 'person', entity_id: 141, label: 'Synthetic Peer', hop: 1 }])],
      edges: endedElsewhere ? [] : [{ id: 'relationship:41', kind: 'relationship', source_node_id: 'person:7', target_node_id: 'person:141', relationship_type_slug: 'mentor', label: 'mentors' }],
    });
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  controller.relationships = [{ counterpart_person_id: 141, counterpart_display_name: 'Synthetic Peer', counterpart_label: 'mentors', counterpart_vcard_uid: 'person-141', direction: 'outgoing' as const, relationship: relationship(41) }];
  render(RelationshipsTab, { client, controller, personID: 7 });
  render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
  expect(await screen.findByRole('list', { name: 'Network connections' })).toBeDefined();
  await fireEvent.click(within(screen.getByRole('region', { name: 'Connections' })).getByRole('button', { name: /^Actions for/ }));
  await fireEvent.click(await screen.findByRole('menuitem', { name: 'Delete…' }));
  await waitFor(() => expect(screen.getByRole('button', { name: 'Delete' })).toHaveProperty('disabled', false));
  endedElsewhere = true;
  await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
  await screen.findByText('This connection changed since you opened it.');
  await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
  await waitFor(() => expect(controller.relationships).toEqual([]));
  await waitFor(() => expect(screen.queryByRole('list', { name: 'Network connections' })).toBeNull());
});
