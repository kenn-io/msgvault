import type { SavedViewStateEnvelope } from '../api/generated/models';
import {
  filterDimensionLabel,
  groupedByLabel,
  presentationLabel,
  searchModeLabel
} from '../explore/labels';
import {
  DEFAULT_EXPLORE_COLUMNS,
  type ExploreFilterDimension,
  type ExploreURLState
} from '../explore/models';

export type CanonicalState = SavedViewStateEnvelope;
export const CURRENT_SCHEMA_VERSION = 1;

// Saved views written before the v2 filter names still use these fields.
const LEGACY_FILTER_FIELDS: Record<string, ExploreFilterDimension> = {
  source_id: 'source',
  participant_id: 'participant'
};

export function canonicalSavedViewState(state: ExploreURLState): CanonicalState {
  const query = state.query.trim();
  return {
    ...(query ? { query, search_mode: state.searchMode } : {}),
    filters: state.filters.map((filter) => ({
      field: filter.dimension,
      operator: 'in',
      values: [...filter.values]
    })),
    grouping: [...state.groupingChain],
    presentation: state.presentation,
    sort: state.sort.map((sort) => ({ field: sort.field, direction: sort.direction })),
    columns: [...state.columns]
  };
}

function filterDimension(field: string): ExploreFilterDimension {
  return LEGACY_FILTER_FIELDS[field] ?? (field as ExploreFilterDimension);
}

function searchMode(saved: CanonicalState): ExploreURLState['searchMode'] {
  return saved.search_mode === 'semantic' || saved.search_mode === 'hybrid'
    ? saved.search_mode
    : 'full_text';
}

export function exploreStateFromSavedView(saved: CanonicalState): Partial<ExploreURLState> {
  const presentation = saved.presentation ?? 'table';
  return {
    workspace: presentation === 'files' ? 'files' : 'everything',
    query: saved.query ?? '',
    searchMode: searchMode(saved),
    filters: (saved.filters ?? []).map((filter) => ({
      dimension: filterDimension(filter.field),
      values: [...filter.values]
    })),
    groupingChain: [...(saved.grouping ?? [])] as ExploreURLState['groupingChain'],
    presentation,
    sort: (saved.sort ?? [{ field: 'occurred_at', direction: 'desc' }]) as ExploreURLState['sort'],
    columns: (saved.columns ?? DEFAULT_EXPLORE_COLUMNS) as ExploreURLState['columns'],
    activeRow: null,
    selectedRow: null,
    conversationAnchor: null,
    scrollAnchor: null
  };
}

export function savedViewSummary(saved: CanonicalState): string[] {
  const parts: string[] = [];
  if (saved.query) parts.push(`${searchModeLabel(searchMode(saved))}: “${saved.query}”`);
  for (const filter of saved.filters ?? []) {
    const label = filterDimensionLabel(filterDimension(filter.field));
    parts.push(`${label}: ${filter.values.join(', ')}`);
  }
  if (saved.grouping?.length) parts.push(groupedByLabel(saved.grouping));
  parts.push(presentationLabel(saved.presentation ?? 'table'));
  return parts;
}
