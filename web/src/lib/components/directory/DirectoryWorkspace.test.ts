import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { DirectoryController } from '../../directory/controller.svelte';
import type { DirectoryURLState } from '../../directory/models';
import { chooseSelectOption } from '../../../test/kit-ui';
import DirectoryWorkspace from './DirectoryWorkspace.svelte';

const state: DirectoryURLState = {
  directoryQuery: '', directoryContactState: '', directoryCategory: '',
  directoryOrganization: '', directoryPrimaryChannel: '', directoryLastContactAfter: '', directoryLastContactBefore: '', directorySort: 'name', directoryPersonID: null
};

function pathOf(request: Request): string {
  return new URL(request.url, document.baseURI).pathname;
}

function directoryResponse() {
  return Response.json({
    people: [{
      id: 7, revision: 2, display_name: 'Synthetic Person', contact_state: 'active',
      categories: ['friend'], organizations: ['Example Org'], primary_channel: 'email'
    }]
  });
}

describe('DirectoryWorkspace', () => {
  afterEach(() => { vi.useRealTimers(); });

  it('sends one request and replaces one history entry after a burst of typing', async () => {
    vi.useFakeTimers();
    const commits: Array<[Partial<DirectoryURLState>, string]> = [];
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return directoryResponse();
    }));
    const controller = new DirectoryController(client, (patch, history) => commits.push([patch, history]));
    render(DirectoryWorkspace, { client, controller, state });
    await vi.advanceTimersByTimeAsync(0);
    expect(requests).toHaveLength(1);

    const search = screen.getByRole('searchbox', { name: 'Search directory' }) as HTMLInputElement;
    for (const typed of ['a', 'al', 'ali', 'alic', 'alice']) {
      await fireEvent.input(search, { target: { value: typed } });
      await vi.advanceTimersByTimeAsync(100);
    }
    expect(search.value).toBe('alice');
    expect(requests).toHaveLength(1);
    expect(commits).toEqual([]);

    await vi.advanceTimersByTimeAsync(250);
    expect(commits).toEqual([[{
      directoryQuery: 'alice', directoryCategory: '', directoryOrganization: '',
      directoryLastContactAfter: '', directoryLastContactBefore: ''
    }, 'replace']]);
    expect(requests).toHaveLength(2);
    expect(new URL(requests[1]!.url).searchParams.get('q')).toBe('alice');
    expect(controller.query).toBe('alice');
  });

  it('applies a pending text edit before a select filter pushes', async () => {
    vi.useFakeTimers();
    const commits: Array<[Partial<DirectoryURLState>, string]> = [];
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return directoryResponse();
    }));
    const controller = new DirectoryController(client, (patch, history) => commits.push([patch, history]));
    render(DirectoryWorkspace, { client, controller, state });
    await vi.advanceTimersByTimeAsync(0);

    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization filter' }), { target: { value: 'Example Org' } });
    await chooseSelectOption(screen.getByRole('combobox', { name: /^Contact state:/ }), 'Active');
    await vi.advanceTimersByTimeAsync(0);

    expect(commits.map(([, history]) => history)).toEqual(['replace', 'push']);
    const last = new URL(requests.at(-1)!.url).searchParams;
    expect(last.get('organization')).toBe('Example Org');
    expect(last.get('contact_state')).toBe('active');
  });

  it('keeps the current rows visible while a filter change loads', async () => {
    let resolveFiltered: ((response: Response) => void) | undefined;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).searchParams.get('contact_state') === 'active') {
        return new Promise<Response>((resolve) => { resolveFiltered = resolve; });
      }
      return directoryResponse();
    }));
    const controller = new DirectoryController(client);
    render(DirectoryWorkspace, { client, controller, state });
    await screen.findByRole('row', { name: /Synthetic Person/ });

    await chooseSelectOption(screen.getByRole('combobox', { name: /^Contact state:/ }), 'Active');
    await waitFor(() => expect(resolveFiltered).toBeDefined());
    expect(screen.getByRole('row', { name: /Synthetic Person/ })).toBeDefined();
    expect(screen.getByRole('status').textContent).toBe('Updating people…');
    expect(screen.queryByText('Loading people…')).toBeNull();

    resolveFiltered?.(Response.json({ people: [{ id: 8, revision: 1, display_name: 'Filtered Person', contact_state: 'active', categories: [], organizations: [] }] }));
    await screen.findByRole('row', { name: /Filtered Person/ });
    expect(screen.queryByRole('row', { name: /Synthetic Person/ })).toBeNull();
    expect(screen.queryByRole('status')).toBeNull();
  });

  it('offers free-text category and organization filters when no server facet catalog exists', () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async () => directoryResponse()));
    const controller = new DirectoryController(client);

    render(DirectoryWorkspace, { client, controller, state });

    expect(screen.getByRole('textbox', { name: 'Category filter' })).toBeDefined();
    expect(screen.getByRole('textbox', { name: 'Organization filter' })).toBeDefined();
  });

  it('offers last-contact range and ordering controls', () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async () => directoryResponse()));
    const controller = new DirectoryController(client);

    render(DirectoryWorkspace, { client, controller, state });

    expect(screen.getByRole('textbox', { name: 'Last contacted after' })).toBeDefined();
    expect(screen.getByRole('textbox', { name: 'Last contacted before' })).toBeDefined();
    expect(screen.getByRole('combobox', { name: /^Directory order:/ })).toBeDefined();
  });

  it('offers only contact states accepted by the Directory handler contract', async () => {
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return directoryResponse();
    }));
    const controller = new DirectoryController(client);
    render(DirectoryWorkspace, { client, controller, state });

    const contactState = screen.getByRole('combobox', { name: /^Contact state:/ });
    await chooseSelectOption(contactState, 'Active');
    await waitFor(() => expect(new URL(requests.at(-1)!.url).searchParams.get('contact_state')).toBe('active'));
    await chooseSelectOption(contactState, 'Inactive');
    await waitFor(() => expect(new URL(requests.at(-1)!.url).searchParams.get('contact_state')).toBe('inactive'));
    await fireEvent.click(contactState);
    expect(screen.queryByRole('option', { name: /unknown/i })).toBeNull();
  });

  it('promotes an explicitly supplied participant context and commits the returned person ID', async () => {
    const commits: Array<Partial<DirectoryURLState>> = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (pathOf(request) === '/api/v1/people') return Response.json({ id: 42, revision: 1 }, { status: 201 });
      if (pathOf(request) === '/api/v1/people/directory') return directoryResponse();
      if (pathOf(request).endsWith('/files/search')) return Response.json({ files: [], total_count: 0, cache_revision: 'synthetic', search_provenance: {} });
      return Response.json({ id: 42, revision: 1, participant_ids: [], vcard_uid: '', created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' });
    });
    const client = createAPIClient(fetchFn);
    const controller = new DirectoryController(client, (patch) => commits.push(patch));

    render(DirectoryWorkspace, { client, controller, state, promotionParticipantID: 11 });

    await fireEvent.click(await screen.findByRole('button', { name: 'Promote to person' }));

    await waitFor(() => expect(commits).toContainEqual({ directoryPersonID: 42 }));
    await waitFor(() => expect(controller.selectedPersonID).toBe(42));
  });

  it('keeps loaded rows visible when loading another page fails and retries that page', async () => {
    let pageRequests = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (pathOf(request) !== '/api/v1/people/directory') throw new Error(`unexpected ${pathOf(request)}`);
      pageRequests += 1;
      if (pageRequests === 1) return Response.json({
        people: [{ id: 7, revision: 2, display_name: 'Synthetic Person', contact_state: 'active', categories: [], organizations: [] }],
        next_cursor: 'next'
      });
      if (pageRequests === 2) throw new Error('network offline');
      return Response.json({ people: [{ id: 8, revision: 1, display_name: 'Fixture Person', contact_state: 'unknown', categories: [], organizations: [] }] });
    });
    const client = createAPIClient(fetchFn);
    const controller = new DirectoryController(client);
    render(DirectoryWorkspace, { client, controller, state });

    expect(await screen.findByText('Synthetic Person')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Load more people' }));
    expect((await screen.findByRole('alert')).textContent).toContain('network offline');
    expect(screen.getByText('Synthetic Person')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry loading more people' }));
    expect(await screen.findByText('Fixture Person')).toBeDefined();
  });

  it('offers a page-one reload after a terminal cursor failure and retains rows until success', async () => {
    let pageRequests = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (pathOf(request) !== '/api/v1/people/directory') throw new Error(`unexpected ${pathOf(request)}`);
      pageRequests += 1;
      if (pageRequests === 1) return Response.json({
        people: [{ id: 7, revision: 2, display_name: 'Retained Person', contact_state: 'active', categories: [], organizations: [] }],
        next_cursor: 'invalidated'
      });
      if (pageRequests === 2) return Response.json({ error: 'invalid_cursor', message: 'Directory changed' }, { status: 400 });
      return Response.json({
        people: [{ id: 8, revision: 1, display_name: 'Reloaded Person', contact_state: 'active', categories: [], organizations: [] }]
      });
    });
    const client = createAPIClient(fetchFn);
    const controller = new DirectoryController(client);
    render(DirectoryWorkspace, { client, controller, state });

    expect(await screen.findByText('Retained Person')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Load more people' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Directory changed');
    expect(screen.getByText('Retained Person')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Reload directory' }));
    expect(await screen.findByText('Reloaded Person')).toBeDefined();
  });

  it('renders actionable binding guidance from the structured promotion code', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (pathOf(request) === '/api/v1/people') {
        return Response.json({ error: 'person_binding_conflict', message: 'Different durable profiles own this cluster.' }, { status: 409 });
      }
      return directoryResponse();
    }));
    const controller = new DirectoryController(client);
    render(DirectoryWorkspace, { client, controller, state, promotionParticipantID: 11 });

    await fireEvent.click(await screen.findByRole('button', { name: 'Promote to person' }));
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Different durable profiles own this cluster.');
    expect(alert.textContent).toContain('already belongs to another durable person');
  });

  it('renders structured editing through the selection-owned profile controller', async () => {
    const selectedState = { ...state, directoryPersonID: 7 };
    const selectedPerson = {
      id: 7, revision: 2, display_name: 'Synthetic Person', participant_ids: [], vcard_uid: 'person-7',
      created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z'
    };
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') return directoryResponse();
      if (path === '/api/v1/people/7/profile') return new Response(JSON.stringify({
        person: selectedPerson, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: []
      }), { headers: { 'Content-Type': 'application/json', ETag: '"person-7-r2"' } });
      if (path === '/api/v1/people/7') return new Response(JSON.stringify(selectedPerson), {
        headers: { 'Content-Type': 'application/json', ETag: '"person-7-r2"' }
      });
      if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
      if (path.endsWith('/files/search')) return Response.json({ files: [], total_count: 0, cache_revision: 'synthetic', search_provenance: {} });
      if (path.endsWith('/employments')) return Response.json({ employments: [] });
      if (path.endsWith('/relationships')) return Response.json({ relationships: [] });
      if (path.endsWith('/days')) return Response.json({ person_id: 7, total_count: 0, days: [] });
      if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: '2026-01-01T00:00:00Z', stale: false });
      throw new Error(`unexpected ${path}`);
    }));
    const controller = new DirectoryController(client);

    render(DirectoryWorkspace, { client, controller, state: selectedState });

    expect(await screen.findByRole('heading', { name: 'Structured profile' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Add name' })).toHaveProperty('disabled', false);
    expect(controller.profile).not.toBeNull();
    await fireEvent.click(screen.getByRole('tab', { name: 'Organizations' }));
    expect(screen.getByRole('tabpanel', { name: 'Organizations' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Add employment' })).toBeDefined();
    expect(controller.entity).not.toBeNull();
  });

  it('removes a confirmed deleted person from the Directory and clears its URL selection', async () => {
    const selectedState = { ...state, directoryPersonID: 7 };
    const commits: Array<Partial<DirectoryURLState>> = [];
    const selectedPerson = {
      id: 7, revision: 2, display_name: 'Synthetic Person', participant_ids: [], vcard_uid: 'person-7',
      created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z'
    };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') return directoryResponse();
      if (path === '/api/v1/people/7' && request.method === 'DELETE') return new Response(null, { status: 204 });
      if (path === '/api/v1/people/7') return new Response(JSON.stringify(selectedPerson), { headers: { 'Content-Type': 'application/json', ETag: '"person-7-r2"' } });
      if (path === '/api/v1/people/7/profile') return new Response(JSON.stringify({
        person: selectedPerson, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: []
      }), { headers: { 'Content-Type': 'application/json', ETag: '"person-7-r2"' } });
      if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
      if (path.endsWith('/files/search')) return Response.json({ files: [], total_count: 0, cache_revision: 'synthetic', search_provenance: {} });
      if (path.endsWith('/employments')) return Response.json({ employments: [] });
      if (path.endsWith('/relationships')) return Response.json({ relationships: [] });
      if (path.endsWith('/days')) return Response.json({ person_id: 7, total_count: 0, days: [] });
      if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: '2026-01-01T00:00:00Z', stale: false });
      return Response.json({ error: 'unavailable', message: 'not rendered' }, { status: 503 });
    });
    const controller = new DirectoryController(createAPIClient(fetchFn), (patch) => commits.push(patch));
    render(DirectoryWorkspace, { client: createAPIClient(fetchFn), controller, state: selectedState });

    await fireEvent.click(await screen.findByRole('button', { name: 'Delete person' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete person' }));

    await waitFor(() => expect(controller.selectedPersonID).toBeNull());
    expect(controller.rows).toEqual([]);
    expect(commits).toContainEqual({ directoryPersonID: null });
  });

  it('renders unfiltered Directory category summaries immediately after add and close', async () => {
    const selectedState = { ...state, directoryPersonID: 7 };
    const selectedPerson = {
      id: 7, revision: 2, display_name: 'Synthetic Person', participant_ids: [], vcard_uid: 'person-7',
      created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z'
    };
    const friend = {
      person_id: 7, original_value: 'Friend', normalized_value: 'friend',
      envelope: { id: 31, ordinal: 0, source: 'user', created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', vcard: {} }
    };
    const vip = {
      person_id: 7, original_value: 'VIP', normalized_value: 'vip',
      envelope: { id: 32, ordinal: 1, source: 'user', created_at: '2026-01-02T00:00:00Z', updated_at: '2026-01-02T00:00:00Z', vcard: {} }
    };
    let profileWrite = 0;
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') return Response.json({ people: [{
        id: 7, revision: 2, display_name: 'Synthetic Person', contact_state: 'active',
        categories: ['Friend'], organizations: ['Example Org'], primary_channel: 'email'
      }] });
      if (path === '/api/v1/people/7/profile' && request.method === 'PATCH') {
        profileWrite += 1;
        const categories = profileWrite === 1 ? [friend, vip] : [friend];
        return new Response(JSON.stringify({
          person: { ...selectedPerson, revision: 2 + profileWrite }, names: [], contact_points: [], addresses: [], dates: [], categories, media: []
        }), { headers: { 'Content-Type': 'application/json', ETag: `"person-7-r${2 + profileWrite}"` } });
      }
      if (path === '/api/v1/people/7/profile') return new Response(JSON.stringify({
        person: selectedPerson, names: [], contact_points: [], addresses: [], dates: [], categories: [friend], media: []
      }), { headers: { 'Content-Type': 'application/json', ETag: '"person-7-r2"' } });
      if (path === '/api/v1/people/7') return new Response(JSON.stringify(selectedPerson), { headers: { 'Content-Type': 'application/json', ETag: '"person-7-r2"' } });
      if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
      if (path.endsWith('/files/search')) return Response.json({ files: [], total_count: 0, cache_revision: 'synthetic', search_provenance: {} });
      if (path.endsWith('/employments')) return Response.json({ employments: [] });
      if (path.endsWith('/relationships')) return Response.json({ relationships: [] });
      if (path.endsWith('/days')) return Response.json({ person_id: 7, total_count: 0, days: [] });
      if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: '2026-01-01T00:00:00Z', stale: false });
      throw new Error(`unexpected ${request.method} ${path}`);
    });
    const client = createAPIClient(fetchFn);
    const controller = new DirectoryController(client);
    render(DirectoryWorkspace, { client, controller, state: selectedState });

    await fireEvent.click(await screen.findByRole('button', { name: 'Add category' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Category' }), { target: { value: 'VIP' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save category' }));

    const row = await screen.findByRole('row', { name: /Synthetic Person/ });
    await waitFor(() => expect(row.textContent).toContain('Friend · VIP'));
    expect(requests.filter((request) => pathOf(request) === '/api/v1/people/directory')).toHaveLength(1);

    await fireEvent.click(screen.getByRole('button', { name: 'Close category VIP' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm close category' }));
    await waitFor(() => expect(row.textContent).not.toContain('VIP'));
    expect(row.textContent).toContain('Friend');
    expect(requests.filter((request) => pathOf(request) === '/api/v1/people/directory')).toHaveLength(1);
  });

  it('keeps a server-returned Unicode category match visible after the exact write overlay', async () => {
    const selectedState = { ...state, directoryCategory: 'STRASSE', directoryPersonID: 7 };
    const selectedPerson = {
      id: 7, revision: 2, display_name: 'Synthetic Person', participant_ids: [], vcard_uid: 'person-7',
      created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z'
    };
    const street = {
      person_id: 7, original_value: 'Straße', normalized_value: 'strasse',
      envelope: { id: 31, ordinal: 0, source: 'user', created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', vcard: {} }
    };
    let directoryRead = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') {
        directoryRead += 1;
        return Response.json({ people: [{
          id: 7, revision: 2, display_name: 'Synthetic Person', contact_state: 'active',
          categories: [], organizations: ['Example Org'], primary_channel: 'email'
        }] });
      }
      if (path === '/api/v1/people/7/profile' && request.method === 'PATCH') {
        return new Response(JSON.stringify({
          person: { ...selectedPerson, revision: 3 }, names: [], contact_points: [], addresses: [], dates: [], categories: [street], media: []
        }), { headers: { 'Content-Type': 'application/json', ETag: '"person-7-r3"' } });
      }
      if (path === '/api/v1/people/7/profile') return new Response(JSON.stringify({
        person: selectedPerson, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: []
      }), { headers: { 'Content-Type': 'application/json', ETag: '"person-7-r2"' } });
      if (path === '/api/v1/people/7') return new Response(JSON.stringify(selectedPerson), { headers: { 'Content-Type': 'application/json', ETag: '"person-7-r2"' } });
      if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
      if (path.endsWith('/files/search')) return Response.json({ files: [], total_count: 0, cache_revision: 'synthetic', search_provenance: {} });
      if (path.endsWith('/employments')) return Response.json({ employments: [] });
      if (path.endsWith('/relationships')) return Response.json({ relationships: [] });
      if (path.endsWith('/days')) return Response.json({ person_id: 7, total_count: 0, days: [] });
      if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: '2026-01-01T00:00:00Z', stale: false });
      throw new Error(`unexpected ${request.method} ${path}`);
    });
    const client = createAPIClient(fetchFn);
    const controller = new DirectoryController(client);
    render(DirectoryWorkspace, { client, controller, state: selectedState });

    await fireEvent.click(await screen.findByRole('button', { name: 'Add category' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Category' }), { target: { value: 'Straße' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save category' }));

    const row = await screen.findByRole('row', { name: /Synthetic Person/ });
    await waitFor(() => expect(row.textContent).toContain('Straße'));
    expect(directoryRead).toBe(2);
    expect(controller.category).toBe('STRASSE');
    expect(controller.selectedPersonID).toBe(7);
  });

  it('keeps prior rows and reloads current filters after filtered write reconciliation fails', async () => {
    const selectedState = {
      ...state,
      directoryQuery: 'synthetic',
      directoryContactState: 'active',
      directoryCategory: 'STRASSE',
      directoryOrganization: 'Example Org',
      directoryPrimaryChannel: 'email',
      directoryPersonID: 7
    };
    const selectedPerson = {
      id: 7, revision: 2, display_name: 'Synthetic Person', participant_ids: [], vcard_uid: 'person-7',
      created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z'
    };
    const street = {
      person_id: 7, original_value: 'Straße', normalized_value: 'strasse',
      envelope: { id: 31, ordinal: 0, source: 'user', created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', vcard: {} }
    };
    const directoryRequests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') {
        directoryRequests.push(request);
        if (directoryRequests.length === 1) return Response.json({ people: [{
          id: 7, revision: 2, display_name: 'Synthetic Person', contact_state: 'active',
          categories: [], organizations: ['Example Org'], primary_channel: 'email'
        }], next_cursor: 'old-next' });
        if (directoryRequests.length === 2) {
          return Response.json({ error: 'unavailable', message: 'Directory reconciliation unavailable.' }, { status: 503 });
        }
        return Response.json({ people: [{
          id: 7, revision: 3, display_name: 'Synthetic Person', contact_state: 'active',
          categories: ['Straße'], organizations: ['Example Org'], primary_channel: 'email'
        }], next_cursor: 'recovered-next' });
      }
      if (path === '/api/v1/people/7/profile' && request.method === 'PATCH') {
        return new Response(JSON.stringify({
          person: { ...selectedPerson, revision: 3 }, names: [], contact_points: [], addresses: [], dates: [], categories: [street], media: []
        }), { headers: { 'Content-Type': 'application/json', ETag: '"person-7-r3"' } });
      }
      if (path === '/api/v1/people/7/profile') return new Response(JSON.stringify({
        person: selectedPerson, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: []
      }), { headers: { 'Content-Type': 'application/json', ETag: '"person-7-r2"' } });
      if (path === '/api/v1/people/7') return new Response(JSON.stringify(selectedPerson), { headers: { 'Content-Type': 'application/json', ETag: '"person-7-r2"' } });
      if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
      if (path.endsWith('/files/search')) return Response.json({ files: [], total_count: 0, cache_revision: 'synthetic', search_provenance: {} });
      if (path.endsWith('/employments')) return Response.json({ employments: [] });
      if (path.endsWith('/relationships')) return Response.json({ relationships: [] });
      if (path.endsWith('/days')) return Response.json({ person_id: 7, total_count: 0, days: [] });
      if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: '2026-01-01T00:00:00Z', stale: false });
      throw new Error(`unexpected ${request.method} ${path}`);
    });
    const client = createAPIClient(fetchFn);
    const controller = new DirectoryController(client);
    render(DirectoryWorkspace, { client, controller, state: selectedState });

    const row = await screen.findByRole('row', { name: /Synthetic Person/ });
    await fireEvent.click(await screen.findByRole('button', { name: 'Add category' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Category' }), { target: { value: 'Straße' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save category' }));

    expect(await screen.findByText('Directory reconciliation unavailable.')).toBeDefined();
    expect(row.textContent).not.toContain('Straße');
    expect(await screen.findByRole('button', { name: 'Close category Straße' })).toBeDefined();
    expect(controller.profile?.conflict).toBeNull();
    expect(controller.selectedPersonID).toBe(7);
    expect(screen.getByRole('button', { name: 'Reload directory' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Load more people' })).toBeNull();

    await controller.loadNextPage();
    expect(directoryRequests).toHaveLength(2);
    expect(controller.pageError).toBe('Directory reconciliation unavailable.');
    expect(controller.pageRecovery).toBe('reload');

    await fireEvent.click(screen.getByRole('button', { name: 'Reload directory' }));

    await waitFor(() => expect(row.textContent).toContain('Straße'));
    expect(controller.pageError).toBeNull();
    expect(controller.pageRecovery).toBeNull();
    expect(controller.cursor).toBe('recovered-next');
    const reloadParameters = new URL(directoryRequests[2]!.url).searchParams;
    expect(reloadParameters.get('q')).toBe('synthetic');
    expect(reloadParameters.get('contact_state')).toBe('active');
    expect(reloadParameters.get('category')).toBe('STRASSE');
    expect(reloadParameters.get('organization')).toBe('Example Org');
    expect(reloadParameters.get('primary_channel')).toBe('email');
  });

  it('updates the preferred channel without changing the observed Directory channel', async () => {
    const selectedState = { ...state, directoryPrimaryChannel: 'email', directoryPersonID: 7 };
    const selectedPerson = {
      id: 7, revision: 2, display_name: 'Synthetic Person', participant_ids: [], vcard_uid: 'person-7',
      created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z'
    };
    const definition = {
      id: 1, universal_id: 'primary-channel-id', object_type: 'person', slug: 'primary_channel', label: 'Primary channel',
      value_type: 'text', field_type: 'select', cardinality: 'single', display_order: 0, is_required: false,
      ownership: 'system', ui_creatable: true, ui_editable: true, api_mutable: true, is_searchable: false,
      is_sensitive: false, is_audited: true, is_deletable: false, history_exempt: false,
      options: { choices: [{ value: 'email', label: 'Email' }, { value: 'chat', label: 'Chat' }] }, is_active: true,
      revision: 1, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z'
    };
    const email = {
      id: 41, person_id: 7, definition_id: 1, definition_slug: 'primary_channel', ordinal: 0,
      value: { type: 'text', text: 'email' }, active_from: '2026-01-01T00:00:00Z', created_at: '2026-01-01T00:00:00Z', source: 'user'
    };
    const chat = {
      ...email, id: 42, value: { type: 'text', text: 'chat' }, active_from: '2026-01-02T00:00:00Z', created_at: '2026-01-02T00:00:00Z'
    };
    let attributeWrite = 0;
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') return directoryResponse();
      if (path === '/api/v1/people/7/profile') return new Response(JSON.stringify({
        person: selectedPerson, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: []
      }), { headers: { 'Content-Type': 'application/json', ETag: '"person-7-r2"' } });
      if (path === '/api/v1/people/7') return new Response(JSON.stringify(selectedPerson), { headers: { 'Content-Type': 'application/json', ETag: '"person-7-r2"' } });
      if (path === '/api/v1/people/7/attributes/primary_channel') {
        attributeWrite += 1;
        return attributeWrite === 1
          ? Response.json({ dry_run: false, value: chat, superseded: { ...email, active_until: '2026-01-02T00:00:00Z', superseded_at: '2026-01-02T00:00:00Z' } })
          : Response.json({ dry_run: false, superseded: { ...chat, active_until: '2026-01-03T00:00:00Z', superseded_at: '2026-01-03T00:00:00Z' } });
      }
      if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [{ definition, current: [email], history: [] }] });
      if (path.endsWith('/files/search')) return Response.json({ files: [], total_count: 0, cache_revision: 'synthetic', search_provenance: {} });
      if (path.endsWith('/employments')) return Response.json({ employments: [] });
      if (path.endsWith('/relationships')) return Response.json({ relationships: [] });
      if (path.endsWith('/days')) return Response.json({ person_id: 7, total_count: 0, days: [] });
      if (path.endsWith('/contact-state')) return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: '2026-01-01T00:00:00Z', stale: false });
      throw new Error(`unexpected ${request.method} ${path}`);
    });
    const client = createAPIClient(fetchFn);
    const controller = new DirectoryController(client);
    render(DirectoryWorkspace, { client, controller, state: selectedState });

    await fireEvent.click(await screen.findByRole('button', { name: 'Edit Primary channel value 1' }));
    await fireEvent.change(screen.getByRole('combobox', { name: 'Primary channel' }), { target: { value: 'chat' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));

    const row = screen.getByRole('row', { name: /Synthetic Person/ });
    await waitFor(() => expect(
      controller.profile?.attributes?.attributes?.[0]?.current?.[0]?.value
    ).toEqual({ type: 'text', text: 'chat' }));
    expect(row.textContent).toContain('email · active');
    expect(requests.filter((request) => pathOf(request) === '/api/v1/people/directory')).toHaveLength(1);

    await fireEvent.click(screen.getByRole('button', { name: 'Close Primary channel value 1' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm close attribute' }));
    await waitFor(() => expect(controller.profile?.attributes?.attributes?.[0]?.current).toEqual([]));
    expect(row.textContent).toContain('email · active');
    expect(requests.filter((request) => pathOf(request) === '/api/v1/people/directory')).toHaveLength(1);
  });

  it('returns focus to the roving row after a narrow detail drawer closes', async () => {
    const changeListeners = new Set<(event: MediaQueryListEvent) => void>();
    vi.stubGlobal('matchMedia', () => ({
      matches: true,
      addEventListener: (_type: string, listener: (event: MediaQueryListEvent) => void) => changeListeners.add(listener),
      removeEventListener: (_type: string, listener: (event: MediaQueryListEvent) => void) => changeListeners.delete(listener)
    }));
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (pathOf(request) === '/api/v1/people/directory') return directoryResponse();
      return Response.json({ id: 7, revision: 1, participant_ids: [], vcard_uid: '', created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' });
    });
    const client = createAPIClient(fetchFn);
    const controller = new DirectoryController(client);
    const rendered = render(DirectoryWorkspace, { client, controller, state });

    const row = await screen.findByRole('row', { name: /Synthetic Person/ });
    await fireEvent.click(row);
    expect(await screen.findByRole('dialog', { name: 'Person detail' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    await waitFor(() => expect(controller.selectedPersonID).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(row));

    rendered.unmount();
    vi.unstubAllGlobals();
  });

  it('destroys the person publication context when a narrow detail drawer closes', async () => {
    vi.stubGlobal('matchMedia', () => ({
      matches: true,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn()
    }));
    let publicationSignal: AbortSignal | undefined;
    let resolvePublication!: (response: Response) => void;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = pathOf(request);
      if (path === '/api/v1/people/directory') return directoryResponse();
      if (path === '/api/v1/carddav/publications/7') {
        publicationSignal = request.signal;
        return new Promise<Response>((resolve) => { resolvePublication = resolve; });
      }
      return Response.json({
        id: 7, revision: 1, participant_ids: [], vcard_uid: '',
        created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z'
      });
    });
    const client = createAPIClient(fetchFn);
    const controller = new DirectoryController(client);
    const rendered = render(DirectoryWorkspace, { client, controller, state });

    await fireEvent.click(await screen.findByRole('row', { name: /Synthetic Person/ }));
    expect(await screen.findByRole('dialog', { name: 'Person detail' })).toBeDefined();
    await waitFor(() => expect(publicationSignal).toBeDefined());
    await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(publicationSignal?.aborted).toBe(true);
    resolvePublication(Response.json({
      person_id: 7, state: 'published', desired: true,
      address_book: { id: 5, name: 'Old contacts' }
    }));
    await Promise.resolve();
    expect(screen.queryByText('Old contacts')).toBeNull();

    rendered.unmount();
    vi.unstubAllGlobals();
  });
});
