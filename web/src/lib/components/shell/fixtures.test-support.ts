import type { SavedView } from '../../api/generated/models';

export function view(id: number, name: string, presentation = 'table'): SavedView {
  return {
    id,
    name,
    revision: 1,
    schema_version: 2,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    canonical_state: { filters: [], grouping: [], presentation, sort: [], columns: [] },
  };
}

export const pair = { id: 17, left_id: 1, left_kind: 'participant', right_id: 2, right_kind: 'participant', basis: 'email', source: 'user', state: 'candidate', actionable: true, application_pending: false, review_token: 'review-17', evidence: [], created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z' };
