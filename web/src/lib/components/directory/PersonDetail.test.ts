import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { DirectoryEntityController } from '../../directory/entity-controller.svelte';
import type { DirectoryReadBundle } from '../../directory/models';
import PersonDetail from './PersonDetail.svelte';

function profileMaintenanceResponse(request: Request): Response | undefined {
  const path = new URL(request.url).pathname;
  if (path === '/api/v1/people/7/tracking') {
    return Response.json({ person_id: 7, tracked: false, tracked_at: null });
  }
  if (path === '/api/v1/person-fact-targets') {
    return Response.json({ version: 'v1', fingerprint: 'not-rendered', targets: [] });
  }
  return undefined;
}

describe('PersonDetail', () => {
  it('renders available read sections, marks sensitive attributes, and keeps Media & Files person-scoped', async () => {
    const requestPaths: string[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      requestPaths.push(path);
      const maintenance = profileMaintenanceResponse(request);
      if (maintenance) return maintenance;
      if (path.endsWith('/merges')) return Response.json({ merges: [], limit: 100, offset: 0 });
      if (path === '/api/v1/people/7/employments') return Response.json({ employments: [{ id: 3, person_id: 7, organization_id: 2, is_current: true, is_primary: true, source: 'user', revision: 1, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', title: 'Engineer' }], projection: { employment_id: 3, organization_id: 2, organization_name: 'Example Org', vcard: {} } });
      if (path === '/api/v1/people/7/relationships') return Response.json({ relationships: [
        { counterpart_person_id: 9, counterpart_display_name: 'Synthetic Child', counterpart_label: 'child', counterpart_vcard_uid: 'urn:uuid:child', direction: 'outgoing', relationship: { id: 4, source_person_id: 7, target_person_id: 9, relationship_type_id: 1, type_slug: 'parent', forward_label: 'parent', reverse_label: 'child', is_symmetric: false, status: 'active', source: 'user', created_by: 'user', updated_by: 'user', revision: 1, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', vcard_identity: {} } },
        { counterpart_person_id: 10, counterpart_label: 'parent', counterpart_vcard_uid: 'urn:uuid:parent', direction: 'incoming', relationship: { id: 5, source_person_id: 10, target_person_id: 7, relationship_type_id: 1, type_slug: 'parent', forward_label: 'parent', reverse_label: 'child', is_symmetric: false, status: 'active', source: 'user', created_by: 'user', updated_by: 'user', revision: 1, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', vcard_identity: {} } }
      ] });
      if (path === '/api/v1/relationship-types') return Response.json({ relationship_types: [] });
      return Response.json({ files: [], total_count: 0, cache_revision: 'synthetic', search_provenance: {} });
    }));
    const bundle = {
      person: { id: 7, revision: 2, display_name: 'Synthetic Person', participant_ids: [], vcard_uid: '', created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' },
      structuredProfile: {
        person: { id: 7, revision: 2, participant_ids: [], vcard_uid: '', created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' },
        names: [{ person_id: 7, name_kind: 'legal', original_value: 'Synthetic Person', is_derived: false, envelope: { id: 1, ordinal: 0, source: 'user', created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', vcard: {} } }],
        contact_points: [{ person_id: 7, address_kind: 'email', original_value: 'person@example.test', normalized_value: 'person@example.test', normalization: 'email', normalization_version: 1, service_slug: 'email', envelope: { id: 2, ordinal: 0, source: 'archive_observation', created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', vcard: {} } }],
        addresses: [], dates: [], categories: [], media: []
      },
      attributes: { person_id: 7, attributes: [{ definition: {
        id: 1, slug: 'private-note', label: 'Private note', value_type: 'text', field_type: 'text',
        api_mutable: true, cardinality: 'single', display_order: 0, history_exempt: false,
        is_sensitive: true, is_active: true, is_audited: true, is_deletable: true, is_required: false,
        is_searchable: false, object_type: 'person', ownership: 'user', revision: 1,
        ui_creatable: true, ui_editable: true, universal_id: 'synthetic-private-note',
        created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z'
      }, current: [{ id: 1, person_id: 7, definition_id: 1, definition_slug: 'private-note', ordinal: 0, source: 'user', active_from: '2026-01-01T00:00:00Z', created_at: '2026-01-01T00:00:00Z', value: { type: 'text', text: 'Synthetic value' } }] }] },
      contactState: { person_id: 7, cadence_status: 'active', computed_at: '2026-01-01T00:00:00Z', interaction_count: 4, stale: false },
      activity: { person_id: 7, total_count: 1, days: [{ local_date: '2026-01-01', entry_count: 1, event_count: 0, direct_count: 1 }] },
      etags: {}, errors: {}
    } satisfies DirectoryReadBundle;

    const entityController = new DirectoryEntityController(client, 7);
    void entityController.load();
    render(PersonDetail, { client, bundle, personID: 7, entityController });

    expect(screen.getByText('Names')).toBeDefined();
    expect(screen.getByText('person@example.test')).toBeDefined();
    expect(screen.getByText('Sensitive')).toBeDefined();
    expect(document.body.innerHTML).not.toContain('Synthetic value');
    expect(await screen.findByText(/Example Org/)).toBeDefined();
    expect(screen.getByText('Synthetic Child · child')).toBeDefined();
    expect(screen.getByText('urn:uuid:parent · parent')).toBeDefined();
    expect(screen.queryByText(/outgoing: parent/)).toBeNull();
    expect(requestPaths.filter((path) => path === '/api/v1/people/7/employments')).toHaveLength(1);
    expect(requestPaths.filter((path) => path === '/api/v1/people/7/relationships')).toHaveLength(1);
    expect(requestPaths).not.toContain('/api/v1/people/7/network');
    expect(screen.getByText('Activity')).toBeDefined();
    expect(screen.queryByText('Provenance and history')).toBeNull();
    await fireEvent.click(screen.getByRole('tab', { name: 'Media & Files' }));
    await waitFor(() => expect(requestPaths).toContain('/api/v1/people/7/files/search'));
    expect(requestPaths).not.toContain('/api/v1/participants/7/files/search');
    expect(requestPaths).not.toContain('/api/v1/files/search');
  });

  it('does not claim an organization name for an employment outside the primary projection', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const maintenance = profileMaintenanceResponse(request);
      if (maintenance) return maintenance;
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/people/7/employments') return Response.json({
        employments: [{ id: 4, person_id: 7, organization_id: 9, is_current: true, is_primary: false, source: 'user', revision: 1, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', role: 'Contributor' }],
        projection: { employment_id: 3, organization_id: 2, organization_name: 'Incorrect organization', vcard: {} }
      });
      if (path === '/api/v1/people/7/relationships') return Response.json({ relationships: [] });
      if (path === '/api/v1/relationship-types') return Response.json({ relationship_types: [] });
      return Response.json({ merges: [], limit: 100, offset: 0 });
    }));
    const entityController = new DirectoryEntityController(client, 7);
    void entityController.load();

    render(PersonDetail, { client, bundle: { etags: {}, errors: {} }, personID: 7, entityController });

    expect(await screen.findByText(/Contributor/)).toBeDefined();
    expect(screen.queryByText('Incorrect organization')).toBeNull();
    expect(screen.getByText(/Organization 9/)).toBeDefined();
  });

  it('shows the exact failed section without inventing missing data', () => {
    render(PersonDetail, {
      client: createAPIClient(vi.fn()), personID: 7,
      bundle: { etags: {}, errors: { structuredProfile: 'profile service unavailable' } }
    });

    expect(screen.getByRole('alert').textContent).toContain('Profile: profile service unavailable');
    expect(screen.queryByText('Names')).toBeNull();
  });

  it('implements roving keyboard tabs with linked tabpanels', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const maintenance = profileMaintenanceResponse(request);
      if (maintenance) return maintenance;
      if (new URL(request.url).pathname.endsWith('/merges')) return Response.json({ merges: [], limit: 100, offset: 0 });
      if (new URL(request.url).pathname.endsWith('/network')) return Response.json({
        root_person_id: 7, depth: 1, truncated: false,
        nodes: [{ id: 'person:7', kind: 'person', entity_id: 7, label: 'Synthetic Person', hop: 0 }], edges: []
      });
      return Response.json({ files: [], total_count: 0, cache_revision: 'synthetic', search_provenance: {} });
    }));
    const entityController = new DirectoryEntityController(client, 7);
    render(PersonDetail, { client, personID: 7, bundle: { etags: {}, errors: {} }, entityController });

    const overview = screen.getByRole('tab', { name: 'Overview' });
    const organizations = screen.getByRole('tab', { name: 'Organizations' });
    const relationships = screen.getByRole('tab', { name: 'Relationships' });
    const network = screen.getByRole('tab', { name: 'Network' });
    const media = screen.getByRole('tab', { name: 'Media & Files' });
    expect(overview.getAttribute('tabindex')).toBe('0');
    expect(screen.getAllByRole('tab')).toHaveLength(5);
    expect(await screen.findByRole('heading', { name: 'Merge history' })).toBeDefined();
    expect(organizations.getAttribute('tabindex')).toBe('-1');
    expect(relationships.getAttribute('tabindex')).toBe('-1');
    expect(network.getAttribute('tabindex')).toBe('-1');
    expect(media.getAttribute('tabindex')).toBe('-1');
    expect(overview.getAttribute('aria-controls')).toBe(screen.getByRole('tabpanel', { name: 'Overview' }).id);

    overview.focus();
    await fireEvent.keyDown(overview, { key: 'ArrowRight' });
    expect(document.activeElement).toBe(organizations);
    expect(organizations.getAttribute('aria-selected')).toBe('true');
    await fireEvent.keyDown(organizations, { key: 'ArrowRight' });
    expect(document.activeElement).toBe(relationships);
    await waitFor(() => expect(relationships.getAttribute('aria-controls')).toBe(screen.getByRole('tabpanel', { name: 'Relationships' }).id));
    await fireEvent.keyDown(relationships, { key: 'ArrowRight' });
    expect(document.activeElement).toBe(network);
    await waitFor(() => expect(network.getAttribute('aria-controls')).toBe(screen.getByRole('tabpanel', { name: 'Network' }).id));
    await fireEvent.keyDown(network, { key: 'ArrowRight' });
    expect(document.activeElement).toBe(media);
    expect(media.getAttribute('aria-controls')).toBe(screen.getByRole('tabpanel', { name: 'Media & Files' }).id);

    await fireEvent.keyDown(media, { key: 'ArrowRight' });
    expect(document.activeElement).toBe(overview);
    await fireEvent.keyDown(overview, { key: 'ArrowLeft' });
    expect(document.activeElement).toBe(media);

    await fireEvent.keyDown(media, { key: 'Home' });
    expect(document.activeElement).toBe(overview);
    expect(overview.getAttribute('aria-selected')).toBe('true');

    await fireEvent.keyDown(overview, { key: 'End' });
    expect(document.activeElement).toBe(media);
  });

  it('selects durable people and opens the exact organization editor from network actions', async () => {
    const onOpenPerson = vi.fn();
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      const maintenance = profileMaintenanceResponse(request);
      if (maintenance) return maintenance;
      if (path.endsWith('/network')) return Response.json({
        root_person_id: 7, depth: 1, truncated: false,
        nodes: [
          { id: 'person:7', kind: 'person', entity_id: 7, label: 'Selected Person', hop: 0 },
          { id: 'person:8', kind: 'person', entity_id: 8, label: 'Curated Peer', hop: 1 },
          { id: 'organization:21', kind: 'organization', entity_id: 21, label: 'Shared Organization', hop: 1 }
        ],
        edges: [
          { id: 'relationship:31', kind: 'relationship', source_node_id: 'person:7', target_node_id: 'person:8', label: 'works with' },
          { id: 'employment:41', kind: 'employment', source_node_id: 'person:8', target_node_id: 'organization:21', label: 'Engineer' }
        ]
      });
      if (path === '/api/v1/organizations/21') return new Response(JSON.stringify({
        organization: { id: 21, revision: 2, name: 'Shared Organization', kind: 'company', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z' },
        names: [], contact_points: [], addresses: [], categories: [], identifiers: [], media: []
      }), { headers: { ETag: '"organization-21-r2"' } });
      throw new Error(`unexpected ${request.method} ${path}`);
    }));
    const entityController = new DirectoryEntityController(client, 7);
    render(PersonDetail, {
      client, personID: 7, bundle: { etags: {}, errors: {} }, entityController, onOpenPerson
    });

    await fireEvent.click(screen.getByRole('tab', { name: 'Network' }));
    await fireEvent.click((await screen.findAllByRole('button', { name: 'Open person Curated Peer' }))[0]!);
    expect(onOpenPerson).toHaveBeenCalledWith(8);

    await fireEvent.click(screen.getByRole('tab', { name: 'Network' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Open organization Shared Organization' }));
    await waitFor(() => expect(screen.getByRole('tab', { name: 'Organizations' }).getAttribute('aria-selected')).toBe('true'));
    expect(await screen.findByRole('dialog', { name: 'Edit Shared Organization' })).toBeDefined();
    expect(entityController.organizationETags.get(21)).toBe('"organization-21-r2"');
  });

  it('mounts one compact profile-maintenance card before CardDAV without adding a tab', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/people/7/tracking') {
        return Response.json({ person_id: 7, tracked: false, tracked_at: null });
      }
      if (path === '/api/v1/person-fact-targets') return Response.json({
        version: 'v1', fingerprint: 'not-rendered', targets: []
      });
      if (path === '/api/v1/carddav/publications/7') {
        return Response.json({ error: 'carddav_unavailable', message: 'not rendered' }, { status: 503 });
      }
      if (path === '/api/v1/people/7/merges') return Response.json({ merges: [], limit: 100, offset: 0 });
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    render(PersonDetail, {
      client: createAPIClient(fetchFn), personID: 7,
      bundle: {
        person: {
          id: 7, revision: 1, display_name: 'Synthetic Person', participant_ids: [], vcard_uid: '',
          created_at: '2026-08-29T00:00:00Z', updated_at: '2026-08-29T00:00:00Z'
        },
        etags: {}, errors: {}
      }
    });

    const maintenance = await screen.findByRole('heading', { name: 'Profile maintenance' });
    const publication = await screen.findByRole('heading', { name: 'CardDAV publication' });
    expect(maintenance.compareDocumentPosition(publication) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getAllByRole('tab')).toHaveLength(5);
    expect(screen.queryByRole('tab', { name: /maintenance/i })).toBeNull();
  });

  it('mounts compact CardDAV publication in Overview and threads conflict and status callbacks', async () => {
    const onOpenCardDAVConflict = vi.fn();
    const onAnnounce = vi.fn();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      const maintenance = profileMaintenanceResponse(request);
      if (maintenance) return maintenance;
      if (path === '/api/v1/carddav/publications/7') return Response.json({
        person_id: 7,
        state: 'conflict',
        desired: true,
        conflict_id: 41,
        address_book: { id: 5, name: 'Synthetic contacts' }
      });
      if (path === '/api/v1/people/7/merges') return Response.json({ merges: [], limit: 100, offset: 0 });
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    render(PersonDetail, {
      client: createAPIClient(fetchFn),
      personID: 7,
      bundle: {
        person: {
          id: 7, revision: 1, display_name: 'Synthetic Person', participant_ids: [], vcard_uid: '',
          created_at: '2026-08-28T10:00:00Z', updated_at: '2026-08-28T10:00:00Z'
        },
        etags: {},
        errors: {}
      },
      onOpenCardDAVConflict,
      onAnnounce
    });

    expect(await screen.findByRole('heading', { name: 'CardDAV publication' })).toBeDefined();
    expect(screen.getAllByRole('tab')).toHaveLength(5);
    expect(screen.queryByRole('tab', { name: /CardDAV/i })).toBeNull();
    await fireEvent.click(await screen.findByRole('button', { name: 'Review CardDAV conflict 41' }));
    expect(onOpenCardDAVConflict).toHaveBeenCalledWith(41);
    expect(onAnnounce).not.toHaveBeenCalled();
  });

  it('renders unconfigured CardDAV publication as an optional Settings handoff without an operational alert', async () => {
    const onOpenCardDAVSettings = vi.fn();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      const maintenance = profileMaintenanceResponse(request);
      if (maintenance) return maintenance;
      if (path === '/api/v1/carddav/publications/7') {
        return Response.json({
          error: 'carddav_unavailable',
          message: 'synthetic private setup detail must not render'
        }, { status: 503 });
      }
      if (path === '/api/v1/people/7/merges') return Response.json({ merges: [], limit: 100, offset: 0 });
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    render(PersonDetail, {
      client: createAPIClient(fetchFn),
      personID: 7,
      bundle: { etags: {}, errors: {} },
      onOpenCardDAVSettings
    });

    expect(await screen.findByText('CardDAV publication is unavailable. Configure or repair it in CardDAV settings.')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Retry CardDAV publication' })).toBeNull();
    expect(document.body.textContent).not.toContain('synthetic private setup detail');
    await fireEvent.click(screen.getByRole('button', { name: 'Open CardDAV settings' }));
    expect(onOpenCardDAVSettings).toHaveBeenCalledOnce();
  });
});
