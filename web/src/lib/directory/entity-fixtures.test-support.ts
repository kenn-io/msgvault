import { organization, employment } from './entity-data.test-support';
export { organization, employment, directoryPerson, relationshipType } from './entity-data.test-support';
import { createAPIClient } from '../api/client';
import { DirectoryEntityController } from './entity-controller.svelte';

export { person, mergeResult } from './person-fixtures.test-support';

export function requestOf(input: RequestInfo | URL): Request {
  return input instanceof Request ? input : new Request(input);
}

export function pathOf(request: Request): string {
  return new URL(request.url, document.baseURI).pathname;
}

export function organizationProfile(revision = 1, name = 'Synthetic Org') {
  return {
    organization: organization(21, revision, name),
    names: [{ organization_id: 21, name_kind: 'alias', name: 'Synthetic Works', name_normalized: 'synthetic works', envelope: envelope(31) }],
    contact_points: [], addresses: [], categories: [], identifiers: [],
    media: [{ organization_id: 21, media_kind: 'logo', original_value: 'Existing logo', content_hash: 'sha256:synthetic', has_data: true, envelope: envelope(32) }]
  };
}

export function envelope(id: number, overrides: Record<string, unknown> = {}) {
  return { id, ordinal: 0, source: 'user', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', vcard: {}, ...overrides };
}

export function richOrganizationProfile(revision = 3) {
  const importedEnvelope = envelope(40, {
    ordinal: 7, pref: 2, type_label: 'work', type_tokens: ['WORK', 'INTERNET'], source: 'vcard_import',
    source_ref: 'card-42', source_resource_uid: 'resource-card-42', active_from: '2025-02-03T04:05:06Z',
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



export function collection(rows = [employment()], projection = { employment_id: 11, organization_id: 21, organization_name: 'Synthetic Org', title: 'Engineer', vcard: {} }) {
  return Response.json({ employments: rows, projection });
}

export function controllerWith(fetchFn: typeof fetch, personID = 7, options: { onDirectoryChange?: () => void | Promise<void> } = {}) {
  const client = createAPIClient(fetchFn);
  return { client, controller: new DirectoryEntityController(client, personID, options) };
}



export function candidate(id: number, state = 'candidate') {
  return { id, left_id: id * 10, left_kind: 'participant', right_id: id * 10 + 1, right_kind: 'participant', basis: 'email', source: 'user', state, actionable: state === 'candidate' || state === 'conflict', application_pending: false, review_token: `token-${id}-${state}`, evidence: [], created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z' };
}



export function relationship(id = 41, revision = 1, overrides: Record<string, unknown> = {}) {
  return {
    id, revision, relationship_type_id: 31, type_slug: 'mentor', source_person_id: 7, target_person_id: 8,
    forward_label: 'mentors', reverse_label: 'is mentored by', is_symmetric: false, status: 'active', source: 'user',
    created_by: 'user', updated_by: 'user', vcard_identity: {},
    created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', ...overrides
  };
}
