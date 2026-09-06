import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { appShortcuts } from '@kenn-io/kit-ui';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { DirectoryEntityController } from '../../directory/entity-controller.svelte';
import { chooseSelectOption } from '../../../test/kit-ui';
import OrganizationEmploymentTab from './OrganizationEmploymentTab.svelte';

function requestOf(input: RequestInfo | URL): Request {
  return input instanceof Request ? input : new Request(input);
}

function pathOf(request: Request): string {
  return new URL(request.url, document.baseURI).pathname;
}

function organization(id = 21, revision = 1, name = 'Synthetic Org') {
  return {
    id, revision, name, kind: 'company', primary_domain: 'synthetic.example', description: 'A synthetic organization.',
    created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z'
  };
}

function organizationProfile(revision = 1, name = 'Synthetic Org') {
  return {
    organization: organization(21, revision, name),
    names: [{ organization_id: 21, name_kind: 'alias', name: 'Synthetic Works', name_normalized: 'synthetic works', envelope: envelope(31) }],
    contact_points: [], addresses: [], categories: [], identifiers: [],
    media: [{ organization_id: 21, media_kind: 'logo', original_value: 'Existing logo', content_hash: 'sha256:synthetic', has_data: true, envelope: envelope(32) }]
  };
}

function envelope(id: number, overrides: Record<string, unknown> = {}) {
  return { id, ordinal: 0, source: 'user', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', vcard: {}, ...overrides };
}

function richOrganizationProfile(revision = 3) {
  const importedEnvelope = envelope(40, {
    ordinal: 7, pref: 2, type_label: 'work', type_tokens: ['WORK', 'INTERNET'], source: 'vcard_import',
    source_ref: 'card-42', source_resource_uid: 'resource-card-42', confidence: 0.87, active_from: '2025-02-03T04:05:06Z',
    vcard: { property: 'EMAIL', group: 'item2', prop_id: 'prop-9', pid: ['1.1', '2.1'], altid: 'alt-4' }
  });
  return {
    organization: organization(21, revision),
    names: [
      { organization_id: 21, name_kind: 'legal', name: 'Synthetic Legal Ltd', name_normalized: 'synthetic legal ltd', envelope: { ...importedEnvelope, id: 41, vcard: { ...importedEnvelope.vcard, property: 'FN' } } },
      { organization_id: 21, name_kind: 'alias', name: 'Synthetic Works', name_normalized: 'synthetic works', envelope: envelope(42) }
    ],
    categories: [{ organization_id: 21, category: 'Research', category_normalized: 'research', envelope: { ...importedEnvelope, id: 43, vcard: { ...importedEnvelope.vcard, property: 'CATEGORIES' } } }],
    contact_points: [{ organization_id: 21, address_kind: 'email', original_value: 'concurrent@example.test', normalized_value: 'concurrent@example.test', normalization: 'email', normalization_version: 1, service_slug: 'email', scope_kind: 'department', scope_value: 'research', uri: 'mailto:concurrent@example.test', envelope: importedEnvelope }],
    addresses: [{ organization_id: 21, address_kind: 'work', original_value: 'Suite 5, 12 Example Road', post_office_box: 'PO 12', extended_address: 'Suite 5', street_address: '12 Example Road', locality: 'Exampleton', region: 'EX', postal_code: '12345', country_name: 'Exampleland', extended_components: 'Lab 3', free_text: 'Rear entrance', place_uri: 'geo:1,2', geo_uri: 'geo:1,2', label: 'HQ', timezone: 'Etc/UTC', country_code: 'EX', envelope: { ...importedEnvelope, id: 44, vcard: { ...importedEnvelope.vcard, property: 'ADR' } } }],
    identifiers: [{ organization_id: 21, identifier_kind: 'registry', identifier_value: 'REG-42', normalized_value: 'reg-42', envelope: { ...importedEnvelope, id: 45, vcard: { ...importedEnvelope.vcard, property: 'X-REGISTRY' } } }],
    media: [{ organization_id: 21, media_kind: 'logo', media_type: 'image/png', uri: 'https://example.test/logo.png', original_value: 'Concurrent logo', content_hash: 'sha256:concurrent', byte_size: 1200, has_data: true, envelope: { ...importedEnvelope, id: 46, vcard: { ...importedEnvelope.vcard, property: 'LOGO' } } }]
  };
}

function employment(id = 11, revision = 1, overrides: Record<string, unknown> = {}) {
  return {
    id, revision, person_id: 7, organization_id: 21, source: 'user', is_current: true, is_primary: false,
    title: 'Engineer', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', ...overrides
  };
}

function collection(rows = [employment()], projection = { employment_id: 11, organization_id: 21, organization_name: 'Synthetic Org', title: 'Engineer', vcard: {} }) {
  return Response.json({ employments: rows, projection });
}

function controllerWith(fetchFn: typeof fetch): DirectoryEntityController {
  return new DirectoryEntityController(createAPIClient(fetchFn), 7);
}

describe('OrganizationEmploymentTab', () => {
  it('keeps local employment history actionable beside a reconciliation warning and recovers', async () => {
    let reads = 0;
    let resolveRefresh!: (response: Response) => void;
    const pendingRefresh = new Promise<Response>((resolve) => { resolveRefresh = resolve; });
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
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

    expect(screen.getByRole('alert').textContent).toContain('reconciliation failed');
    expect(screen.getByText('Locally updated role')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Edit employment' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'End employment' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Delete employment' })).toBeDefined();
    const refresh = screen.getByRole('button', { name: 'Refresh employment records' });
    await fireEvent.click(refresh);

    await waitFor(() => expect(reads).toBe(1));
    expect(screen.getByRole('status').textContent).toContain('Loading employment records');
    expect(screen.getByText('Locally updated role')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Edit employment' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'End employment' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Delete employment' })).toBeDefined();

    resolveRefresh(collection([employment(12, 2, { title: 'Recovered role' })], undefined));
    expect(screen.queryByRole('alert')).toBeNull();
    expect(await screen.findByText('Recovered role')).toBeDefined();
  });

  it('renders loading, failure, empty state, and bounded organization search results', async () => {
    let resolveSearch!: (response: Response) => void;
    const search = new Promise<Response>((resolve) => { resolveSearch = resolve; });
    let searches = 0;
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (pathOf(request) === '/api/v1/organizations') {
        searches += 1;
        return searches === 1 ? search : Response.json({ organizations: [organization()], total: 1, limit: 50, offset: 0 });
      }
      throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
    }));
    controller.employments = [];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    expect(screen.getByText('No employment records.')).toBeDefined();
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search organizations' }), { target: { value: 'synthetic' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Search organizations' }));
    expect(screen.getByRole('status').textContent).toContain('Searching organizations');
    resolveSearch(Response.json({ error: 'unavailable', message: 'Organization search unavailable.' }, { status: 503 }));
    expect((await screen.findByRole('alert')).textContent).toContain('Organization search unavailable.');

    await fireEvent.click(screen.getByRole('button', { name: 'Search organizations' }));
    expect(await screen.findByRole('button', { name: 'Manage Synthetic Org' })).toBeDefined();
  });

  it('creates an employment for the exact selected person with user provenance and renders reconciled projection', async () => {
    const requests: Request[] = [];
    let employmentReads = 0;
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/organizations') return Response.json({ organizations: [organization()], total: 1, limit: 50, offset: 0 });
      if (path === '/api/v1/employments' && request.method === 'POST') {
        return new Response(JSON.stringify(employment(12, 1, { title: 'Architect', is_primary: true })), { status: 201, headers: { ETag: '"employment-12-r1"' } });
      }
      if (path === '/api/v1/people/7/employments') {
        employmentReads += 1;
        return collection([employment(11, 2, { is_primary: false }), employment(12, 1, { title: 'Architect', is_primary: true })], {
          employment_id: 12, organization_id: 21, organization_name: 'Synthetic Org', title: 'Architect', vcard: {}
        });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.employments = [employment(11, 1, { is_primary: true })];
    controller.employmentProjection = { employment_id: 11, organization_id: 21, organization_name: 'Old Org', title: 'Engineer', vcard: {} };
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Add employment' }));
    await chooseSelectOption(await screen.findByRole('combobox', { name: /^Organization:/ }), 'Synthetic Org');
    await fireEvent.input(screen.getByRole('textbox', { name: 'Employment title' }), { target: { value: 'Architect' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create employment' }));

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Add employment' })).toBeNull());
    const post = requests.find((request) => pathOf(request) === '/api/v1/employments' && request.method === 'POST')!;
    const postedBody = await post.json();
    expect(postedBody).toMatchObject({ person_id: 7, organization_id: 21, source: 'user', title: 'Architect' });
    expect(postedBody.source_ref).toBeUndefined();
    expect(employmentReads).toBe(1);
    expect(screen.getByText(/Primary organization: Synthetic Org/)).toBeDefined();
    expect(screen.getAllByText(/Architect/)).toHaveLength(2);
    expect(screen.getByText(/Engineer/).closest('li')?.textContent).not.toContain('Primary');
  });

  it('uses a fresh employment ETag, retains a conflict draft, and retries only after another explicit save', async () => {
    const requests: Request[] = [];
    let reads = 0;
    let patches = 0;
    const hiddenByRead = [
      { person_id: 7, organization_id: 21, source: 'user', source_ref: 'modal-old', address_id: 11, confidence: 0.11 },
      { person_id: 8, organization_id: 22, source: 'vcard_import', source_ref: 'first-fresh', address_id: 22, confidence: 0.22 },
      { person_id: 9, organization_id: 23, source: 'extraction', source_ref: 'conflict-current', address_id: 33, confidence: 0.33 },
      { person_id: 10, organization_id: 24, source: 'enrichment', source_ref: 'retry-fresh', address_id: 44, confidence: 0.44 }
    ];
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
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
      if (path === '/api/v1/organizations/23' && request.method === 'GET') {
        return new Response(JSON.stringify({ ...organizationProfile(6, 'Concurrent Employment Org'), organization: organization(23, 6, 'Concurrent Employment Org') }), { headers: { ETag: '"organization-23-r6"' } });
      }
      if (path === '/api/v1/employments/11' && request.method === 'PATCH') {
        patches += 1;
        if (patches === 1) return Response.json({ error: 'revision_conflict', message: 'stale revision' }, { status: 412 });
        return new Response(JSON.stringify(employment(11, 6, { ...hiddenByRead[3], title: 'Draft title' })), { headers: { ETag: '"employment-11-r6"' } });
      }
      if (path === '/api/v1/people/7/employments') return collection([employment(11, 6, { ...hiddenByRead[3], title: 'Draft title' })], {
        employment_id: 11, organization_id: 24, organization_name: 'Retry Employment Org', title: 'Draft title', vcard: {}
      });
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.employments = [employment()];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Edit employment' }));
    const title = await screen.findByRole('textbox', { name: 'Employment title' }) as HTMLInputElement;
    await fireEvent.input(title, { target: { value: 'Draft title' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save employment' }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('changed elsewhere');
    expect(alert.textContent).toContain('Changed title');
    expect(alert.textContent).toContain('Concurrent research');
    expect(alert.textContent).toContain('Concurrent campus');
    expect(alert.textContent).toContain('Concurrent Employment Org (23)');
    expect(alert.textContent).not.toContain('Exact Employment Org (23)');
    expect(alert.textContent).toContain('Address ID 33');
    expect(alert.textContent).toContain('0.33');
    expect(title.value).toBe('Draft title');
    expect(patches).toBe(1);
    const firstPatch = requests.find((request) => request.method === 'PATCH')!;
    expect(firstPatch.headers.get('If-Match')).toBe('"employment-11-r3"');
    expect(await firstPatch.clone().json()).toMatchObject({ ...hiddenByRead[1], title: 'Draft title' });

    await waitFor(() => expect(screen.getByRole('button', { name: 'Save employment' })).toHaveProperty('disabled', false));
    await fireEvent.click(screen.getByRole('button', { name: 'Save employment' }));
    await waitFor(() => expect(patches).toBe(2));
    const retryPatch = requests.filter((request) => request.method === 'PATCH')[1]!;
    expect(retryPatch.headers.get('If-Match')).toBe('"employment-11-r5"');
    expect(await retryPatch.clone().json()).toMatchObject({ ...hiddenByRead[3], title: 'Draft title' });
  });

  it('loads the exact organization label for a disabled existing-employment selector', async () => {
    const requests: Request[] = [];
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
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

    await fireEvent.click(screen.getByRole('button', { name: 'Edit employment' }));
    const selector = await screen.findByRole('combobox', { name: /^Organization:/ });
    expect(selector.textContent).toContain('Exact Employment Org');
    expect(selector.textContent).not.toContain('Choose an organization');
    await fireEvent.click(screen.getByRole('button', { name: 'Save employment' }));
    await waitFor(() => expect(requests.some((request) => request.method === 'PATCH')).toBe(true));
    expect(await requests.find((request) => request.method === 'PATCH')!.json()).toMatchObject({ organization_id: 21 });
  });

  it('offers Make primary only for current non-primary employments', () => {
    const controller = controllerWith(vi.fn<typeof fetch>());
    controller.employments = [
      employment(11, 1, { is_current: false, is_primary: false, title: 'Historical role' }),
      employment(12, 1, { is_current: true, is_primary: false, title: 'Current role' })
    ];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    const historical = screen.getByText('Historical role').closest('li')!;
    const current = screen.getByText('Current role').closest('li')!;
    expect(within(historical).queryByRole('button', { name: 'Make primary employment' })).toBeNull();
    expect(within(current).getByRole('button', { name: 'Make primary employment' })).toBeDefined();
  });

  it('makes a current employment primary and reconciles the projection', async () => {
    const requests: Request[] = [];
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') return new Response(JSON.stringify(employment()), { headers: { ETag: '"employment-11-r1"' } });
      if (path === '/api/v1/employments/11/primary') return new Response(JSON.stringify(employment(11, 2, { is_primary: true })), { headers: { ETag: '"employment-11-r2"' } });
      if (path === '/api/v1/people/7/employments') return collection([employment(11, 2, { is_primary: true })]);
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.employments = [employment()];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Make primary employment' }));
    await waitFor(() => expect(screen.getByText('Primary', { selector: 'small' })).toBeDefined());
    expect(requests.find((request) => pathOf(request) === '/api/v1/employments/11/primary')?.headers.get('If-Match')).toBe('"employment-11-r1"');
  });

  it('retains a make-primary conflict and renders the complete refreshed employment', async () => {
    let reads = 0;
    let primaryWrites = 0;
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') {
        reads += 1;
        return new Response(JSON.stringify(employment(11, reads + 1, reads === 1 ? {} : {
          organization_id: 22,
          role: 'Concurrent role',
          department: 'Concurrent primary department',
          address_id: 66,
          confidence: 0.66,
          source: 'archive_observation',
          source_ref: 'concurrent-primary'
        })), { headers: { ETag: `"employment-11-r${reads + 1}"` } });
      }
      if (path === '/api/v1/employments/11/primary') {
        primaryWrites += 1;
        return Response.json({ error: 'revision_conflict', message: 'primary changed elsewhere' }, { status: 412 });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.organizations = [organization()];
    controller.employments = [employment()];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Make primary employment' }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('primary changed elsewhere');
    expect(alert.textContent).toContain('Organization 22');
    expect(alert.textContent).not.toContain('Synthetic Org (22)');
    expect(alert.textContent).toContain('Concurrent role');
    expect(alert.textContent).toContain('Concurrent primary department');
    expect(alert.textContent).toContain('Address ID 66');
    expect(alert.textContent).toContain('0.66');
    expect(alert.textContent).toContain('archive_observation');
    expect(alert.textContent).toContain('concurrent-primary');
    expect(primaryWrites).toBe(1);
  });

  it('supports end and delete with reconciled sibling state without offering primary for history', async () => {
    const requests: Request[] = [];
    let revision = 1;
    let deleted = false;
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
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
    controller.employments = [employment()];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'End employment' }));
    const endDate = await screen.findByRole('textbox', { name: 'Employment end date' });
    await waitFor(() => expect(endDate).toHaveProperty('disabled', false));
    await fireEvent.input(endDate, { target: { value: '2026-08' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm end employment' }));
    await waitFor(() => expect(requests.some((request) => pathOf(request) === '/api/v1/employments/11/end')).toBe(true));
    expect((await requests.find((request) => pathOf(request) === '/api/v1/employments/11/end')!.json())).toEqual({ end_date: '2026-08' });
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'End employment' })).toBeNull());

    expect(screen.queryByRole('button', { name: 'Make primary employment' })).toBeNull();

    await fireEvent.click(screen.getByRole('button', { name: 'Delete employment' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Confirm delete employment' }));
    await waitFor(() => expect(screen.getByText('No employment records.')).toBeDefined());
    expect(requests.find((request) => request.method === 'DELETE')?.headers.get('If-Match')).toBe('"employment-11-r2"');
  });

  it('blocks an unknown employment create until explicit refresh and keeps the modal draft open', async () => {
    let posts = 0;
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
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

    await fireEvent.click(screen.getByRole('button', { name: 'Add employment' }));
    await chooseSelectOption(await screen.findByRole('combobox', { name: /^Organization:/ }), 'Synthetic Org');
    await fireEvent.input(screen.getByRole('textbox', { name: 'Employment title' }), { target: { value: 'Draft role' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create employment' }));
    expect((await screen.findByRole('alert')).textContent).toContain('outcome is unknown');
    expect((screen.getByRole('textbox', { name: 'Employment title' }) as HTMLInputElement).value).toBe('Draft role');
    expect(screen.getByRole('button', { name: 'Create employment' })).toHaveProperty('disabled', true);
    expect(posts).toBe(1);

    await fireEvent.click(screen.getByRole('button', { name: 'Refresh employment records' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Create employment' })).toHaveProperty('disabled', false));
    expect(posts).toBe(1);
  });

  it('blocks an unknown organization create until its own explicit refresh and retains the draft', async () => {
    let posts = 0;
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/organizations' && request.method === 'POST') {
        posts += 1;
        return Response.json({ error: 'unavailable', message: 'response unavailable' }, { status: 503 });
      }
      if (path === '/api/v1/organizations') return Response.json({ organizations: [], total: 0, limit: 50, offset: 0 });
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'New organization' }));
    const name = screen.getByRole('textbox', { name: 'Organization name' }) as HTMLInputElement;
    await fireEvent.input(name, { target: { value: 'Unknown Result Org' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create organization' }));

    expect((await screen.findByRole('alert')).textContent).toContain('outcome is unknown');
    expect(name.value).toBe('Unknown Result Org');
    expect(screen.getByRole('button', { name: 'Create organization' })).toHaveProperty('disabled', true);
    expect(posts).toBe(1);
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh organizations' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Create organization' })).toHaveProperty('disabled', false));
    expect(posts).toBe(1);
  });

  it('creates, fresh-ETag edits, replaces a profile without losing media hashes, and deletes an unused organization', async () => {
    const requests: Request[] = [];
    let readRevision = 2;
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/organizations' && request.method === 'GET') return Response.json({ organizations: [organization()], total: 1, limit: 50, offset: 0 });
      if (path === '/api/v1/organizations' && request.method === 'POST') return new Response(JSON.stringify(organization(22, 1, 'New Synthetic Org')), { status: 201, headers: { ETag: '"organization-22-r1"' } });
      if (path === '/api/v1/organizations/21' && request.method === 'GET') return new Response(JSON.stringify(organizationProfile(readRevision)), { headers: { ETag: `"organization-21-r${readRevision}"` } });
      if (path === '/api/v1/organizations/21' && request.method === 'PATCH') return new Response(JSON.stringify(organization(21, 3, 'Renamed Synthetic Org')), { headers: { ETag: '"organization-21-r3"' } });
      if (path === '/api/v1/organizations/21/profile' && request.method === 'PUT') return new Response(JSON.stringify(organizationProfile(4)), { headers: { ETag: '"organization-21-r4"' } });
      if (path === '/api/v1/organizations/21' && request.method === 'DELETE') return new Response(null, { status: 204 });
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.organizations = [organization()];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'New organization' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization name' }), { target: { value: 'New Synthetic Org' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create organization' }));
    expect(await requests.find((request) => request.method === 'POST')!.json()).toMatchObject({ name: 'New Synthetic Org' });
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Create organization' })).toBeNull());

    await fireEvent.click(screen.getByRole('button', { name: 'Manage Synthetic Org' }));
    const dialog = await screen.findByRole('dialog', { name: 'Edit Synthetic Org' });
    const organizationName = await within(dialog).findByRole('textbox', { name: 'Organization name' });
    await fireEvent.input(organizationName, { target: { value: 'Renamed Synthetic Org' } });
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Save organization' }));
    await waitFor(() => expect(requests.some((request) => request.method === 'PATCH')).toBe(true));
    expect(requests.find((request) => request.method === 'PATCH')?.headers.get('If-Match')).toBe('"organization-21-r2"');
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Edit Synthetic Org' })).toBeNull());

    await fireEvent.click(screen.getByRole('button', { name: 'Manage Renamed Synthetic Org' }));
    readRevision = 3;
    await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization aliases' }), { target: { value: 'Synthetic Works, Example Works' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save organization profile' }));
    await waitFor(() => expect(requests.some((request) => request.method === 'PUT')).toBe(true));
    const put = requests.find((request) => request.method === 'PUT')!;
    expect(put.headers.get('If-Match')).toBe('"organization-21-r3"');
    expect(await put.json()).toMatchObject({ media: [{ content_hash: 'sha256:synthetic' }] });
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());

    await fireEvent.click(screen.getByRole('button', { name: 'Manage Synthetic Org' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Delete organization' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete organization' }));
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Manage Synthetic Org' })).toBeNull());
  });

  it('does not undo a concurrent organization retirement during an unrelated field edit', async () => {
    const requests: Request[] = [];
    let reads = 0;
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/organizations/21' && request.method === 'GET') {
        reads += 1;
        const base = organizationProfile(reads + 1);
        const profile = reads === 2
          ? { ...base, organization: { ...base.organization, retired_at: '2026-08-28T00:00:00Z' } }
          : base;
        return new Response(JSON.stringify(profile), { headers: { ETag: `"organization-21-r${reads + 1}"` } });
      }
      if (path === '/api/v1/organizations/21' && request.method === 'PATCH') {
        return new Response(JSON.stringify({ ...organization(21, 3, 'Edited Org'), retired_at: '2026-08-28T00:00:00Z' }), { headers: { ETag: '"organization-21-r3"' } });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.organizations = [organization()];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Manage Synthetic Org' }));
    await fireEvent.input(await screen.findByRole('textbox', { name: 'Organization name' }), { target: { value: 'Edited Org' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save organization' }));
    await waitFor(() => expect(requests.some((request) => request.method === 'PATCH')).toBe(true));

    const body = await requests.find((request) => request.method === 'PATCH')!.json();
    expect(body).toMatchObject({ name: 'Edited Org' });
    expect(body.retired).toBeUndefined();
  });

  it('applies alias/category draft deltas to the fresh profile without deleting concurrent additions', async () => {
    const requests: Request[] = [];
    let reads = 0;
    const baseline = {
      ...organizationProfile(2),
      names: [{ organization_id: 21, name_kind: 'alias', name: 'Baseline Alias', name_normalized: 'baseline alias', envelope: envelope(50) }],
      categories: [{ organization_id: 21, category: 'Baseline Category', category_normalized: 'baseline category', envelope: envelope(51) }]
    };
    const rich = richOrganizationProfile(3);
    const fresh = {
      ...rich,
      names: [
        ...rich.names.filter((item) => item.name_kind !== 'alias'),
        baseline.names[0],
        { organization_id: 21, name_kind: 'alias', name: 'Synthetic Works', name_normalized: 'synthetic works', envelope: envelope(52, { source: 'vcard_import', source_ref: 'concurrent-alias', source_resource_uid: 'resource-concurrent-alias', vcard: { property: 'NICKNAME' } }) }
      ],
      categories: [baseline.categories[0], ...rich.categories]
    };
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/organizations/21' && request.method === 'GET') {
        reads += 1;
        const profile = reads === 1 ? baseline : fresh;
        return new Response(JSON.stringify(profile), { headers: { ETag: `"organization-21-r${reads + 1}"` } });
      }
      if (path === '/api/v1/organizations/21/profile' && request.method === 'PUT') {
        return new Response(JSON.stringify(fresh), { headers: { ETag: '"organization-21-r4"' } });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.organizations = [organization()];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Manage Synthetic Org' }));
    const aliases = await screen.findByRole('textbox', { name: 'Organization aliases' });
    await fireEvent.input(aliases, { target: { value: 'User Draft Alias' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization categories' }), { target: { value: 'User Draft Category' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save organization profile' }));
    await waitFor(() => expect(requests.some((request) => request.method === 'PUT')).toBe(true));

    const put = requests.find((request) => request.method === 'PUT')!;
    expect(put.headers.get('If-Match')).toBe('"organization-21-r3"');
    const body = await put.json();
    expect(body.names).toEqual(expect.arrayContaining([
      expect.objectContaining({ name: 'Synthetic Legal Ltd', name_kind: 'legal', source_ref: 'card-42', vcard_property: 'FN', vcard_group: 'item2', vcard_prop_id: 'prop-9', vcard_pid: ['1.1', '2.1'], vcard_altid: 'alt-4' }),
      expect.objectContaining({ name: 'Synthetic Works', name_kind: 'alias', source_ref: 'concurrent-alias', source_resource_uid: 'resource-concurrent-alias', vcard_property: 'NICKNAME' }),
      expect.objectContaining({ name: 'User Draft Alias', name_kind: 'alias', source: 'user' })
    ]));
    expect(body.names).not.toEqual(expect.arrayContaining([expect.objectContaining({ name: 'Baseline Alias' })]));
    expect(body.categories).toEqual(expect.arrayContaining([
      expect.objectContaining({ category: 'Research', source_ref: 'card-42', source_resource_uid: 'resource-card-42', active_from: '2025-02-03T04:05:06Z' }),
      expect.objectContaining({ category: 'User Draft Category', source: 'user' })
    ]));
    expect(body.categories).not.toEqual(expect.arrayContaining([expect.objectContaining({ category: 'Baseline Category' })]));
    expect(body.contact_points).toEqual([expect.objectContaining({
      contact_kind: 'email', original_value: 'concurrent@example.test', service_slug: 'email', scope_kind: 'department', scope_value: 'research',
      uri: 'mailto:concurrent@example.test', ordinal: 7, pref: 2, type_label: 'work', type_tokens: ['WORK', 'INTERNET'],
      source: 'vcard_import', source_ref: 'card-42', confidence: 0.87, active_from: '2025-02-03T04:05:06Z',
      vcard_property: 'EMAIL', vcard_group: 'item2', vcard_prop_id: 'prop-9', vcard_pid: ['1.1', '2.1'], vcard_altid: 'alt-4'
    })]);
    expect(body.addresses).toEqual([expect.objectContaining({
      address_kind: 'work', original_value: 'Suite 5, 12 Example Road', post_office_box: 'PO 12', extended_address: 'Suite 5',
      street_address: '12 Example Road', locality: 'Exampleton', region: 'EX', postal_code: '12345', country_name: 'Exampleland',
      extended_components: 'Lab 3', free_text: 'Rear entrance', place_uri: 'geo:1,2', geo_uri: 'geo:1,2', label: 'HQ', timezone: 'Etc/UTC', country_code: 'EX',
      source_ref: 'card-42', vcard_property: 'ADR'
    })]);
    expect(body.identifiers).toEqual([expect.objectContaining({ identifier_kind: 'registry', identifier_value: 'REG-42', source_ref: 'card-42', vcard_property: 'X-REGISTRY' })]);
    expect(body.media).toEqual([expect.objectContaining({ media_kind: 'logo', media_type: 'image/png', uri: 'https://example.test/logo.png', original_value: 'Concurrent logo', content_hash: 'sha256:concurrent', source_ref: 'card-42', vcard_property: 'LOGO' })]);
    expect(body.media[0].data).toBeUndefined();
  });

  it('retains a profile draft, shows complete refreshed organization data, and retries only on explicit save', async () => {
    let reads = 0;
    let puts = 0;
    const requests: Request[] = [];
    const concurrent = {
      ...richOrganizationProfile(3),
      organization: { ...organization(21, 3, 'Concurrent Org Name'), primary_domain: 'concurrent.example', description: 'Concurrent organization description' }
    };
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/organizations/21' && request.method === 'GET') {
        reads += 1;
        const profile = reads < 3 ? organizationProfile(reads) : concurrent;
        return new Response(JSON.stringify(profile), { headers: { ETag: `"organization-21-r${reads}"` } });
      }
      if (path === '/api/v1/organizations/21/profile' && request.method === 'PUT') {
        puts += 1;
        return puts === 1
          ? Response.json({ error: 'revision_conflict', message: 'profile changed elsewhere' }, { status: 412 })
          : new Response(JSON.stringify(concurrent), { headers: { ETag: '"organization-21-r5"' } });
      }
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.organizations = [organization()];
    render(OrganizationEmploymentTab, { controller, personID: 7 });

    await fireEvent.click(screen.getByRole('button', { name: 'Manage Synthetic Org' }));
    const aliases = await screen.findByRole('textbox', { name: 'Organization aliases' }) as HTMLInputElement;
    await fireEvent.input(aliases, { target: { value: 'Retained Draft Alias' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save organization profile' }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Concurrent Org Name');
    expect(alert.textContent).toContain('concurrent.example');
    expect(alert.textContent).toContain('Concurrent organization description');
    expect(alert.textContent).toContain('Research');
    expect(alert.textContent).toContain('concurrent@example.test');
    expect(alert.textContent).toContain('Suite 5, 12 Example Road');
    expect(alert.textContent).toContain('REG-42');
    expect(alert.textContent).toContain('Concurrent logo');
    expect(aliases.value).toBe('Retained Draft Alias');
    expect(puts).toBe(1);

    await fireEvent.click(screen.getByRole('button', { name: 'Save organization profile' }));
    await waitFor(() => expect(puts).toBe(2));
    expect(requests.filter((request) => request.method === 'PUT')[1]?.headers.get('If-Match')).toBe('"organization-21-r4"');
  });

  it('keeps a conflicted employment delete open with complete current data and owns modal shortcuts', async () => {
    let reads = 0;
    let deletes = 0;
    const rootShortcut = vi.fn();
    const unregister = appShortcuts.register('x', rootShortcut);
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const path = pathOf(request);
      if (path === '/api/v1/employments/11' && request.method === 'GET') {
        reads += 1;
        return new Response(JSON.stringify(employment(11, reads + 1, {
          department: 'Concurrent department', location: 'Concurrent office', description: 'Concurrent description', address_id: 88, confidence: 0.88
        })), { headers: { ETag: `"employment-11-r${reads + 1}"` } });
      }
      if (path === '/api/v1/employments/11' && request.method === 'DELETE') {
        deletes += 1;
        return deletes === 1 ? Response.json({ error: 'revision_conflict', message: 'stale employment' }, { status: 412 }) : new Response(null, { status: 204 });
      }
      if (path === '/api/v1/people/7/employments') return Response.json({ employments: [] });
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    controller.employments = [employment()];
    const view = render(OrganizationEmploymentTab, { controller, personID: 7 });
    try {
      await fireEvent.click(screen.getByRole('button', { name: 'Delete employment' }));
      expect(appShortcuts.activeScope()).toBe('directory-employment-delete');
      appShortcuts.handleKeydown(new KeyboardEvent('keydown', { key: 'x', cancelable: true }));
      expect(rootShortcut).not.toHaveBeenCalled();
      await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete employment' }));

      const dialog = await screen.findByRole('dialog', { name: 'Delete employment' });
      const alert = await within(dialog).findByRole('alert');
      expect(alert.textContent).toContain('Concurrent department');
      expect(alert.textContent).toContain('Concurrent office');
      expect(alert.textContent).toContain('Concurrent description');
      expect(alert.textContent).toContain('Address ID 88');
      expect(alert.textContent).toContain('0.88');
      expect(deletes).toBe(1);

      window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));
      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Delete employment' })).toBeNull());
      expect(appShortcuts.activeScope()).toBe('root');
    } finally {
      view.unmount();
      unregister();
    }
  });

  it('does not dismiss an editor during submission and closes it after success', async () => {
    let resolveCreate!: (response: Response) => void;
    const pending = new Promise<Response>((resolve) => { resolveCreate = resolve; });
    const controller = controllerWith(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (pathOf(request) === '/api/v1/organizations' && request.method === 'POST') return pending;
      throw new Error(`unexpected ${request.method} ${pathOf(request)}`);
    }));
    render(OrganizationEmploymentTab, { controller, personID: 7 });
    await fireEvent.click(screen.getByRole('button', { name: 'New organization' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization name' }), { target: { value: 'Pending Org' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create organization' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Close organization editor' }));
    expect(screen.getByRole('dialog', { name: 'Create organization' })).toBeDefined();
    resolveCreate(new Response(JSON.stringify(organization(22, 1, 'Pending Org')), { status: 201, headers: { ETag: '"organization-22-r1"' } }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Create organization' })).toBeNull());
  });
});
