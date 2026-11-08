import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import { DirectoryEntityController } from './entity-controller.svelte';
import type { Prepared } from './models';
import { requestOf, pathOf, employment, organization, organizationProfile, relationship, relationshipType as makeRelationshipType } from './entity-fixtures.test-support';

it.each(['update', 'delete'] as const)('returns non-revision refusals as errors for %s', async action => {
  const refusal = action === 'update'
    ? { status: 409, error: 'employment_duplicate_active', message: 'Person already has this current employment' }
    : { status: 404, error: 'employment_not_found', message: 'Employment not found' };
  const requests: Request[] = [];
  const client = createAPIClient(async input => {
    const request = requestOf(input);
    requests.push(request);
    return request.method === 'GET'
      ? Response.json(employment(), { headers: { ETag: '"employment-11-r1"' } })
      : Response.json({ error: refusal.error, message: refusal.message }, { status: refusal.status });
  });
  const controller = new DirectoryEntityController(client, 7);
  try {
    const result = action === 'update'
      ? await controller.updateEmployment(11, () => ({ person_id: 7, organization_id: 21, source: 'user' }))
      : await controller.deleteEmployment(11);
    expect(result).toEqual({ ok: false, kind: 'error', status: refusal.status, message: refusal.message });
    expect(requests.map(request => request.method)).toEqual(['GET', action === 'update' ? 'PATCH' : 'DELETE']);
  } finally { controller.destroy(); }
});

function relationshipType(id = 31, revision = 1) {
  return makeRelationshipType({ id, revision, slug: 'knows', forward_label: 'knows', reverse_label: 'is known by', is_canonical: false, universal_id: `relationship-type-${id}` });
}

function network(personID: number, depth: number) {
  return {
    root_person_id: personID, depth, truncated: false,
    nodes: [{ id: `person:${personID}`, kind: 'person', entity_id: personID, label: `Person ${personID}`, hop: 0 }],
    edges: []
  };
}

function defaultResponse(request: Request): Response {
  const path = pathOf(request);
  if (path === '/api/v1/organizations') {
    return Response.json({ organizations: [{ id: 21, revision: 1, name: 'Synthetic Org', kind: 'company', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z' }], total: 1, limit: 50, offset: 0 });
  }
  if (path === '/api/v1/people/7/employments') {
    return new Response(JSON.stringify({ employments: [employment(11)], projection: { employment_id: 11, organization_id: 21, organization_name: 'Synthetic Org', vcard: {} } }), { headers: { ETag: '"collection-etag-must-not-be-used"' } });
  }
  if (path === '/api/v1/people/7/relationships') {
    return Response.json({ relationships: [{ counterpart_person_id: 8, counterpart_label: 'Synthetic Peer', counterpart_vcard_uid: 'person-8', direction: 'outgoing', relationship: relationship(41) }] });
  }
  if (path === '/api/v1/relationship-types') return Response.json({ relationship_types: [relationshipType()] });
  if (path === '/api/v1/people/7/network') {
    const depth = Number(new URL(request.url).searchParams.get('depth'));
    return Response.json(network(7, depth));
  }
  throw new Error(`unexpected ${request.method} ${path}`);
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((next) => { resolve = next; });
  return { promise, resolve };
}

describe('DirectoryEntityController', () => {
  it('creates an organization from Settings without reading a person employment collection', async () => {
    const requests: Request[] = [];
    const onDirectoryChange = vi.fn();
    const client = createAPIClient(async input => {
      const request = requestOf(input); requests.push(request);
      if (pathOf(request) === '/api/v1/organizations' && request.method === 'POST') return Response.json(organization(22), { status: 201, headers: { ETag: '"organization-22-r1"' } });
      return Response.json({ message: 'Person not found.' }, { status: 404 });
    });
    const controller = new DirectoryEntityController(client, 0, { onDirectoryChange });
    try {
      expect(await controller.createOrganization({ name: 'Synthetic Org', kind: 'company' })).toMatchObject({ ok: true });
      expect(requests.map(request => `${request.method} ${pathOf(request)}`)).toEqual(['POST /api/v1/organizations']);
      expect(onDirectoryChange).toHaveBeenCalledOnce();
    } finally { controller.destroy(); }
  });

  it('keeps the renamed employer after a deferred Details lookup completes', async () => {
    const oldLookup = deferredResponse();
    let organizationReads = 0;
    let renamed = false;
    const client = createAPIClient(async input => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/7/employments') return Response.json({ employments: [employment(11), employment(12, 1, { organization_id: 22 })] });
      if (path === '/api/v1/organizations/22') return Response.json({ ...organizationProfile(), organization: organization(22, 1, 'Second employer') }, { headers: { ETag: '"organization-22-r1"' } });
      if (path === '/api/v1/organizations/21' && request.method === 'GET') {
        if (++organizationReads === 1) return oldLookup.promise;
        return Response.json(organizationProfile(renamed ? 2 : 1, renamed ? 'Renamed employer' : 'Synthetic Org'), { headers: { ETag: renamed ? '"organization-21-r2"' : '"organization-21-r1"' } });
      }
      if (path === '/api/v1/organizations/21' && request.method === 'PATCH') {
        renamed = true;
        return Response.json(organization(21, 2, 'Renamed employer'), { headers: { ETag: '"organization-21-r2"' } });
      }
      return defaultResponse(request);
    });
    const controller = new DirectoryEntityController(client, 7);
    const prepare = vi.spyOn(controller, 'prepareOrganizationMutation');
    const opening = controller.load();
    try {
      await vi.waitFor(() => expect(organizationReads).toBe(1));
      expect(await controller.updateOrganization(21, { name: 'Renamed employer' })).toMatchObject({ ok: true });
      oldLookup.resolve(Response.json(organizationProfile(), { headers: { ETag: '"organization-21-r1"' } }));
      await opening;
      await Promise.allSettled([prepare.mock.results[0]!.value]);
      expect(controller.organizationRecords.get(21)?.organization.name).toBe('Renamed employer');
      expect(controller.organizationRecords.get(22)?.organization.name).toBe('Second employer');
    } finally {
      oldLookup.resolve(Response.json(organizationProfile()));
      await opening;
      await Promise.allSettled([prepare.mock.results[0]!.value]);
      controller.destroy();
    }
  });

  it.each(['connection', 'work'])('refreshes an open Network after a %s write with the selected graph controls', async (kind) => {
    let changed = false;
    const reads: URL[] = [];
    const client = createAPIClient(async input => {
      const request = requestOf(input); const path = pathOf(request);
      if (path.endsWith('/network')) {
        reads.push(new URL(request.url));
        return Response.json({ ...network(7, 3), nodes: [{ id: 'person:7', kind: 'person', entity_id: 7, label: changed ? 'Updated projection' : 'Original projection', hop: 0 }] });
      }
      if (request.method !== 'GET') {
        changed = true;
        return Response.json(kind === 'connection' ? relationship(42) : employment(12));
      }
      return defaultResponse(request);
    });
    const controller = new DirectoryEntityController(client, 7);
    await controller.loadNetwork(3);
    const result = kind === 'connection' ? await controller.createRelationship({ source_person_id: 7, target_person_id: 8, relationship_type_slug: 'knows' }) : await controller.createEmployment({ person_id: 7, organization_id: 21, source: 'user' });
    expect(result.ok).toBe(true);
    expect(controller.network?.nodes[0]?.label).toBe('Updated projection');
    expect(reads.map(url => url.search)).toEqual(['?depth=3&include_ended=false', '?depth=3&include_ended=false']);
    controller.destroy();
  });

  it('loads generated collections without the network and without treating collection ETags as entity ETags', async () => {
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      return defaultResponse(request);
    }));
    const controller = new DirectoryEntityController(client, 7);

    await controller.load();
    await controller.refreshOrganizations('synthetic');

    expect(controller.organizations.map((item) => item.id)).toEqual([21]);
    expect(controller.employments.map((item) => item.id)).toEqual([11]);
    expect(controller.relationships.map((item) => item.relationship.id)).toEqual([41]);
    expect(controller.relationshipTypes.map((item) => item.id)).toEqual([31]);
    expect(controller.network).toBeNull();
    expect(requests.map(pathOf)).not.toContain('/api/v1/people/7/network');

    await controller.loadNetwork();

    expect(controller.network?.depth).toBe(1);
    const networkRequest = requests.find((request) => pathOf(request) === '/api/v1/people/7/network')!;
    expect(new URL(networkRequest.url).searchParams.get('depth')).toBe('1');
    expect(new URL(networkRequest.url).searchParams.get('include_ended')).toBe('false');
    const organizationRequest = requests.find((request) => pathOf(request) === '/api/v1/organizations')!;
    expect(new URL(organizationRequest.url).searchParams.get('q')).toBe('synthetic');
  });

  it('builds an employment write from the same fresh record that supplied its ETag', async () => {
    const requests: Request[] = [];
    const directoryChanged = vi.fn();
    let individualRead = 0;
    let employmentSeenByBuilder: unknown;
    const freshEmployment = employment(11, 2, {
      person_id: 8,
      organization_id: 22,
      source: 'extraction',
      source_ref: 'fresh-card',
      address_id: 72,
      confidence: 0.72
    });
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') {
        individualRead += 1;
        return new Response(JSON.stringify(freshEmployment), { headers: { ETag: '"employment-11-r2"' } });
      }
      if (path === '/api/v1/employments/11' && request.method === 'PATCH') {
        return new Response(JSON.stringify(employment(11, 3)), { headers: { ETag: '"employment-11-r3"' } });
      }
      if (path === '/api/v1/people/7/employments' && individualRead > 0) {
        return Response.json({ employments: [employment(11, 3)], projection: { employment_id: 11, organization_id: 21, organization_name: 'Synthetic Org', vcard: {} } });
      }
      return defaultResponse(request);
    }));
    const controller = new DirectoryEntityController(client, 7, { onDirectoryChange: directoryChanged });
    await controller.load();

    const result = await controller.updateEmployment(11, (current) => {
      employmentSeenByBuilder = current;
      return {
        person_id: current.person_id,
        organization_id: current.organization_id,
        source: current.source as 'extraction',
        source_ref: current.source_ref,
        address_id: current.address_id,
        confidence: current.confidence,
        title: 'Draft title'
      };
    });

    expect(result).toMatchObject({ ok: true, entity: employment(11, 3) });
    expect(employmentSeenByBuilder).toEqual(freshEmployment);
    expect(individualRead).toBe(1);
    const write = requests.find((request) => request.method === 'PATCH')!;
    expect(write.headers.get('If-Match')).toBe('"employment-11-r2"');
    await expect(write.clone().json()).resolves.toMatchObject({
      person_id: 8,
      organization_id: 22,
      source: 'extraction',
      source_ref: 'fresh-card',
      address_id: 72,
      confidence: 0.72,
      title: 'Draft title'
    });
    expect(controller.employments[0]?.revision).toBe(3);
    expect(directoryChanged).toHaveBeenCalledOnce();
  });

  it('updates the retained individual organization record and ETag after a successful write', async () => {
    let organizationRead = 0;
    let patchRequest: Request | undefined;
    const directoryChanged = vi.fn();
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (pathOf(request) === '/api/v1/organizations/21' && request.method === 'GET') {
        organizationRead += 1;
        return new Response(JSON.stringify(organizationProfile(2)), {
          headers: { ETag: '"organization-21-r2"' }
        });
      }
      if (pathOf(request) === '/api/v1/organizations/21' && request.method === 'PATCH') {
        patchRequest = request;
        return new Response(JSON.stringify(organization(21, 3)), { headers: { ETag: '"organization-21-r3"' } });
      }
      return defaultResponse(request);
    }));
    const controller = new DirectoryEntityController(client, 7, { onDirectoryChange: directoryChanged });

    await expect(controller.updateOrganization(21, { name: 'Synthetic Org', kind: 'company' })).resolves.toMatchObject({ ok: true });

    expect(organizationRead).toBe(1);
    expect(patchRequest?.headers.get('If-Match')).toBe('"organization-21-r2"');
    expect(controller.organizationRecords.get(21)?.organization.revision).toBe(3);
    expect(directoryChanged).toHaveBeenCalledOnce();
  });

  it('does not retry a conflict, refreshes the exact entity, and returns a typed result', async () => {
    const requests: Request[] = [];
    let individualRead = 0;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') {
        individualRead += 1;
        const revision = individualRead + 1;
        return new Response(JSON.stringify(employment(11, revision)), { headers: { ETag: `"employment-11-r${revision}"` } });
      }
      if (path === '/api/v1/employments/11' && request.method === 'PATCH') {
        return Response.json({ error: 'revision_conflict', message: 'changed elsewhere' }, { status: 412 });
      }
      return defaultResponse(request);
    }));
    const controller = new DirectoryEntityController(client, 7);
    await controller.load();

    const result = await controller.updateEmployment(11, () => ({
      person_id: 7, organization_id: 21, source: 'user', title: 'Draft title'
    }));

    expect(result).toMatchObject({ ok: false, kind: 'conflict', status: 412, current: { data: employment(11, 3), etag: '"employment-11-r3"' } });
    expect(requests.filter((request) => request.method === 'PATCH')).toHaveLength(1);
    expect(individualRead).toBe(2);
  });

  it('blocks a duplicate create after a lost response until the matching collection refresh succeeds', async () => {
    let postCount = 0;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (pathOf(request) === '/api/v1/employments' && request.method === 'POST') {
        postCount += 1;
        if (postCount === 1) throw new TypeError('response lost');
        return new Response(JSON.stringify(employment(12)), { status: 201, headers: { ETag: '"employment-12-r1"' } });
      }
      return defaultResponse(request);
    }));
    const controller = new DirectoryEntityController(client, 7);
    const body = { person_id: 7, organization_id: 21, source: 'user' } as const;

    await expect(controller.createEmployment(body)).resolves.toMatchObject({ ok: false, kind: 'unknown' });
    await expect(controller.createEmployment(body)).resolves.toMatchObject({ ok: false, kind: 'blocked' });
    expect(postCount).toBe(1);

    await controller.refreshEmployments();
    await expect(controller.createEmployment(body)).resolves.toMatchObject({ ok: true });
    expect(postCount).toBe(2);
  });

  it('keeps an unknown create blocked when a refresh that began before it resolves later', async () => {
    const staleRefresh = deferredResponse();
    let collectionReads = 0;
    let postCount = 0;
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/people/7/employments') {
        collectionReads += 1;
        return collectionReads === 1 ? staleRefresh.promise : defaultResponse(request);
      }
      if (path === '/api/v1/employments' && request.method === 'POST') {
        postCount += 1;
        if (postCount === 1) throw new TypeError('committed but response lost');
        return new Response(JSON.stringify(employment(12)), { status: 201, headers: { ETag: '"employment-12-r1"' } });
      }
      return defaultResponse(request);
    }));
    const controller = new DirectoryEntityController(client, 7);
    const body = { person_id: 7, organization_id: 21, source: 'user' } as const;

    const refresh = controller.refreshEmployments();
    await vi.waitFor(() => expect(collectionReads).toBe(1));
    await expect(controller.createEmployment(body)).resolves.toMatchObject({ ok: false, kind: 'unknown' });
    const oldRefreshRequest = requests.find((request) => pathOf(request) === '/api/v1/people/7/employments')!;

    staleRefresh.resolve(Response.json({ employments: [] }));
    await refresh;
    expect(oldRefreshRequest.signal.aborted).toBe(true);
    await expect(controller.createEmployment(body)).resolves.toMatchObject({ ok: false, kind: 'blocked' });
    expect(postCount).toBe(1);

    await controller.refreshEmployments();
    await expect(controller.createEmployment(body)).resolves.toMatchObject({ ok: true });
    expect(postCount).toBe(2);
  });

  it.each([
    {
      name: 'organization', resource: 'organizations' as const, path: '/api/v1/organizations',
      create: (controller: DirectoryEntityController) => controller.createOrganization({ name: 'Synthetic Org', kind: 'company' })
    },
    {
      name: 'employment', resource: 'employments' as const, path: '/api/v1/employments',
      create: (controller: DirectoryEntityController) => controller.createEmployment({ person_id: 7, organization_id: 21, source: 'user' })
    },
    {
      name: 'person relationship', resource: 'relationships' as const, path: '/api/v1/person-relationships',
      create: (controller: DirectoryEntityController) => controller.createRelationship({ source_person_id: 7, target_person_id: 8, relationship_type_slug: 'knows' })
    },
    {
      name: 'relationship type', resource: 'relationshipTypes' as const, path: '/api/v1/relationship-types',
      create: (controller: DirectoryEntityController) => controller.createRelationshipType({ slug: 'knows', forward_label: 'knows', reverse_label: 'is known by' })
    }
  ])('treats an ambiguous $name 5xx create response as unknown and blocks another POST', async ({ resource, path, create }) => {
    let postCount = 0;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (resource === 'organizations' && request.method === 'GET' && pathOf(request) === path) return defaultResponse(request);
      if (pathOf(request) !== path || request.method !== 'POST') throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
      postCount += 1;
      return Response.json({ error: 'unavailable', message: 'commit result unavailable' }, { status: 503 });
    }));
    const controller = new DirectoryEntityController(client, 7);

    await expect(create(controller)).resolves.toMatchObject({ ok: false, kind: 'unknown' });
    expect(controller.createBlocked[resource]).toBe(true);
    await expect(create(controller)).resolves.toMatchObject({ ok: false, kind: 'blocked' });
    expect(postCount).toBe(1);
    if (resource === 'organizations') {
      await controller.refreshOrganizations();
      expect(controller.createBlocked.organizations).toBe(false);
    }
  });

  it('reconciles sibling primary flags and projection from the selected-person collection after making primary', async () => {
    let collectionReads = 0;
    const initial = [employment(11, 1, { is_primary: true }), employment(12, 1)];
    const reconciled = [employment(11, 2), employment(12, 2, { is_primary: true })];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/7/employments') {
        collectionReads += 1;
        return Response.json(collectionReads === 1
          ? { employments: initial, projection: { employment_id: 11, organization_id: 21, organization_name: 'Old Primary', vcard: {} } }
          : { employments: reconciled, projection: { employment_id: 12, organization_id: 21, organization_name: 'New Primary', vcard: {} } });
      }
      if (path === '/api/v1/employments/12' && request.method === 'GET') {
        return new Response(JSON.stringify(employment(12, 1)), { headers: { ETag: '"employment-12-r1"' } });
      }
      if (path === '/api/v1/employments/12/primary' && request.method === 'POST') {
        return new Response(JSON.stringify(employment(12, 2, { is_primary: true })), { headers: { ETag: '"employment-12-r2"' } });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    const controller = new DirectoryEntityController(client, 7);
    await controller.refreshEmployments();

    await expect(controller.makeEmploymentPrimary(12)).resolves.toMatchObject({ ok: true });

    expect(collectionReads).toBe(2);
    expect(controller.employments).toEqual(reconciled);
    expect(controller.employmentProjection).toMatchObject({ employment_id: 12, organization_name: 'New Primary' });
  });

  it('keeps the successful primary mutation locally and clears stale projection when reconciliation fails', async () => {
    const initial = [employment(11, 1, { is_primary: true, title: 'Old role', department: 'Legacy' }), employment(12, 1)];
    const initialProjection = { employment_id: 11, organization_id: 21, organization_name: 'Old Primary', vcard: {} };
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') {
        return new Response(JSON.stringify(initial[0]), { headers: { ETag: '"employment-11-r1"' } });
      }
      if (path === '/api/v1/employments/12' && request.method === 'GET') {
        return new Response(JSON.stringify(employment(12, 1)), { headers: { ETag: '"employment-12-r1"' } });
      }
      if (path === '/api/v1/employments/12/primary' && request.method === 'POST') {
        return new Response(JSON.stringify(employment(12, 2, { is_primary: true })), { headers: { ETag: '"employment-12-r2"' } });
      }
      if (path === '/api/v1/people/7/employments') {
        return Response.json({ error: 'unavailable', message: 'reconciliation failed' }, { status: 503 });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    const controller = new DirectoryEntityController(client, 7);
    controller.employments = initial;
    controller.employmentProjection = initialProjection;
    await expect(controller.prepareEmploymentMutation(11)).resolves.toEqual({ data: initial[0], etag: '"employment-11-r1"' });
    expect(controller.employmentRecords.get(11)).toEqual(initial[0]);

    await expect(controller.makeEmploymentPrimary(12)).resolves.toMatchObject({ ok: true });

    expect(controller.employments).toEqual([
      employment(11, 1, { is_primary: false, title: 'Old role', department: 'Legacy' }),
      employment(12, 2, { is_primary: true })
    ]);
    expect(controller.employments.filter((item) => item.is_primary)).toHaveLength(1);
    expect(controller.employmentRecords.has(11)).toBe(false);
    expect(controller.employmentRecords.get(12)).toEqual(employment(12, 2, { is_primary: true }));
    expect(controller.employmentProjection).toBeUndefined();
    expect(controller.errors.employments).toBe('reconciliation failed');
  });

  it.each([
    {
      name: 'create', method: 'POST', mutationPath: '/api/v1/employments', id: 12,
      run: (controller: DirectoryEntityController) => controller.createEmployment({ person_id: 7, organization_id: 21, source: 'user' })
    },
    {
      name: 'update', method: 'PATCH', mutationPath: '/api/v1/employments/11', id: 11,
      run: (controller: DirectoryEntityController) => controller.updateEmployment(11, () => ({ person_id: 7, organization_id: 21, source: 'user', title: 'Updated' }))
    },
    {
      name: 'end', method: 'POST', mutationPath: '/api/v1/employments/11/end', id: 11,
      run: (controller: DirectoryEntityController) => controller.endEmployment(11, { end_date: '2026-08' })
    }
  ])('keeps a successful $name locally and clears stale projection when reconciliation fails', async ({ method, mutationPath, id, run }) => {
    const returned = employment(id, 2, { title: `${id} returned` });
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') {
        return new Response(JSON.stringify(employment(11)), { headers: { ETag: '"employment-11-r1"' } });
      }
      if (path === mutationPath && request.method === method) {
        return new Response(JSON.stringify(returned), {
          status: mutationPath === '/api/v1/employments' ? 201 : 200,
          headers: { ETag: `"employment-${id}-r2"` }
        });
      }
      if (path === '/api/v1/people/7/employments') {
        return Response.json({ error: 'unavailable', message: 'reconciliation failed' }, { status: 503 });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    const controller = new DirectoryEntityController(client, 7);
    controller.employments = [employment(11)];
    controller.employmentProjection = { employment_id: 11, organization_id: 21, organization_name: 'Stale Org', vcard: {} };

    await expect(run(controller)).resolves.toMatchObject({ ok: true });

    expect(controller.employments).toContainEqual(returned);
    expect(controller.employmentProjection).toBeUndefined();
    expect(controller.errors.employments).toBe('reconciliation failed');
  });

  it('removes a successfully updated employment from this selection when the returned person changed', async () => {
    const moved = employment(11, 2, { person_id: 8, title: 'Moved role' });
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') {
        return new Response(JSON.stringify(employment(11)), { headers: { ETag: '"employment-11-r1"' } });
      }
      if (path === '/api/v1/employments/11' && request.method === 'PATCH') {
        return new Response(JSON.stringify(moved), { headers: { ETag: '"employment-11-r2"' } });
      }
      if (path === '/api/v1/people/7/employments') {
        return Response.json({ error: 'unavailable', message: 'reconciliation failed' }, { status: 503 });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    const controller = new DirectoryEntityController(client, 7);
    controller.employments = [employment(11), employment(12)];
    controller.employmentProjection = { employment_id: 11, organization_id: 21, organization_name: 'Stale Org', vcard: {} };

    await expect(controller.updateEmployment(11, () => ({
      person_id: 8, organization_id: 21, source: 'user', title: 'Moved role'
    }))).resolves.toMatchObject({ ok: true });

    expect(controller.employments).toEqual([employment(12)]);
    expect(controller.employmentRecords.get(11)).toEqual(moved);
    expect(controller.employmentProjection).toBeUndefined();
    expect(controller.errors.employments).toBe('reconciliation failed');
  });

  it('removes a successful delete locally and clears stale projection when reconciliation fails', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') {
        return new Response(JSON.stringify(employment(11)), { headers: { ETag: '"employment-11-r1"' } });
      }
      if (path === '/api/v1/employments/11' && request.method === 'DELETE') return new Response(null, { status: 204 });
      if (path === '/api/v1/people/7/employments') {
        return Response.json({ error: 'unavailable', message: 'reconciliation failed' }, { status: 503 });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    const controller = new DirectoryEntityController(client, 7);
    controller.employments = [employment(11), employment(12)];
    controller.employmentProjection = { employment_id: 11, organization_id: 21, organization_name: 'Stale Org', vcard: {} };

    await expect(controller.deleteEmployment(11)).resolves.toMatchObject({ ok: true });

    expect(controller.employments).toEqual([employment(12)]);
    expect(controller.employmentProjection).toBeUndefined();
    expect(controller.errors.employments).toBe('reconciliation failed');
  });

  it.each([
    {
      name: 'create', method: 'POST', mutationPath: '/api/v1/employments',
      run: (controller: DirectoryEntityController) => controller.createEmployment({ person_id: 7, organization_id: 21, source: 'user' })
    },
    {
      name: 'update', method: 'PATCH', mutationPath: '/api/v1/employments/11',
      run: (controller: DirectoryEntityController) => controller.updateEmployment(11, () => ({ person_id: 7, organization_id: 21, source: 'user', title: 'Updated' }))
    },
    {
      name: 'end', method: 'POST', mutationPath: '/api/v1/employments/11/end',
      run: (controller: DirectoryEntityController) => controller.endEmployment(11, { end_date: '2026-08' })
    },
    {
      name: 'delete', method: 'DELETE', mutationPath: '/api/v1/employments/11',
      run: (controller: DirectoryEntityController) => controller.deleteEmployment(11)
    }
  ])('reconciles the selected-person employment collection after successful $name', async ({ method, mutationPath, run }) => {
    let collectionReads = 0;
    const finalEmployment = employment(99, 4, { is_primary: true });
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/7/employments') {
        collectionReads += 1;
        return Response.json({ employments: [finalEmployment], projection: { employment_id: 99, organization_id: 21, organization_name: 'Reconciled Org', vcard: {} } });
      }
      if (path === '/api/v1/employments/11' && request.method === 'GET') {
        return new Response(JSON.stringify(employment(11, 1)), { headers: { ETag: '"employment-11-r1"' } });
      }
      if (path === mutationPath && request.method === method) {
        if (method === 'DELETE') return new Response(null, { status: 204 });
        return new Response(JSON.stringify(employment(method === 'POST' && mutationPath === '/api/v1/employments' ? 12 : 11, 2)), {
          status: mutationPath === '/api/v1/employments' ? 201 : 200,
          headers: { ETag: '"employment-mutation-r2"' }
        });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    const controller = new DirectoryEntityController(client, 7);
    controller.employments = [employment(11, 1)];

    await expect(run(controller)).resolves.toMatchObject({ ok: true });

    expect(collectionReads).toBe(1);
    expect(controller.employments).toEqual([finalEmployment]);
    expect(controller.employmentProjection).toMatchObject({ employment_id: 99 });
  });

  it('does not clear an unknown employment-create block during automatic mutation reconciliation', async () => {
    let createAttempted = false;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/employments' && request.method === 'POST') {
        createAttempted = true;
        return Response.json({ error: 'unavailable', message: 'unknown result' }, { status: 503 });
      }
      if (path === '/api/v1/employments/11' && request.method === 'GET') {
        return new Response(JSON.stringify(employment(11, 1)), { headers: { ETag: '"employment-11-r1"' } });
      }
      if (path === '/api/v1/employments/11/end' && request.method === 'POST') {
        return new Response(JSON.stringify(employment(11, 2, { is_current: false })), { headers: { ETag: '"employment-11-r2"' } });
      }
      if (path === '/api/v1/people/7/employments') return Response.json({ employments: [employment(11, 2, { is_current: false })] });
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    const controller = new DirectoryEntityController(client, 7);

    await expect(controller.createEmployment({ person_id: 7, organization_id: 21, source: 'user' })).resolves.toMatchObject({ ok: false, kind: 'unknown' });
    expect(createAttempted).toBe(true);
    await expect(controller.endEmployment(11, { end_date: '2026-08' })).resolves.toMatchObject({ ok: true });
    expect(controller.createBlocked.employments).toBe(true);
  });

  it('reconciles a created connection into the collection and record cache', async () => {
    let collectionReads = 0;
    const created = relationship(42, 1);
    const createdView = {
      counterpart_person_id: 8,
      counterpart_display_name: 'Synthetic Peer',
      counterpart_label: 'knows',
      counterpart_vcard_uid: 'person-8',
      direction: 'outgoing',
      relationship: created
    };
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/7/relationships') {
        collectionReads += 1;
        return Response.json({ relationships: collectionReads === 1 ? [] : [createdView] });
      }
      if (path === '/api/v1/person-relationships' && request.method === 'POST') {
        return new Response(JSON.stringify(created), { status: 201, headers: { ETag: '"relationship-42-r1"' } });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    const controller = new DirectoryEntityController(client, 7);

    await controller.refreshRelationships();
    await expect(controller.createRelationship({
      source_person_id: 7,
      target_person_id: 8,
      relationship_type_slug: 'knows'
    })).resolves.toMatchObject({ ok: true });

    expect(collectionReads).toBe(2);
    expect(controller.relationships).toEqual([createdView]);
    expect(controller.relationshipRecords.get(42)).toEqual(created);
  });

  it('keeps a successful relationship create explicit when collection reconciliation fails', async () => {
    const created = relationship(42, 1);
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/person-relationships' && request.method === 'POST') {
        return new Response(JSON.stringify(created), { status: 201, headers: { ETag: '"relationship-42-r1"' } });
      }
      if (path === '/api/v1/people/7/relationships') {
        return Response.json({ error: 'unavailable', message: 'relationship refresh failed' }, { status: 503 });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    const controller = new DirectoryEntityController(client, 7);

    await expect(controller.createRelationship({
      source_person_id: 7,
      target_person_id: 8,
      relationship_type_slug: 'knows'
    })).resolves.toMatchObject({ ok: true });

    expect(controller.relationshipRecords.get(42)).toEqual(created);
    expect(controller.errors.relationships).toBe('relationship refresh failed');
  });

  it.each([
    {
      name: 'system update',
      current: { ...relationshipType(), ownership: 'system' },
      run: (controller: DirectoryEntityController) => controller.updateRelationshipType(31, { forward_label: 'changed' })
    },
    {
      name: 'system delete',
      current: { ...relationshipType(), ownership: 'system' },
      run: (controller: DirectoryEntityController) => controller.deleteRelationshipType(31)
    },
    {
      name: 'non-deletable user delete',
      current: { ...relationshipType(), ownership: 'user', is_deletable: false },
      run: (controller: DirectoryEntityController) => controller.deleteRelationshipType(31)
    }
  ])('refuses a $name in the controller after the fresh read', async ({ current, run }) => {
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (pathOf(request) === '/api/v1/relationship-types/31' && request.method === 'GET') {
        return new Response(JSON.stringify(current), { headers: { ETag: '"relationship-type-31-r1"' } });
      }
      throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
    }));
    const controller = new DirectoryEntityController(client, 7);

    await expect(run(controller)).resolves.toMatchObject({ ok: false, kind: 'error', status: 403 });

    expect(requests).toHaveLength(1);
    expect(requests[0]?.method).toBe('GET');
  });

  it('uses a fresh relationship ETag for patch and delete without retrying a conflict', async () => {
    const requests: Request[] = [];
    let reads = 0;
    let patchWrites = 0;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/person-relationships/41' && request.method === 'GET') {
        reads += 1;
        return new Response(JSON.stringify(relationship(41, reads)), { headers: { ETag: `"relationship-41-r${reads}"` } });
      }
      if (path === '/api/v1/person-relationships/41' && request.method === 'PATCH') {
        patchWrites += 1;
        return Response.json({ error: 'revision_conflict', message: 'relationship changed elsewhere' }, { status: 412 });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    const controller = new DirectoryEntityController(client, 7);

    const result = await controller.updateRelationship(41, { end_date: '2026-08', notes: 'Retained draft' });

    expect(result).toMatchObject({ ok: false, kind: 'conflict', status: 412, current: { data: relationship(41, 2) } });
    expect(patchWrites).toBe(1);
    expect(reads).toBe(2);
    const patch = requests.find((request) => request.method === 'PATCH')!;
    expect(patch.headers.get('If-Match')).toBe('"relationship-41-r1"');
    await expect(patch.clone().json()).resolves.toEqual({ end_date: '2026-08', notes: 'Retained draft' });
  });

  it('builds an organization profile PUT from the same fresh profile that supplied its ETag', async () => {
    let putRequest: Request | undefined;
    const body = { names: [{ name: 'Synthetic Legal Name', name_kind: 'legal' as const, source: 'user' as const }] };
    const fresh = {
      ...organizationProfile(2),
      contact_points: [{
        organization_id: 21, address_kind: 'email', original_value: 'concurrent@example.test',
        normalized_value: 'concurrent@example.test', normalization: 'email', normalization_version: 1,
        envelope: { id: 52, ordinal: 4, source: 'vcard_import', source_ref: 'concurrent-card', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', vcard: { property: 'EMAIL' } }
      }]
    };
    let profileSeenByBuilder: unknown;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/organizations/21' && request.method === 'GET') {
        return new Response(JSON.stringify(fresh), { headers: { ETag: '"organization-21-r2"' } });
      }
      if (path === '/api/v1/organizations/21/profile' && request.method === 'PUT') {
        putRequest = request;
        return new Response(JSON.stringify({ ...organizationProfile(3), names: [{ organization_id: 21, name: 'Synthetic Legal Name', name_kind: 'legal', name_normalized: 'synthetic legal name', envelope: { id: 51, ordinal: 0, source: 'user', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', vcard: {} } }] }), {
          headers: { ETag: '"organization-21-r3"' }
        });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    const controller = new DirectoryEntityController(client, 7);

    await expect(controller.putOrganizationProfile(21, (profile) => {
      profileSeenByBuilder = profile;
      return body;
    })).resolves.toMatchObject({ ok: true });

    expect(profileSeenByBuilder).toEqual(fresh);
    expect(putRequest?.headers.get('If-Match')).toBe('"organization-21-r2"');
    await expect(putRequest?.clone().json()).resolves.toEqual(body);
    expect(controller.organizationRecords.get(21)?.organization.revision).toBe(3);
    expect(controller.organizationRecords.get(21)?.names?.[0]?.name).toBe('Synthetic Legal Name');
  });

  it('returns an organization profile conflict after refreshing the exact profile without retrying PUT', async () => {
    let organizationReads = 0;
    let putCount = 0;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/organizations/21' && request.method === 'GET') {
        organizationReads += 1;
        const revision = organizationReads + 1;
        return new Response(JSON.stringify(organizationProfile(revision)), { headers: { ETag: `"organization-21-r${revision}"` } });
      }
      if (path === '/api/v1/organizations/21/profile' && request.method === 'PUT') {
        putCount += 1;
        return Response.json({ error: 'revision_conflict', message: 'profile changed elsewhere' }, { status: 412 });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    const controller = new DirectoryEntityController(client, 7);

    const result = await controller.putOrganizationProfile(21, () => ({ names: [] }));

    expect(result).toMatchObject({ ok: false, kind: 'conflict', status: 412, current: { data: organizationProfile(3) } });
    expect(putCount).toBe(1);
    expect(organizationReads).toBe(2);
    expect(controller.organizationRecords.get(21)?.organization.revision).toBe(3);
  });

  it('aborts and generation-guards superseded network depths while retaining the latest successful projection', async () => {
    const depthTwo = deferredResponse();
    let depthTwoReads = 0;
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      if (pathOf(request) !== '/api/v1/people/7/network') return defaultResponse(request);
      const depth = Number(new URL(request.url).searchParams.get('depth'));
      if (depth === 2) {
        depthTwoReads += 1;
        return depthTwoReads === 1
          ? depthTwo.promise
          : Response.json({ error: 'unavailable', message: 'network down' }, { status: 503 });
      }
      if (depth === 3) return Response.json(network(7, 3));
      return Response.json(network(7, 1));
    }));
    const controller = new DirectoryEntityController(client, 7);
    await controller.loadNetwork(1);

    const stale = controller.loadNetwork(2);
    await vi.waitFor(() => expect(requests.some((request) => new URL(request.url).searchParams.get('depth') === '2')).toBe(true));
    const current = controller.loadNetwork(3);
    await current;
    const depthTwoRequest = requests.find((request) => new URL(request.url).searchParams.get('depth') === '2')!;
    expect(depthTwoRequest.signal.aborted).toBe(true);

    depthTwo.resolve(Response.json(network(7, 2)));
    await stale;
    expect(controller.network?.depth).toBe(3);

    const failed = controller.loadNetwork(2);
    await failed;
    expect(controller.network?.depth).toBe(3);
    expect(controller.errors.network).toBe('network down');
  });

  it('aborts every owned request and makes abort-ignorant late responses inert on destroy', async () => {
    const pending = deferredResponse();
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      return pending.promise;
    }));
    const controller = new DirectoryEntityController(client, 7);

    const load = Promise.all([controller.load(), controller.loadNetwork()]);
    await vi.waitFor(() => expect(requests).toHaveLength(4));
    controller.destroy();
    expect(requests.every((request) => request.signal.aborted)).toBe(true);

    pending.resolve(Response.json({ employments: [employment(11)] }));
    await load;
    expect(controller.employments).toEqual([]);
    expect(controller.relationships).toEqual([]);
    expect(controller.relationshipTypes).toEqual([]);
    expect(controller.network).toBeNull();
  });

  it('wires destroy cancellation into an in-flight create without setting an unknown-outcome block', async () => {
    const pending = deferredResponse();
    let createRequest: Request | undefined;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (pathOf(request) === '/api/v1/employments' && request.method === 'POST') {
        createRequest = request;
        return pending.promise;
      }
      return defaultResponse(request);
    }));
    const controller = new DirectoryEntityController(client, 7);

    const create = controller.createEmployment({ person_id: 7, organization_id: 21, source: 'user' });
    await vi.waitFor(() => expect(createRequest).toBeDefined());
    controller.destroy();

    expect(createRequest?.signal.aborted).toBe(true);
    pending.resolve(new Response(JSON.stringify(employment(12)), { status: 201, headers: { ETag: '"employment-12-r1"' } }));
    await expect(create).resolves.toMatchObject({ ok: false, kind: 'error' });
    expect(controller.createBlocked.employments).toBe(false);
  });
});

it.each([
  ['Edit job', '"opaque-job-opening"'], ['End job', '"opaque-job-opening"'], ['Make primary', '"opaque-job-opening"'],
  ['Delete job', '"opaque-job-opening"'], ['Organization', '"opaque-organization-opening"'], ['Organization profile', '"opaque-organization-opening"'],
  ['Delete organization', '"opaque-organization-opening"'], ['Edit connection', '"opaque-connection-opening"'], ['Delete connection', '"opaque-connection-opening"'],
])('uses the opened version for %s and the conflict version for its retry', async (action, openingTag) => {
  const requests: Request[] = [];
  let readRevision = 1;
  const client = createAPIClient(async input => {
    const request = requestOf(input);
    requests.push(request);
    if (request.method !== 'GET') return Response.json({ message: 'Changed elsewhere' }, { status: 412 });
    const path = pathOf(request);
    if (path.includes('/organizations/21')) return Response.json(organizationProfile(readRevision), { headers: { ETag: `"opaque-organization-${readRevision === 1 ? "opening" : "current"}"` } });
    if (path.includes('/employments/11')) return Response.json(employment(11, readRevision), { headers: { ETag: `"opaque-job-${readRevision === 1 ? "opening" : "current"}"` } });
    return Response.json(relationship(41, readRevision), { headers: { ETag: `"opaque-connection-${readRevision === 1 ? "opening" : "current"}"` } });
  });
  const controller = new DirectoryEntityController(client, 7);
  const send = (prepared?: Prepared<never>) => {
    switch (action) {
      case 'Edit job': return controller.updateEmployment(11, () => ({ person_id: 7, organization_id: 21, source: 'user', title: 'Draft title' }), prepared);
      case 'End job': return controller.endEmployment(11, { end_date: '2026' }, prepared);
      case 'Make primary': return controller.makeEmploymentPrimary(11, prepared);
      case 'Delete job': return controller.deleteEmployment(11, prepared);
      case 'Organization': return controller.updateOrganization(21, { name: 'Draft name', kind: 'company' }, prepared);
      case 'Organization profile': return controller.putOrganizationProfile(21, () => ({}), prepared);
      case 'Delete organization': return controller.deleteOrganization(21, prepared);
      case 'Edit connection': return controller.updateRelationship(41, { notes: 'Draft note' }, prepared);
      default: return controller.deleteRelationship(41, prepared);
    }
  };
  try {
    const opened = action.includes('organization') || action.startsWith('Organization') ? await controller.prepareOrganizationMutation(21)
      : action.includes('connection') ? await controller.prepareRelationshipMutation(41) : await controller.prepareEmploymentMutation(11);
    readRevision = 2;
    requests.length = 0;
    const conflict = await send(opened as Prepared<never>);
    expect(conflict).toMatchObject({ ok: false, kind: 'conflict' });
    expect(requests[0].method).not.toBe('GET');
    expect(requests[0].headers.get('If-Match')).toBe(openingTag);
    requests.length = 0;
    expect(await send((conflict as { current?: Prepared<never> }).current)).toMatchObject({ ok: false, kind: 'conflict' });
    expect(requests[0].method).not.toBe('GET');
    expect(requests[0].headers.get('If-Match')).toBe(openingTag.replace('opening', 'current'));
  } finally { controller.destroy(); }
});
