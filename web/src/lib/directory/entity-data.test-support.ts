import type { RelationshipType } from './models';
import type { DirectoryPersonSummary } from '../api/generated/models/directoryPersonSummary';
import { person } from './person-fixtures.test-support';
export { person } from './person-fixtures.test-support';

export function organization(id = 21, revision = 1, name = 'Synthetic Org') {
  return {
    id, revision, name, kind: 'company', primary_domain: 'synthetic.example', description: 'A synthetic organization.',
    created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z'
  };
}

export function employment(id = 11, revision = 1, overrides: Record<string, unknown> = {}) {
  return {
    id, revision, person_id: 7, organization_id: 21, source: 'user', is_current: true, is_primary: false,
    title: 'Engineer', created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', ...overrides
  };
}

export function directoryPerson(id: number, overrides: Partial<DirectoryPersonSummary> = {}): DirectoryPersonSummary {
  const detail = person(id);
  return { id, revision: detail.revision, display_name: detail.display_name, categories: [], organizations: [], carddav_bindings: [], contact_state: 'active', vcard_uid: detail.vcard_uid, ...overrides };
}

export function relationshipType(overrides: Partial<RelationshipType> = {}): RelationshipType {
  return {
    id: 31, revision: 1, slug: 'mentor', forward_label: 'mentors', reverse_label: 'is mentored by',
    is_symmetric: false, is_canonical: true, is_deletable: true, ownership: 'user', universal_id: 'relationship-type-31',
    created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', ...overrides
  };
}

export function relationship(id = 41, revision = 1, overrides: Record<string, unknown> = {}) {
  return {
    id, revision, relationship_type_id: 31, type_slug: 'mentor', source_person_id: 7, target_person_id: 8,
    forward_label: 'mentors', reverse_label: 'is mentored by', is_symmetric: false, status: 'active', source: 'user',
    created_by: 'user', updated_by: 'user', vcard_identity: {},
    created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z', ...overrides
  };
}
