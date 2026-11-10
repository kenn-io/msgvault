import { candidate } from '../../directory/entity-fixtures.test-support';
import type { SavedView } from '../../api/generated/models';

export function view(id: number, name: string, presentation = 'table'): SavedView {
  return {
    id,
    name,
    revision: 1,
    include_count: 0,
    exclude_count: 0,
    stale_item_count: 0,
    redacted_item_count: 0,
    schema_version: 2,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    canonical_state: { filters: [], grouping: [], presentation, sort: [], columns: [] },
  };
}

export const pair = { ...candidate(17), left_id: 1, right_id: 2, review_token: 'review-17' };
