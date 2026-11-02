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
