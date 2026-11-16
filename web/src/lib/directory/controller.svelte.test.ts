import { emptyProfile } from './profile-fixtures.test-support';
import { directoryPerson as fixtureDirectoryPerson, pathOf } from './entity-fixtures.test-support';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import { DirectoryController } from './controller.svelte';

const directoryPerson = (id: number) => fixtureDirectoryPerson(id, { revision: 1, display_name: `Person ${id}` });

function detailResponse(path: string, personID = 7): Response {
  if (path === '/api/v1/relationship-types') return Response.json({ relationship_types: [] });
  if (path === `/api/v1/people/${personID}/network`) return Response.json({ root_person_id: personID, depth: 1, truncated: false, nodes: [], edges: [] });
  if (path === `/api/v1/people/${personID}`) return Response.json({ id: personID, revision: 3 });
  if (path.endsWith('/profile')) return Response.json({ person: { id: personID, revision: 3 }, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] });
  if (path.endsWith('/attributes')) return Response.json({ person_id: personID, attributes: [] });
  if (path.endsWith('/contact-state')) return Response.json({ person_id: personID, cadence_status: 'unknown', interaction_count: 0, computed_at: '2026-08-01T00:00:00Z', stale: false });
  if (path.endsWith('/employments')) return Response.json({ employments: [] });
  if (path.endsWith('/relationships')) return Response.json({ relationships: [] });
  throw new Error(`unexpected detail path ${path}`);
}

function editableDetailResponse(path: string, personID: number): Response {
  if (path === `/api/v1/people/${personID}`) {
    return new Response(JSON.stringify({ id: personID, revision: 3 }), {
      headers: { ETag: `"person-${personID}-r3"` }
    });
  }
  if (path === `/api/v1/people/${personID}/profile`) {
    return new Response(JSON.stringify({
      person: { id: personID, revision: 3 }, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: []
    }), { headers: { ETag: `"person-${personID}-r3"` } });
  }
  return detailResponse(path, personID);
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  let reject!: (cause: unknown) => void;
  const promise = new Promise<Response>((next, fail) => { resolve = next; reject = fail; });
  return { promise, resolve, reject };
}

describe('DirectoryController', () => {
  it('loads page one when browser restoration has default Directory filters', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ people: [directoryPerson(1)] }));
    const controller = new DirectoryController(createAPIClient(fetchFn));

    controller.applyURLState({
      directoryQuery: '', directoryContactState: '', directoryCategory: '',
      directoryOrganization: '', directoryPrimaryChannel: '', directoryLastContactAfter: '', directoryLastContactBefore: '', directorySort: 'name' as const, directoryPersonID: null
    });

    await vi.waitFor(() => expect(controller.rows).toEqual([directoryPerson(1)]));
    expect(fetchFn).toHaveBeenCalledOnce();
  });

  it('serializes filters, aborts a superseded page, and only applies the current generation', async () => {
    let resolveFirst: ((response: Response) => void) | undefined;
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (pathOf(request) !== '/api/v1/people/directory') throw new Error(`unexpected ${pathOf(request)}`);
      if (requests.length === 1) return new Promise<Response>((resolve) => { resolveFirst = resolve; });
      return Response.json({ people: [directoryPerson(2)] });
    });
    const controller = new DirectoryController(createAPIClient(fetchFn));
    controller.applyURLState({
      directoryQuery: 'alice', directoryContactState: 'active', directoryCategory: 'friend',
      directoryOrganization: 'Example Co', directoryPrimaryChannel: 'email', directoryLastContactAfter: '2026-01-01', directoryLastContactBefore: '2026-08-31', directorySort: 'last_contact_desc', directoryPersonID: null
    });
    await vi.waitFor(() => expect(resolveFirst).toBeDefined());

    controller.setFilters({ directoryQuery: 'bob' });
    await vi.waitFor(() => expect(requests).toHaveLength(2));
    expect(requests[0]!.signal.aborted).toBe(true);
    const parameters = new URL(requests[1]!.url).searchParams;
    expect(parameters.get('q')).toBe('bob');
    expect(parameters.get('contact_state')).toBe('active');
    expect(parameters.get('category')).toBe('friend');
    expect(parameters.get('organization')).toBe('Example Co');
    expect(parameters.get('primary_channel')).toBe('email');
    expect(parameters.get('last_contact_after')).toBe('2026-01-01T00:00:00Z');
    expect(parameters.get('last_contact_before')).toBe('2026-08-31T23:59:59.999999999Z');
    expect(parameters.get('sort')).toBe('last_contact_desc');

    await vi.waitFor(() => expect(controller.rows).toEqual([directoryPerson(2)]));

    resolveFirst?.(Response.json({ people: [directoryPerson(1)] }));
    await Promise.resolve();
    expect(controller.rows).toEqual([directoryPerson(2)]);
  });

  it('dedupes appended rows, stops repeated cursors, and retains a retryable cursor after transient failure', async () => {
    let secondPageFails = true;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const cursor = new URL(request.url).searchParams.get('cursor');
      if (!cursor) return Response.json({ people: [directoryPerson(1)], next_cursor: 'page-2' });
      if (secondPageFails) {
        secondPageFails = false;
        throw new TypeError('network down');
      }
      return Response.json({ people: [directoryPerson(1), directoryPerson(2)], next_cursor: 'page-2' });
    });
    const controller = new DirectoryController(createAPIClient(fetchFn));
    await controller.loadFirstPage();
    await controller.loadNextPage();

    expect(controller.rows).toEqual([directoryPerson(1)]);
    expect(controller.cursor).toBe('page-2');
    expect(controller.pageError).toBe('network down');
    expect(controller.pageRecovery).toBe('retry');

    await controller.loadNextPage();
    expect(controller.rows).toEqual([directoryPerson(1), directoryPerson(2)]);
    expect(controller.cursor).toBe('page-2');

    await controller.loadNextPage();
    expect(controller.cursor).toBeNull();
    expect(controller.pageError).toContain('repeated a cursor');
    expect(controller.pageRecovery).toBe('reload');
  });

  it('retains rows through invalid-cursor recovery until reloading page one succeeds', async () => {
    let firstPage = 0;
    let finishReload: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const cursor = new URL(request.url).searchParams.get('cursor');
      if (cursor) {
        return Response.json({ error: 'invalid_cursor', message: 'directory changed' }, { status: 400 });
      }
      firstPage += 1;
      if (firstPage === 1) return Response.json({ people: [directoryPerson(1)], next_cursor: 'stale-cursor' });
      return new Promise<Response>((resolve) => { finishReload = resolve; });
    });
    const controller = new DirectoryController(createAPIClient(fetchFn));

    await controller.loadFirstPage();
    await controller.loadNextPage();
    expect(controller.rows).toEqual([directoryPerson(1)]);
    expect(controller.cursor).toBeNull();
    expect(controller.pageRecovery).toBe('reload');

    const reload = controller.reloadFirstPage();
    await vi.waitFor(() => expect(finishReload).toBeDefined());
    expect(controller.rows).toEqual([directoryPerson(1)]);
    finishReload?.(Response.json({ people: [directoryPerson(2)] }));
    await reload;
    expect(controller.rows).toEqual([directoryPerson(2)]);
    expect(controller.pageRecovery).toBeNull();
  });

  it('keeps rows visible while a filter change loads and replaces them on success', async () => {
    const commits: Array<[Record<string, unknown>, string]> = [];
    let resolveSecond: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const query = new URL(request.url).searchParams.get('q');
      if (query === 'bob') return new Promise<Response>((resolve) => { resolveSecond = resolve; });
      return Response.json({ people: [directoryPerson(1)] });
    });
    const controller = new DirectoryController(createAPIClient(fetchFn), (patch, history) => commits.push([patch, history]));

    await controller.loadFirstPage();
    controller.setFilters({ directoryQuery: 'bob' }, 'replace');
    await vi.waitFor(() => expect(resolveSecond).toBeDefined());

    expect(controller.loading).toBe(true);
    expect(controller.rows).toEqual([directoryPerson(1)]);
    expect(commits).toEqual([[{ directoryQuery: 'bob' }, 'replace']]);

    resolveSecond?.(Response.json({ people: [directoryPerson(2)] }));
    await vi.waitFor(() => expect(controller.loading).toBe(false));
    expect(controller.rows).toEqual([directoryPerson(2)]);
  });

  it('pushes a history entry for a filter change by default', async () => {
    const commits: Array<[Record<string, unknown>, string]> = [];
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ people: [] }));
    const controller = new DirectoryController(createAPIClient(fetchFn), (patch, history) => commits.push([patch, history]));

    controller.setFilters({ directoryContactState: 'active' });

    expect(commits).toEqual([[{ directoryContactState: 'active' }, 'push']]);
  });

  it('clears terminal-error rows when a filter change has a new-request failure', async () => {
    let firstPage = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const cursor = new URL(request.url).searchParams.get('cursor');
      const query = new URL(request.url).searchParams.get('q');
      if (cursor) return Response.json({ error: 'invalid_cursor', message: 'directory changed' }, { status: 400 });
      firstPage += 1;
      if (firstPage === 1) return Response.json({ people: [directoryPerson(1)], next_cursor: 'stale-cursor' });
      if (query === 'bob') return Response.json({ error: 'unavailable', message: 'new query failed' }, { status: 503 });
      throw new Error(`unexpected directory request ${request.url}`);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn));

    await controller.loadFirstPage();
    await controller.loadNextPage();
    expect(controller.rows).toEqual([directoryPerson(1)]);
    expect(controller.pageRecovery).toBe('reload');

    controller.setFilters({ directoryQuery: 'bob' });
    expect(controller.rows).toEqual([directoryPerson(1)]);
    expect(controller.loading).toBe(true);
    await vi.waitFor(() => expect(controller.error).toBe('new query failed'));
    expect(controller.rows).toEqual([]);
    expect(controller.pageRecovery).toBeNull();
  });

  it('clears a stale 404 selection and retains partial detail errors and ETags', async () => {
    const commits: Array<Record<string, unknown>> = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/8') return Response.json({ error: 'not_found', message: 'gone' }, { status: 404 });
      if (path === '/api/v1/people/7/profile') return Response.json({ error: 'down', message: 'structured down' }, { status: 503 });
      if (path === '/api/v1/people/7') return new Response(JSON.stringify({ id: 7, revision: 3 }), { headers: { ETag: '"person-etag"' } });
      if (path === '/api/v1/people/7/attributes') return new Response(JSON.stringify({ person_id: 7, attributes: [] }), { headers: { ETag: '"attributes-etag"' } });
      return detailResponse(path);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn), (patch) => commits.push(patch));

    await controller.loadSelection(8, true);
    expect(controller.selectedPersonID).toBeNull();
    expect(controller.entity).toBeNull();
    expect(commits).toContainEqual({ directoryPersonID: null });

    await controller.loadSelection(7, true);
    expect(controller.detail?.person).toEqual({ id: 7, revision: 3 });
    expect(controller.detail?.errors.structuredProfile).toBe('structured down');
    expect(controller.detail?.etags).toMatchObject({ person: '"person-etag"', attributes: '"attributes-etag"' });
    expect(controller.detail?.attributes).toEqual({ person_id: 7, attributes: [] });
    expect(controller.detail?.structuredProfile).toBeUndefined();
  });

  it('retains successful detail sections and ETags when one section rejects', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/7/profile') throw new TypeError('profile network down');
      if (path === '/api/v1/people/7') return new Response(JSON.stringify({ id: 7, revision: 3 }), { headers: { ETag: '"person-etag"' } });
      if (path === '/api/v1/people/7/attributes') return new Response(JSON.stringify({ person_id: 7, attributes: [] }), { headers: { ETag: '"attributes-etag"' } });
      return detailResponse(path);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn));

    await controller.loadSelection(7, true);

    expect(controller.detail?.person).toEqual({ id: 7, revision: 3 });
    expect(controller.detail?.attributes).toEqual({ person_id: 7, attributes: [] });
    expect(controller.detail?.etags).toMatchObject({ person: '"person-etag"', attributes: '"attributes-etag"' });
    expect(controller.detail?.errors.structuredProfile).toBe('profile network down');
    expect(controller.detail?.errors.person).toBeUndefined();
  });

  it('destroys the old entity controller before a new selection and ignores its late responses', async () => {
    const oldEmployments = deferredResponse();
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/people/7/employments') return oldEmployments.promise;
      if (path === '/api/v1/people/8/employments') return Response.json({ employments: [{ id: 80, person_id: 8, organization_id: 1, is_current: true, is_primary: true, source: 'user', revision: 1, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' }] });
      return detailResponse(path, path.includes('/people/8') ? 8 : 7);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn));

    await controller.loadSelection(7, true);
    const oldEntity = controller.entity!;
    const oldLoad = oldEntity.load();
    await vi.waitFor(() => expect(requests.some(request => pathOf(request) === '/api/v1/people/7/employments')).toBe(true));
    const oldRequest = requests.find((request) => pathOf(request) === '/api/v1/people/7/employments')!;
    await controller.loadSelection(8, true);
    await controller.entity!.load();

    expect(oldRequest.signal.aborted).toBe(true);
    expect(controller.entity?.personID).toBe(8);
    await vi.waitFor(() => expect(controller.entity?.employments.map((item) => item.id)).toEqual([80]));

    oldEmployments.resolve(Response.json({ employments: [{ id: 70, person_id: 7, organization_id: 1, is_current: true, is_primary: true, source: 'user', revision: 1, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' }] }));
    await oldLoad;
    await vi.waitFor(() => expect(oldEntity.employmentsLoading).toBe(false));
    expect(oldEntity.employments).toEqual([]);
    expect(controller.entity?.personID).toBe(8);
    expect(controller.entity?.employments.map((item) => item.id)).toEqual([80]);
  });

  it('loads profile sections once per selection and leaves entity reads to Details', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      return detailResponse(path);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn));

    await controller.loadSelection(7, true);
    expect(requests.map(pathOf)).not.toContain('/api/v1/people/7/employments');
    expect(requests.map(pathOf)).not.toContain('/api/v1/people/7/relationships');
    await controller.entity!.load();

    const paths = requests.map(pathOf).sort();
    expect(paths).toEqual([...new Set(paths)]);
    expect(paths).toContain('/api/v1/people/7/employments');
    expect(paths).toContain('/api/v1/people/7/relationships');
    expect(paths).toContain('/api/v1/relationship-types');
    expect(paths).not.toContain('/api/v1/people/7/network');
    expect(controller.detail).not.toHaveProperty('employments');
    expect(controller.detail).not.toHaveProperty('relationships');

    await controller.loadSelection(7);
    await controller.entity!.load();
    expect(requests.filter(request => pathOf(request) === '/api/v1/people/7/relationships')).toHaveLength(2);
    controller.destroy();
  });

  it('reloads Directory ordering, cursor membership, and the open Network after a person rename', async () => {
    const requests: Request[] = [];
    let directoryRead = 0;
    let renamed = false;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') {
        directoryRead += 1;
        return Response.json({ people: directoryRead === 1
          ? [directoryPerson(7), directoryPerson(8)]
          : [directoryPerson(8), { ...directoryPerson(7), revision: 5, display_name: 'Concurrent Name' }] });
      }
      if (path === '/api/v1/people/7' && request.method === 'GET') {
        return new Response(JSON.stringify({ id: 7, revision: renamed ? 4 : 3, display_name: renamed ? 'Zed Example' : 'Person 7', participant_ids: [], created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', vcard_uid: 'person-7' }), { headers: { ETag: `"person-7-r${renamed ? 4 : 3}"` } });
      }
      if (path === '/api/v1/people/7/network') return Response.json({ root_person_id: 7, depth: 1, truncated: false, nodes: [{ id: 'person:7', kind: 'person', entity_id: 7, label: renamed ? 'Zed Example' : 'Person 7', hop: 0 }], edges: [] });
      if (path === '/api/v1/people/7' && request.method === 'PATCH') {
        renamed = true;
        return new Response(JSON.stringify({ id: 7, revision: 4, display_name: 'Zed Example', participant_ids: [], created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', vcard_uid: 'person-7' }), { headers: { ETag: '"person-7-r4"' } });
      }
      if (path === '/api/v1/people/7/profile') {
        return new Response(JSON.stringify(emptyProfile({ id: 7, revision: renamed ? 4 : 3, display_name: renamed ? 'Zed Example' : 'Person 7', participant_ids: [], created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', vcard_uid: 'person-7' })), { headers: { ETag: `"person-7-r${renamed ? 4 : 3}"` } });
      }
      if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
      return detailResponse(path);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn));

    await controller.loadFirstPage();
    await controller.loadSelection(7, true);
    await controller.entity?.loadNetwork();
    await controller.profile?.rename('Zed Example');
    expect(controller.entity?.network?.nodes[0]?.label).toBe('Zed Example');

    expect(controller.rows).toEqual([directoryPerson(8), { ...directoryPerson(7), revision: 5, display_name: 'Concurrent Name' }]);
    expect(controller.detail?.person?.display_name).toBe('Zed Example');
    expect(requests.filter((request) => pathOf(request) === '/api/v1/people/directory')).toHaveLength(2);
  });

  it('removes a renamed person that no longer matches the active Directory search', async () => {
    let directoryRead = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') {
        directoryRead += 1;
        return Response.json({ people: directoryRead === 1 ? [directoryPerson(7)] : [] });
      }
      if (path === '/api/v1/people/7' && request.method === 'GET') {
        return new Response(JSON.stringify({ id: 7, revision: 3, display_name: 'Person 7' }), { headers: { ETag: '"person-7-r3"' } });
      }
      if (path === '/api/v1/people/7' && request.method === 'PATCH') {
        return new Response(JSON.stringify({ id: 7, revision: 4, display_name: 'Zed Example' }), { headers: { ETag: '"person-7-r4"' } });
      }
      return editableDetailResponse(path, 7);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn));
    controller.query = 'person';

    await controller.loadFirstPage();
    await controller.loadSelection(7, true);
    await controller.profile?.rename('Zed Example');

    expect(controller.rows).toEqual([]);
  });

  it('clears a Directory display name after a null rename', async () => {
    let directoryRead = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') {
        directoryRead += 1;
        return Response.json({ people: directoryRead === 1
          ? [directoryPerson(7)]
          : [{ ...directoryPerson(7), revision: 4, display_name: undefined }] });
      }
      if (path === '/api/v1/people/7' && request.method === 'GET') {
        return new Response(JSON.stringify({ id: 7, revision: 3, display_name: 'Person 7' }), { headers: { ETag: '"person-7-r3"' } });
      }
      if (path === '/api/v1/people/7' && request.method === 'PATCH') {
        return new Response(JSON.stringify({ id: 7, revision: 4 }), { headers: { ETag: '"person-7-r4"' } });
      }
      return editableDetailResponse(path, 7);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn));

    await controller.loadFirstPage();
    await controller.loadSelection(7, true);
    await controller.profile?.rename(null);

    expect(controller.rows).toEqual([{ ...directoryPerson(7), revision: 4, display_name: undefined }]);
  });

  it('refreshes Directory membership after an employment mutation', async () => {
    let directoryRead = 0;
    let employmentCreated = false;
    const createdEmployment = {
      id: 11, person_id: 7, organization_id: 21, is_current: true, is_primary: true,
      source: 'user', revision: 1, created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z'
    };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') {
        directoryRead += 1;
        return Response.json({ people: directoryRead === 1 ? [directoryPerson(7)] : [] });
      }
      if (path === '/api/v1/employments' && request.method === 'POST') {
        employmentCreated = true;
        return Response.json(createdEmployment, { status: 201, headers: { ETag: '"employment-11-r1"' } });
      }
      if (path === '/api/v1/people/7/employments') {
        return Response.json({ employments: employmentCreated ? [createdEmployment] : [] });
      }
      return editableDetailResponse(path, 7);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn));
    controller.organization = 'Old Org';

    await controller.loadFirstPage();
    await controller.loadSelection(7, true);
    await vi.waitFor(() => expect(controller.entity?.employmentsLoading).toBe(false));
    await controller.entity?.createEmployment({ person_id: 7, organization_id: 21, source: 'user' });

    expect(controller.rows).toEqual([]);
    expect(directoryRead).toBe(2);
  });

  function categoryHarness(filtered: boolean) {
    const commits: Array<Record<string, unknown>> = [];
    const directoryRequests: Request[] = [];
    let directoryRead = 0;
    let profileWrite = 0;
    const category = {
      person_id: 7, original_value: 'Straße', normalized_value: 'strasse',
      envelope: { id: 31, ordinal: 0, source: 'user', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', vcard: {} }
    };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') {
        directoryRequests.push(request);
        directoryRead += 1;
        if (!filtered) return Response.json({ people: [directoryPerson(7)] });
        if (directoryRead === 1) return Response.json({ people: [directoryPerson(8)], next_cursor: 'initial-page-2' });
        if (directoryRead === 2) return Response.json({ people: [directoryPerson(9)], next_cursor: 'initial-page-3' });
        if (directoryRead === 3) return Response.json({ people: [directoryPerson(7), directoryPerson(8)], next_cursor: 'added-page-2' });
        if (directoryRead === 4) return Response.json({ people: [directoryPerson(9)], next_cursor: 'added-page-3' });
        if (directoryRead === 5) return Response.json({ people: [directoryPerson(8)], next_cursor: 'closed-page-2' });
        return Response.json({ people: [directoryPerson(9)], next_cursor: 'closed-page-3' });
      }
      if (path === '/api/v1/people/7/profile' && request.method === 'PATCH') {
        profileWrite += 1;
        return new Response(JSON.stringify({
          person: { id: 7, revision: 3 + profileWrite }, names: [], contact_points: [], addresses: [], dates: [],
          categories: profileWrite === 1 ? [category] : [], media: []
        }), { headers: { ETag: `"person-7-r${3 + profileWrite}"` } });
      }
      if (path === '/api/v1/people/7') return new Response(JSON.stringify({ id: 7, revision: 3 + profileWrite }), { headers: { ETag: `"person-7-r${3 + profileWrite}"` } });
      if (path === '/api/v1/people/7/profile') return new Response(JSON.stringify({ person: { id: 7, revision: 3 + profileWrite }, names: [], contact_points: [], addresses: [], dates: [], categories: profileWrite === 1 ? [category] : [], media: [] }), { headers: { ETag: `"person-7-r${3 + profileWrite}"` } });
      if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
      return detailResponse(path);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn), (patch) => commits.push(patch));
    if (filtered) {
      controller.query = 'synthetic person';
      controller.contactState = 'active';
      controller.category = 'STRASSE';
      controller.organization = 'Example Org';
      controller.primaryChannel = 'email';
    }
    return { controller, commits, directoryRequests, category };
  }

  it.each([true, false])('reconciles Unicode categories after add and close (filtered=%s)', async filtered => {
    const { controller, commits, directoryRequests, category } = categoryHarness(filtered);
    await controller.loadFirstPage();
    if (filtered) await controller.loadNextPage();
    await controller.loadSelection(7, true);
    await controller.profile?.patchProfile({ categories: { add: [{ original_value: 'Straße', envelope: { source: 'user' } }] } });
    expect(controller.rows.map((row) => row.id)).toEqual(filtered ? [7, 8, 9] : [7]);
    expect(controller.rows[0]?.categories).toEqual(['Straße']);
    expect(controller.cursor).toBe(filtered ? 'added-page-3' : null);
    expect(controller.selectedPersonID).toBe(7);
    expect(controller.detail?.structuredProfile?.categories).toEqual([category]);
    expect(commits).toEqual([{ directoryPersonID: 7 }]);

    await controller.profile?.patchProfile({ categories: { supersede: [31] } });

    expect(controller.rows.map((row) => row.id)).toEqual(filtered ? [8, 9] : [7]);
    if (!filtered) expect(controller.rows[0]?.categories).toEqual([]);
    expect(controller.cursor).toBe(filtered ? 'closed-page-3' : null);
    expect(controller.selectedPersonID).toBe(7);
    expect(controller.detail?.structuredProfile?.categories).toEqual([]);
    if (!filtered) expect(directoryRequests).toHaveLength(1);
    for (const request of directoryRequests.slice(2)) {
      const parameters = new URL(request.url).searchParams;
      expect(parameters.get('q')).toBe('synthetic person');
      expect(parameters.get('contact_state')).toBe('active');
      expect(parameters.get('category')).toBe('STRASSE');
      expect(parameters.get('organization')).toBe('Example Org');
      expect(parameters.get('primary_channel')).toBe('email');
    }
  });

  it('keeps the observed Directory channel after a preferred-channel set and clear', async () => {
    let directoryRead = 0;
    let attributeWrite = 0;
    const definition = {
      id: 1, universal_id: 'primary-channel-id', object_type: 'person', slug: 'primary_channel', label: 'Primary channel',
      value_type: 'text', field_type: 'select', cardinality: 'single', display_order: 0, is_required: false,
      ownership: 'system', ui_creatable: true, ui_editable: true, api_mutable: true, is_searchable: false,
      is_sensitive: false, is_audited: true, is_deletable: false, history_exempt: false,
      options: { choices: [
        { value: 'email', label: 'Email' }, { value: 'phone', label: 'Phone' },
        { value: 'sms', label: 'SMS' }, { value: 'chat', label: 'Chat' },
        { value: 'in_person', label: 'In person' }
      ] },
      is_active: true,
      revision: 1, created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z'
    };
    const current = {
      id: 41, person_id: 7, definition_id: 1, definition_slug: 'primary_channel', ordinal: 0,
      value: { type: 'text', text: 'chat' }, active_from: '2026-08-01T00:00:00Z', created_at: '2026-08-01T00:00:00Z', source: 'user'
    };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') {
        directoryRead += 1;
        return Response.json({
          people: [{ ...directoryPerson(7), primary_channel: 'email' }, directoryPerson(8)],
          next_cursor: 'observed-next'
        });
      }
      if (path === '/api/v1/people/7/attributes/primary_channel') {
        attributeWrite += 1;
        return attributeWrite === 1
          ? Response.json({ dry_run: false, value: current })
          : Response.json({ dry_run: false, superseded: { ...current, active_until: '2026-08-02T00:00:00Z', superseded_at: '2026-08-02T00:00:00Z' } });
      }
      if (path === '/api/v1/people/7') return new Response(JSON.stringify({ id: 7, revision: 3 }), { headers: { ETag: '"person-7-r3"' } });
      if (path === '/api/v1/people/7/profile') return new Response(JSON.stringify({ person: { id: 7, revision: 3 }, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] }), { headers: { ETag: '"person-7-r3"' } });
      if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [{ definition, current: attributeWrite === 1 ? [current] : [], history: [] }] });
      return detailResponse(path);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn));
    controller.primaryChannel = 'email';
    await controller.loadFirstPage();
    await controller.loadSelection(7, true);

    await controller.profile?.setAttribute('primary_channel', { value: { type: 'text', text: 'chat' } });
    expect(controller.profile?.attributes?.attributes?.[0]?.current).toEqual([current]);
    expect(controller.rows[0]?.primary_channel).toBe('email');
    expect(controller.cursor).toBe('observed-next');
    expect(directoryRead).toBe(1);

    await controller.profile?.clearAttribute('primary_channel', current.id);
    expect(controller.profile?.attributes?.attributes?.[0]?.current).toEqual([]);
    expect(controller.rows[0]?.primary_channel).toBe('email');
    expect(controller.cursor).toBe('observed-next');
    expect(controller.selectedPersonID).toBe(7);
    expect(directoryRead).toBe(1);
  });

  it('reconciles the written row when selection changes before the write completes', async () => {
    const commits: Array<Record<string, unknown>> = [];
    const writeResponse = deferredResponse();
    const directoryRequests: Request[] = [];
    const category = {
      person_id: 7, original_value: 'VIP', normalized_value: 'vip',
      envelope: { id: 31, ordinal: 0, source: 'user', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', vcard: {} }
    };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') {
        directoryRequests.push(request);
        return directoryRequests.length === 1
          ? Response.json({ people: [directoryPerson(7), directoryPerson(8)], next_cursor: 'old-next' })
          : Response.json({ people: [{ ...directoryPerson(7), categories: ['stale'] }, directoryPerson(8)], next_cursor: 'new-next' });
      }
      if (path === '/api/v1/people/7/profile' && request.method === 'PATCH') return writeResponse.promise;
      return editableDetailResponse(path, path.startsWith('/api/v1/people/8') ? 8 : 7);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn), (patch) => commits.push(patch));
    controller.category = 'vip';
    await controller.loadFirstPage();
    await controller.loadSelection(7, true);
    const oldProfile = controller.profile!;

    const write = oldProfile.patchProfile({ categories: { add: [{ original_value: 'VIP', envelope: { source: 'user' } }] } });
    await vi.waitFor(() => expect(fetchFn.mock.calls.some(([input]) => {
      const request = input instanceof Request ? input : new Request(input);
      return request.method === 'PATCH';
    })).toBe(true));
    await controller.loadSelection(8, true);
    const selectedProfile = controller.profile;
    const selectedDetail = controller.detail;

    writeResponse.resolve(new Response(JSON.stringify({
      person: { id: 7, revision: 4 }, names: [], contact_points: [], addresses: [], dates: [], categories: [category], media: []
    }), { headers: { ETag: '"person-7-r4"' } }));
    await write;

    expect(directoryRequests).toHaveLength(2);
    expect(controller.rows[0]).toMatchObject({ id: 7, revision: 4, categories: ['VIP'] });
    expect(controller.cursor).toBe('new-next');
    expect(controller.selectedPersonID).toBe(8);
    expect(controller.profile).toBe(selectedProfile);
    expect(controller.detail).toBe(selectedDetail);
    expect(controller.detail?.person?.id).toBe(8);
    expect(commits).toEqual([{ directoryPersonID: 7 }, { directoryPersonID: 8 }]);
  });

  it('commits reconciliation without restoring selection when selection changes during its request', async () => {
    const commits: Array<Record<string, unknown>> = [];
    const reconciliation = deferredResponse();
    const directoryRequests: Request[] = [];
    const category = {
      person_id: 7, original_value: 'VIP', normalized_value: 'vip',
      envelope: { id: 31, ordinal: 0, source: 'user', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', vcard: {} }
    };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') {
        directoryRequests.push(request);
        return directoryRequests.length === 1
          ? Response.json({ people: [directoryPerson(7), directoryPerson(8)], next_cursor: 'old-next' })
          : reconciliation.promise;
      }
      if (path === '/api/v1/people/7/profile' && request.method === 'PATCH') {
        return new Response(JSON.stringify({
          person: { id: 7, revision: 4 }, names: [], contact_points: [], addresses: [], dates: [], categories: [category], media: []
        }), { headers: { ETag: '"person-7-r4"' } });
      }
      return editableDetailResponse(path, path.startsWith('/api/v1/people/8') ? 8 : 7);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn), (patch) => commits.push(patch));
    controller.category = 'vip';
    await controller.loadFirstPage();
    await controller.loadSelection(7, true);

    const write = controller.profile!.patchProfile({ categories: { add: [{ original_value: 'VIP', envelope: { source: 'user' } }] } });
    await vi.waitFor(() => expect(directoryRequests).toHaveLength(2));
    await controller.loadSelection(8, true);
    const selectedProfile = controller.profile;
    const selectedDetail = controller.detail;

    reconciliation.resolve(Response.json({
      people: [{ ...directoryPerson(7), categories: ['stale'] }, directoryPerson(8)],
      next_cursor: 'new-next'
    }));
    await write;

    expect(controller.rows[0]).toMatchObject({ id: 7, revision: 4, categories: ['VIP'] });
    expect(controller.cursor).toBe('new-next');
    expect(controller.selectedPersonID).toBe(8);
    expect(controller.profile).toBe(selectedProfile);
    expect(controller.detail).toBe(selectedDetail);
    expect(controller.detail?.person?.id).toBe(8);
    expect(commits).toEqual([{ directoryPersonID: 7 }, { directoryPersonID: 8 }]);
  });

  it('lets a newer selected-person mutation supersede an older reconciliation', async () => {
    const firstReconciliation = deferredResponse();
    const secondReconciliation = deferredResponse();
    const directoryRequests: Request[] = [];
    const categoryFor = (personID: number, value: string, id: number) => ({
      person_id: personID, original_value: value, normalized_value: value.toLowerCase(),
      envelope: { id, ordinal: 0, source: 'user', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', vcard: {} }
    });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') {
        directoryRequests.push(request);
        if (directoryRequests.length === 1) return Response.json({ people: [directoryPerson(7), directoryPerson(8)], next_cursor: 'old-next' });
        if (directoryRequests.length === 2) return firstReconciliation.promise;
        return secondReconciliation.promise;
      }
      if (request.method === 'PATCH' && path === '/api/v1/people/7/profile') {
        return new Response(JSON.stringify({
          person: { id: 7, revision: 4 }, names: [], contact_points: [], addresses: [], dates: [],
          categories: [categoryFor(7, 'VIP Seven', 31)], media: []
        }), { headers: { ETag: '"person-7-r4"' } });
      }
      if (request.method === 'PATCH' && path === '/api/v1/people/8/profile') {
        return new Response(JSON.stringify({
          person: { id: 8, revision: 4 }, names: [], contact_points: [], addresses: [], dates: [],
          categories: [categoryFor(8, 'VIP Eight', 32)], media: []
        }), { headers: { ETag: '"person-8-r4"' } });
      }
      return editableDetailResponse(path, path.startsWith('/api/v1/people/8') ? 8 : 7);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn));
    controller.category = 'vip';
    await controller.loadFirstPage();
    await controller.loadSelection(7, true);

    const firstWrite = controller.profile!.patchProfile({ categories: { add: [{ original_value: 'VIP Seven', envelope: { source: 'user' } }] } });
    await vi.waitFor(() => expect(directoryRequests).toHaveLength(2));
    await controller.loadSelection(8, true);
    const selectedProfile = controller.profile;
    const secondWrite = controller.profile!.patchProfile({ categories: { add: [{ original_value: 'VIP Eight', envelope: { source: 'user' } }] } });
    await vi.waitFor(() => expect(directoryRequests).toHaveLength(3));
    expect(directoryRequests[1]!.signal.aborted).toBe(true);

    secondReconciliation.resolve(Response.json({
      people: [{ ...directoryPerson(7), categories: ['server seven'] }, { ...directoryPerson(8), categories: ['stale'] }],
      next_cursor: 'newest-next'
    }));
    await secondWrite;
    firstReconciliation.resolve(Response.json({ people: [directoryPerson(99)], next_cursor: 'stale-next' }));
    await firstWrite;

    expect(controller.rows).toEqual([
      { ...directoryPerson(7), categories: ['server seven'] },
      { ...directoryPerson(8), revision: 4, categories: ['VIP Eight'] }
    ]);
    expect(controller.cursor).toBe('newest-next');
    expect(controller.selectedPersonID).toBe(8);
    expect(controller.profile).toBe(selectedProfile);
  });

  it('makes an abort-ignorant Load more response inert when reconciliation supersedes it', async () => {
    const commits: Array<Record<string, unknown>> = [];
    const oldPage = deferredResponse();
    const reconciledPage = deferredResponse();
    const directoryRequests: Request[] = [];
    let firstPageLoaded = false;
    const category = {
      person_id: 7, original_value: 'VIP', normalized_value: 'vip',
      envelope: { id: 31, ordinal: 0, source: 'user', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', vcard: {} }
    };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') {
        directoryRequests.push(request);
        const cursor = new URL(request.url).searchParams.get('cursor');
        if (!firstPageLoaded) {
          firstPageLoaded = true;
          return Response.json({ people: [directoryPerson(7), directoryPerson(8)], next_cursor: 'old-page-2' });
        }
        if (cursor === 'old-page-2') return oldPage.promise;
        if (cursor === null) return reconciledPage.promise;
        throw new Error(`unexpected directory cursor ${cursor}`);
      }
      if (path === '/api/v1/people/7/profile' && request.method === 'PATCH') {
        return new Response(JSON.stringify({
          person: { id: 7, revision: 4 }, names: [], contact_points: [], addresses: [], dates: [], categories: [category], media: []
        }), { headers: { ETag: '"person-7-r4"' } });
      }
      if (path === '/api/v1/people/7') return new Response(JSON.stringify({ id: 7, revision: 3 }), { headers: { ETag: '"person-7-r3"' } });
      if (path === '/api/v1/people/7/profile') return new Response(JSON.stringify({ person: { id: 7, revision: 3 }, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] }), { headers: { ETag: '"person-7-r3"' } });
      if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
      return detailResponse(path);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn), (patch) => commits.push(patch));
    controller.query = 'synthetic';
    controller.category = 'vip';
    await controller.loadFirstPage();
    await controller.loadSelection(7, true);
    const selectedProfile = controller.profile;

    const loadMore = controller.loadNextPage();
    await vi.waitFor(() => expect(directoryRequests).toHaveLength(2));
    const write = controller.profile!.patchProfile({ categories: { add: [{ original_value: 'VIP', envelope: { source: 'user' } }] } });
    await vi.waitFor(() => expect(directoryRequests).toHaveLength(3));

    expect(directoryRequests[1]!.signal.aborted).toBe(true);
    reconciledPage.resolve(Response.json({
      people: [directoryPerson(9), { ...directoryPerson(7), categories: ['stale'] }],
      next_cursor: 'new-page-2'
    }));
    await write;
    oldPage.resolve(Response.json({ people: [directoryPerson(10)], next_cursor: 'old-page-3' }));
    await loadMore;

    expect(controller.rows.map((row) => row.id)).toEqual([9, 7]);
    expect(controller.rows[1]?.categories).toEqual(['VIP']);
    expect(controller.cursor).toBe('new-page-2');
    expect(controller.loadingMore).toBe(false);
    expect(controller.pageError).toBeNull();
    expect(controller.selectedPersonID).toBe(7);
    expect(controller.profile).toBe(selectedProfile);
    expect(controller.query).toBe('synthetic');
    expect(controller.category).toBe('vip');
    expect(commits).toEqual([{ directoryPersonID: 7 }]);
  });

  it('gates Load more during reconciliation and resumes from the atomically replaced cursor', async () => {
    const commits: Array<Record<string, unknown>> = [];
    const reconciliationPageOne = deferredResponse();
    const reconciliationPageTwo = deferredResponse();
    const oldLoadAttempt = deferredResponse();
    const directoryRequests: Request[] = [];
    let initialPageLoaded = false;
    const category = {
      person_id: 7, original_value: 'VIP', normalized_value: 'vip',
      envelope: { id: 31, ordinal: 0, source: 'user', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', vcard: {} }
    };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') {
        directoryRequests.push(request);
        const cursor = new URL(request.url).searchParams.get('cursor');
        if (!initialPageLoaded) {
          initialPageLoaded = true;
          return Response.json({ people: [directoryPerson(7), directoryPerson(8)], next_cursor: 'old-page-2' });
        }
        if (cursor === 'old-page-2') return Response.json({ people: [directoryPerson(9)], next_cursor: 'old-page-3' });
        if (cursor === null) return reconciliationPageOne.promise;
        if (cursor === 'new-page-2') return reconciliationPageTwo.promise;
        if (cursor === 'old-page-3') return oldLoadAttempt.promise;
        if (cursor === 'new-page-3') return Response.json({ people: [directoryPerson(13)] });
        throw new Error(`unexpected directory cursor ${cursor}`);
      }
      if (path === '/api/v1/people/7/profile' && request.method === 'PATCH') {
        return new Response(JSON.stringify({
          person: { id: 7, revision: 4 }, names: [], contact_points: [], addresses: [], dates: [], categories: [category], media: []
        }), { headers: { ETag: '"person-7-r4"' } });
      }
      if (path === '/api/v1/people/7') return new Response(JSON.stringify({ id: 7, revision: 3 }), { headers: { ETag: '"person-7-r3"' } });
      if (path === '/api/v1/people/7/profile') return new Response(JSON.stringify({ person: { id: 7, revision: 3 }, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] }), { headers: { ETag: '"person-7-r3"' } });
      if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
      return detailResponse(path);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn), (patch) => commits.push(patch));
    controller.category = 'vip';
    await controller.loadFirstPage();
    await controller.loadNextPage();
    await controller.loadSelection(7, true);
    const selectedProfile = controller.profile;

    const write = controller.profile!.patchProfile({ categories: { add: [{ original_value: 'VIP', envelope: { source: 'user' } }] } });
    await vi.waitFor(() => expect(directoryRequests.filter((request) => new URL(request.url).searchParams.get('cursor') === null)).toHaveLength(2));
    const attemptedLoad = controller.loadNextPage();

    reconciliationPageOne.resolve(Response.json({ people: [directoryPerson(11), { ...directoryPerson(7), categories: ['stale'] }], next_cursor: 'new-page-2' }));
    await vi.waitFor(() => expect(directoryRequests.some((request) => new URL(request.url).searchParams.get('cursor') === 'new-page-2')).toBe(true));
    expect(controller.rows.map((row) => row.id)).toEqual([7, 8, 9]);
    expect(controller.cursor).toBe('old-page-3');
    reconciliationPageTwo.resolve(Response.json({ people: [directoryPerson(12)], next_cursor: 'new-page-3' }));
    await write;

    if (directoryRequests.some((request) => new URL(request.url).searchParams.get('cursor') === 'old-page-3')) {
      oldLoadAttempt.resolve(Response.json({ people: [directoryPerson(99)], next_cursor: 'old-page-4' }));
    }
    await attemptedLoad;

    expect(directoryRequests.some((request) => new URL(request.url).searchParams.get('cursor') === 'old-page-3')).toBe(false);
    expect(controller.rows.map((row) => row.id)).toEqual([11, 7, 12]);
    expect(controller.rows[1]?.categories).toEqual(['VIP']);
    expect(controller.cursor).toBe('new-page-3');
    expect(controller.selectedPersonID).toBe(7);
    expect(controller.profile).toBe(selectedProfile);
    expect(controller.category).toBe('vip');
    expect(commits).toEqual([{ directoryPersonID: 7 }]);

    await controller.loadNextPage();

    expect(controller.rows.map((row) => row.id)).toEqual([11, 7, 12, 13]);
    expect(controller.cursor).toBeNull();
  });

  it.each([
    { name: 'page-one HTTP', failurePage: 1, network: false, message: 'Directory reconciliation unavailable.' },
    { name: 'page-N HTTP', failurePage: 2, network: false, message: 'Directory reconciliation unavailable.' },
    { name: 'network rejection', failurePage: 1, network: true, message: 'reconciliation network down' }
  ])('preserves the prior page sequence and offers Reload after a $name failure', async ({ name, failurePage, network, message }) => {
    const directoryRequests: Request[] = [];
    let directoryRead = 0;
    let reconciliationRead = 0; let profileWritten = false;
    const category = {
      person_id: 7, original_value: 'VIP', normalized_value: 'vip',
      envelope: { id: 31, ordinal: 0, source: 'user', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', vcard: {} }
    };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') {
        directoryRequests.push(request);
        directoryRead += 1;
        if (directoryRead === 1) return Response.json({ people: [directoryPerson(7), directoryPerson(8)], next_cursor: 'old-page-2' });
        if (directoryRead === 2) return Response.json({ people: [directoryPerson(9)], next_cursor: 'old-page-3' });
        reconciliationRead += 1;
        if (name === 'page-one HTTP' && reconciliationRead > 1) return Response.json({ people: [directoryPerson(7)] });
        if (failurePage === 2 && reconciliationRead === 1) {
          return Response.json({ people: [directoryPerson(11)], next_cursor: 'new-page-2' });
        }
        if (network) throw new TypeError(message);
        return Response.json({ error: 'unavailable', message }, { status: 503 });
      }
      if (path === '/api/v1/people/7/profile' && request.method === 'PATCH') {
        profileWritten = true;
        return new Response(JSON.stringify({
          person: { id: 7, revision: 4 }, names: [], contact_points: [], addresses: [], dates: [], categories: [category], media: []
        }), { headers: { ETag: '"person-7-r4"' } });
      }
      if (path === '/api/v1/people/7') return new Response(JSON.stringify({ id: 7, revision: profileWritten ? 4 : 3 }), { headers: { ETag: `"person-7-r${profileWritten ? 4 : 3}"` } });
      if (path === '/api/v1/people/7/profile') return new Response(JSON.stringify({ person: { id: 7, revision: profileWritten ? 4 : 3 }, names: [], contact_points: [], addresses: [], dates: [], categories: profileWritten ? [category] : [], media: [] }), { headers: { ETag: `"person-7-r${profileWritten ? 4 : 3}"` } });
      if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
      return detailResponse(path);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn));
    controller.query = 'synthetic';
    controller.contactState = 'active';
    controller.category = 'vip';
    controller.organization = 'Example Org';
    controller.primaryChannel = 'email';
    await controller.loadFirstPage();
    await controller.loadNextPage();
    await controller.loadSelection(7, true);
    const selectedProfile = controller.profile;

    await expect(controller.profile!.patchProfile({ categories: { add: [{ original_value: 'VIP', envelope: { source: 'user' } }] } })).resolves.toBeUndefined();

    expect(controller.rows).toEqual([directoryPerson(7), directoryPerson(8), directoryPerson(9)]);
    expect(controller.cursor).toBe('old-page-3');
    expect(controller.cursor !== null).toBe(true);
    expect(controller.loadingMore).toBe(false);
    expect(controller.pageError).toBe(message);
    expect(controller.pageRecovery).toBe('reload');
    expect(controller.selectedPersonID).toBe(7);
    expect(controller.profile).toBe(selectedProfile);
    expect(controller.profile?.structuredProfile?.categories).toEqual([category]);
    expect(controller.profile?.conflict).toBeNull();
    expect(reconciliationRead).toBe(failurePage);
    expect(controller.query).toBe('synthetic');
    expect(controller.contactState).toBe('active');
    expect(controller.category).toBe('vip');
    expect(controller.organization).toBe('Example Org');
    expect(controller.primaryChannel).toBe('email');

    const readsBeforeBlockedLoad = directoryRead;
    await controller.loadNextPage();
    expect(directoryRead).toBe(readsBeforeBlockedLoad);
    expect(controller.rows).toEqual([directoryPerson(7), directoryPerson(8), directoryPerson(9)]);
    expect(controller.cursor).toBe('old-page-3');
    expect(controller.pageError).toBe(message);
    expect(controller.pageRecovery).toBe('reload');

    if (name === 'page-one HTTP') {
      await controller.reloadFirstPage();
      const parameters = new URL(directoryRequests.at(-1)!.url).searchParams;
      expect(Object.fromEntries(['q', 'contact_state', 'category', 'organization', 'primary_channel'].map(key => [key, parameters.get(key)]))).toEqual({ q: 'synthetic', contact_state: 'active', category: 'vip', organization: 'Example Org', primary_channel: 'email' });
      expect(controller.pageError).toBeNull();
    }
  });

  it('lets a new filter load supersede an abort-ignorant reconciliation failure', async () => {
    const reconciliation = deferredResponse();
    const directoryRequests: Request[] = [];
    let firstPageLoaded = false;
    const category = {
      person_id: 7, original_value: 'VIP', normalized_value: 'vip',
      envelope: { id: 31, ordinal: 0, source: 'user', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', vcard: {} }
    };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') {
        directoryRequests.push(request);
        const query = new URL(request.url).searchParams.get('q');
        if (!firstPageLoaded) {
          firstPageLoaded = true;
          return Response.json({ people: [directoryPerson(7)], next_cursor: 'old-next' });
        }
        if (query === 'replacement') return Response.json({ people: [directoryPerson(20)], next_cursor: 'replacement-next' });
        return reconciliation.promise;
      }
      if (path === '/api/v1/people/7/profile' && request.method === 'PATCH') {
        return new Response(JSON.stringify({
          person: { id: 7, revision: 4 }, names: [], contact_points: [], addresses: [], dates: [], categories: [category], media: []
        }), { headers: { ETag: '"person-7-r4"' } });
      }
      if (path === '/api/v1/people/7') return new Response(JSON.stringify({ id: 7, revision: 3 }), { headers: { ETag: '"person-7-r3"' } });
      if (path === '/api/v1/people/7/profile') return new Response(JSON.stringify({ person: { id: 7, revision: 3 }, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] }), { headers: { ETag: '"person-7-r3"' } });
      if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
      return detailResponse(path);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn));
    controller.category = 'vip';
    await controller.loadFirstPage();
    await controller.loadSelection(7, true);

    const write = controller.profile!.patchProfile({ categories: { add: [{ original_value: 'VIP', envelope: { source: 'user' } }] } });
    await vi.waitFor(() => expect(directoryRequests).toHaveLength(2));
    controller.setFilters({ directoryQuery: 'replacement' });
    await vi.waitFor(() => expect(controller.rows.map((row) => row.id)).toEqual([20]));
    expect(directoryRequests[1]!.signal.aborted).toBe(true);

    reconciliation.reject(new TypeError('stale reconciliation failed'));
    await write;

    expect(controller.rows.map((row) => row.id)).toEqual([20]);
    expect(controller.cursor).toBe('replacement-next');
    expect(controller.pageError).toBeNull();
    expect(controller.pageRecovery).toBeNull();
    expect(controller.selectedPersonID).toBe(7);
    expect(controller.query).toBe('replacement');
    expect(controller.category).toBe('vip');
  });

  it('loads selected-person attributes with history', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/people/7') return Response.json({ id: 7, revision: 3 });
      if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [{ definition: { slug: 'alias' }, current: [], history: [{ id: 19 }] }] });
      return detailResponse(path);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn));

    await controller.loadSelection(7, true);

    const attributesRequest = requests.find((request) => pathOf(request).endsWith('/attributes'))!;
    expect(new URL(attributesRequest.url).searchParams.get('history')).toBe('true');
    expect(controller.detail?.attributes?.attributes?.[0]?.history).toEqual([{ id: 19 }]);
  });

  it('restarts page one without a cursor for same-filter history restoration', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') {
        const cursor = new URL(request.url).searchParams.get('cursor');
        if (cursor === 'page-2') return Response.json({ people: [directoryPerson(2)], next_cursor: 'page-3' });
        return Response.json({ people: [directoryPerson(1)], next_cursor: 'page-2' });
      }
      return detailResponse(path);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn));
    const state = {
      directoryQuery: 'alice', directoryContactState: '', directoryCategory: '',
      directoryOrganization: '', directoryPrimaryChannel: '', directoryLastContactAfter: '', directoryLastContactBefore: '', directorySort: 'name', directoryPersonID: null
    } as const;
    controller.applyURLState(state);
    await vi.waitFor(() => expect(controller.rows).toEqual([directoryPerson(1)]));
    await controller.loadNextPage();
    expect(controller.cursor).toBe('page-3');

    controller.applyURLState({ ...state, directoryPersonID: 7 }, true);
    await vi.waitFor(() => expect(controller.rows).toEqual([directoryPerson(1)]));

    const directoryRequests = requests.filter((request) => pathOf(request) === '/api/v1/people/directory');
    expect(directoryRequests).toHaveLength(3);
    expect(new URL(directoryRequests[2]!.url).searchParams.get('cursor')).toBeNull();
  });

  it.each([200, 201])('treats %i promotion as successful, refreshes page one, and notifies the write owner', async (status) => {
    const commits: Array<Record<string, unknown>> = [];
    const onWrite = vi.fn();
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (pathOf(request) === '/api/v1/people') return Response.json({ id: 42, revision: 1 }, { status });
      if (pathOf(request) === '/api/v1/people/directory') return Response.json({ people: [directoryPerson(42)] });
      if (pathOf(request) === '/api/v1/people/42') return detailResponse(pathOf(request), 42);
      return detailResponse(pathOf(request), 42);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn), (patch) => commits.push(patch), onWrite);

    await expect(controller.promote(11)).resolves.toEqual({ ok: true, personID: 42 });
    expect(controller.promotionResult).toEqual({ ok: true, personID: 42 });
    expect(commits).toContainEqual({ directoryPersonID: 42 });
    await expect(requests[0]!.clone().json()).resolves.toEqual({ participant_id: 11 });
    expect(controller.rows).toEqual([directoryPerson(42)]);
    expect(onWrite).toHaveBeenCalledOnce();
  });

  it('exposes person binding conflicts without selecting a person', async () => {
    const controller = new DirectoryController(createAPIClient(vi.fn<typeof fetch>(async () =>
      Response.json({ error: 'person_binding_conflict', message: 'bound elsewhere' }, { status: 409 })
    )));

    await expect(controller.promote(11)).resolves.toEqual({ ok: false, code: 'person_binding_conflict', message: 'bound elsewhere' });
    expect(controller.promotionResult).toEqual({ ok: false, code: 'person_binding_conflict', message: 'bound elsewhere' });
    expect(controller.selectedPersonID).toBeNull();
  });

  it.each(['saved person', 'new candidate'])('clears failed promotion feedback when opening a %s', async destination => {
    const client = createAPIClient(vi.fn<typeof fetch>(async input => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'POST' && pathOf(request) === '/api/v1/people') return Response.json({ message: 'Save unavailable' }, { status: 503 });
      return detailResponse(pathOf(request));
    }));
    const controller = new DirectoryController(client);
    try {
      await expect(controller.promote(11)).resolves.toMatchObject({ ok: false });
      expect(controller.promotionError).toBe('Save unavailable');
      expect(controller.promotionResult).toMatchObject({ ok: false });
      if (destination === 'saved person') await controller.loadSelection(7, true); else controller.resetForPromotion();
      expect(controller.selectedPersonID).toBe(destination === 'saved person' ? 7 : null);
      expect(controller.promotionError).toBeNull();
      expect(controller.promotionResult).toBeNull();
    } finally { controller.destroy(); }
  });

  it.each(['selected', 'unselected'])('reconciles a split with its source profile %s', async selection => {
    const selected = selection === 'selected';
    const requests: Request[] = [];
    const controller = new DirectoryController(createAPIClient(async input => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/attribute-definitions') return Response.json({ definitions: [] });
      if (path === '/api/v1/people/directory') return Response.json({ people: selected ? [directoryPerson(7)] : requests.length === 1 ? [directoryPerson(12)] : [directoryPerson(12), directoryPerson(19)] });
      return detailResponse(path);
    }));
    try {
      if (selected) {
        await controller.loadSelection(7);
        await controller.entity!.loadNetwork(1);
        requests.length = 0;
      } else await controller.loadFirstPage();
      await controller.reconcilePersonSplit({ sourcePersonID: selected ? 7 : 12, newPersonID: selected ? 9 : 19 });
      if (selected) {
        expect(requests.filter(request => request.method === 'GET').map(pathOf)).toEqual(expect.arrayContaining(['/api/v1/people/7/profile', '/api/v1/people/7/employments', '/api/v1/people/7/network']));
      } else {
        expect(controller.rows.map(row => row.id)).toEqual([12, 19]);
        expect(requests.map(pathOf)).toEqual(['/api/v1/people/directory', '/api/v1/people/directory']);
        expect(controller.detail).toBeNull();
      }
    } finally { controller.destroy(); }
  });

  it.each(['person', 'profile', 'attributes', 'definitions', 'contact-state', 'employments', 'relationships', 'relationship-types', 'network'])('reports a failed %s refresh after a split and allows retry', async resource => {
    let fail = false;
    const path = resource === 'person' ? '/api/v1/people/7' : resource === 'definitions' ? '/api/v1/attribute-definitions' : resource === 'relationship-types' ? '/api/v1/relationship-types' : `/api/v1/people/7/${resource}`;
    const controller = new DirectoryController(createAPIClient(async input => {
      const requestPath = pathOf(input instanceof Request ? input : new Request(input));
      if (requestPath === '/api/v1/people/directory') return Response.json({ people: [directoryPerson(7)] });
      if (fail && requestPath === path) return Response.json({ message: 'Synthetic refresh failed' }, { status: 503 });
      if (requestPath === '/api/v1/attribute-definitions') return Response.json({ definitions: [] });
      return editableDetailResponse(requestPath, 7);
    }));
    try {
      await controller.loadSelection(7);
      await controller.entity!.loadNetwork(1);
      fail = true;
      await expect(controller.reconcilePersonSplit({ sourcePersonID: 7, newPersonID: 9 })).rejects.toThrow();
      fail = false;
      await expect(controller.reconcilePersonSplit({ sourcePersonID: 7, newPersonID: 9 })).resolves.toBeUndefined();
    } finally { controller.destroy(); }
  });

  it.each([
    { destination: 'relationship candidate', status: 201 },
    { destination: 'saved selection', status: 201 },
    { destination: 'relationship candidate', status: 409 }
  ])('ignores a late promotion $status after opening a new $destination', async ({ destination, status }) => {
    const commits: Array<Record<string, unknown>> = [];
    let resolvePromotion: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (pathOf(request) === '/api/v1/people' && request.method === 'POST') {
        return new Promise<Response>((resolve) => { resolvePromotion = resolve; });
      }
      if (pathOf(request) === '/api/v1/people/directory') return Response.json({ people: [directoryPerson(42)] });
      return detailResponse(pathOf(request), pathOf(request).startsWith('/api/v1/people/7') ? 7 : 42);
    });
    const controller = new DirectoryController(createAPIClient(fetchFn), (patch) => commits.push(patch));

    const promotion = controller.promote(11);
    await vi.waitFor(() => expect(resolvePromotion).toBeDefined());
    if (destination === 'saved selection') await controller.loadSelection(7); else controller.resetForPromotion();
    resolvePromotion?.(Response.json(status === 409 ? { error: 'person_binding_conflict', message: 'bound elsewhere' } : { id: 42, revision: 1 }, { status }));
    await promotion;

    expect(controller.selectedPersonID).toBe(destination === 'saved selection' ? 7 : null);
    expect(controller.promotionResult).toBeNull();
    expect(controller.promotionError).toBeNull();
    expect(commits).not.toContainEqual({ directoryPersonID: 42 });
  });

  it.each([false, true])('removes a deleted person and preserves a later selection (reselect=%s)', async reselect => {
    const pending = deferredResponse();
    const commit = vi.fn();
    const onWrite = vi.fn();
    const client = createAPIClient(async input => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') return Response.json({ people: reselect ? [directoryPerson(7), directoryPerson(9)] : [directoryPerson(7)] });
      if (path === '/api/v1/people/7' && request.method === 'DELETE') return pending.promise;
      return editableDetailResponse(path, path.includes('/people/9') ? 9 : 7);
    });
    const controller = new DirectoryController(client, commit, onWrite);
    try {
      await controller.loadFirstPage();
      await controller.loadSelection(7, true);
      const deletion = controller.profile!.deletePerson();
      if (reselect) await controller.loadSelection(9, true);
      const selected = reselect ? controller.profile : null;
      commit.mockClear();
      pending.resolve(new Response(null, { status: 204 }));
      await deletion;
      expect(controller.selectedPersonID).toBe(reselect ? 9 : null);
      expect(controller.profile).toBe(selected);
      expect(controller.detail?.person?.id ?? null).toBe(reselect ? 9 : null);
      expect(controller.rows).toEqual(reselect ? [directoryPerson(9)] : []);
      if (reselect) expect(commit).not.toHaveBeenCalled();
      else expect(commit).toHaveBeenCalledWith({ directoryPersonID: null }, 'push');
      expect(onWrite).toHaveBeenCalledOnce();
    } finally { pending.resolve(new Response(null, { status: 204 })); controller.destroy(); }
  });
});
