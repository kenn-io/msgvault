import type { SavedViewStateEnvelope } from '../api/generated/models';
import {
  filterDimensionLabel,
  filterValueLabel,
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
export const CURRENT_SCHEMA_VERSION = 2;

function trimGoWhitespace(query: string): string {
  return query.replace(/^\p{White_Space}+|\p{White_Space}+$/gu, '');
}

// Saved views written before the v2 filter names still use these fields.
const LEGACY_FILTER_FIELDS: Record<string, ExploreFilterDimension> = {
  source_id: 'source',
  participant_id: 'participant'
};

export function canonicalSavedViewState(state: ExploreURLState): CanonicalState {
  const query = trimGoWhitespace(state.query);
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

// Keep trimming and pipe boundaries in step with internal/savedview/service.go and search.IsStandalonePipe.
function quoteLegacyPipes(query: string): string {
  query = trimGoWhitespace(query);
  let result = '';
  let quote = '';
  let escaped = false;
  for (let i = 0; i < query.length; i++) {
    const char = query[i]!;
    if (quote) {
      if (escaped) escaped = false;
      else if (char === '\\') escaped = true;
      else if (char === quote) quote = '';
    } else if (char === '"' || char === "'") {
      quote = char;
    } else if (char === '|' && (i === 0 || query[i - 1] === ' ') && (i + 1 === query.length || query[i + 1] === ' ')) {
      result += '"|"';
      continue;
    }
    result += char;
  }
  return result;
}

export function exploreStateFromSavedView(saved: CanonicalState, schemaVersion = CURRENT_SCHEMA_VERSION): Partial<ExploreURLState> {
  const presentation = saved.presentation ?? 'table';
  const mode = searchMode(saved);
  const query = saved.query ?? '';
  return {
    workspace: presentation === 'files' ? 'files' : 'everything',
    query: schemaVersion === 1 && mode === 'full_text' ? quoteLegacyPipes(query) : query,
    searchMode: mode,
    filters: (saved.filters ?? []).map((filter) => ({
      dimension: filterDimension(filter.field),
      values: [...filter.values]
    })),
    groupingChain: [...(saved.grouping ?? [])] as ExploreURLState['groupingChain'],
    presentation,
    sort: (saved.sort ?? [{ field: 'occurred_at', direction: 'desc' }]) as ExploreURLState['sort'],
    columns: (saved.columns ?? DEFAULT_EXPLORE_COLUMNS) as ExploreURLState['columns'],
    // Saved views never store these Files-only settings, so opening one resets them.
    fileSort: { field: 'occurred_at', direction: 'desc' },
    fileFilenameQuery: '',
    fileMIMEFamilies: [],
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
    const dimension = filterDimension(filter.field);
    const values = filter.values.map((value) => filterValueLabel(dimension, value));
    parts.push(`${filterDimensionLabel(dimension)}: ${values.join(', ')}`);
  }
  if (saved.grouping?.length) parts.push(groupedByLabel(saved.grouping));
  parts.push(presentationLabel(saved.presentation ?? 'table'));
  return parts;
}
