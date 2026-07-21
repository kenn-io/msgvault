import type {
  ArchiveFilter,
  ArchivePresentation,
  ArchiveSearchMode,
  ArchiveSort,
  ArchiveURLState
} from './types';

const STATE_PARAMETER = 'state';

export const defaultArchiveURLState: ArchiveURLState = {
  schemaVersion: 1,
  query: '',
  searchMode: 'fts',
  filters: [],
  groupingChain: [],
  presentation: 'table',
  sort: [],
  columns: ['kind', 'people', 'subject', 'excerpt', 'time'],
  selectedRow: null,
  inspectorPinned: false,
  conversationAnchor: null,
  scrollKey: null
};

function freshDefaultArchiveURLState(): ArchiveURLState {
  return {
    ...defaultArchiveURLState,
    filters: defaultArchiveURLState.filters.map((filter) => ({
      ...filter,
      value: Array.isArray(filter.value) ? [...filter.value] : filter.value
    })),
    groupingChain: [...defaultArchiveURLState.groupingChain],
    sort: defaultArchiveURLState.sort.map((sort) => ({ ...sort })),
    columns: [...defaultArchiveURLState.columns]
  };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isSearchMode(value: unknown): value is ArchiveSearchMode {
  return value === 'fts' || value === 'vector' || value === 'hybrid';
}

function isPresentation(value: unknown): value is ArchivePresentation {
  return value === 'table' || value === 'timeline' || value === 'files';
}

function stringOrNull(value: unknown, fallback: string | null): string | null {
  return typeof value === 'string' || value === null ? value : fallback;
}

function stringArray(value: unknown, fallback: string[]): string[] {
  return Array.isArray(value) && value.every((item) => typeof item === 'string')
    ? [...value]
    : [...fallback];
}

function isFilterValue(value: unknown): boolean {
  return (
    typeof value === 'string' ||
    typeof value === 'number' ||
    typeof value === 'boolean' ||
    (Array.isArray(value) && value.every((item) => typeof item === 'string'))
  );
}

function isFilter(value: unknown): value is ArchiveFilter {
  return (
    isRecord(value) &&
    typeof value.field === 'string' &&
    typeof value.operator === 'string' &&
    isFilterValue(value.value)
  );
}

function filterArray(value: unknown): ArchiveFilter[] {
  return Array.isArray(value) && value.every(isFilter) ? value : [];
}

function isSort(value: unknown): value is ArchiveSort {
  return (
    isRecord(value) &&
    typeof value.field === 'string' &&
    (value.direction === 'asc' || value.direction === 'desc')
  );
}

function sortArray(value: unknown): ArchiveSort[] {
  return Array.isArray(value) && value.every(isSort) ? value : [];
}

function normalizeArchiveURLState(value: unknown): ArchiveURLState {
  if (!isRecord(value)) return freshDefaultArchiveURLState();

  // Session-owned bulk selection is intentionally discarded even if an old or
  // future producer writes it into the state envelope.
  const { bulkSelection: _bulkSelection, ...futureAndKnownFields } = value;

  return {
    ...futureAndKnownFields,
    schemaVersion:
      typeof value.schemaVersion === 'number' && Number.isSafeInteger(value.schemaVersion)
        ? value.schemaVersion
        : defaultArchiveURLState.schemaVersion,
    query: typeof value.query === 'string' ? value.query : defaultArchiveURLState.query,
    searchMode: isSearchMode(value.searchMode)
      ? value.searchMode
      : defaultArchiveURLState.searchMode,
    filters: filterArray(value.filters),
    groupingChain: stringArray(value.groupingChain, defaultArchiveURLState.groupingChain),
    presentation: isPresentation(value.presentation)
      ? value.presentation
      : defaultArchiveURLState.presentation,
    sort: sortArray(value.sort),
    columns: stringArray(value.columns, defaultArchiveURLState.columns),
    selectedRow: stringOrNull(value.selectedRow, defaultArchiveURLState.selectedRow),
    inspectorPinned:
      typeof value.inspectorPinned === 'boolean'
        ? value.inspectorPinned
        : defaultArchiveURLState.inspectorPinned,
    conversationAnchor: stringOrNull(
      value.conversationAnchor,
      defaultArchiveURLState.conversationAnchor
    ),
    scrollKey: stringOrNull(value.scrollKey, defaultArchiveURLState.scrollKey)
  };
}

export function serializeArchiveURLState(state: ArchiveURLState): string {
  const parameters = new URLSearchParams();
  parameters.set(STATE_PARAMETER, JSON.stringify(normalizeArchiveURLState(state)));
  return `?${parameters.toString()}`;
}

export function parseArchiveURLState(search: string): ArchiveURLState {
  const parameters = new URLSearchParams(search.startsWith('?') ? search.slice(1) : search);
  const encoded = parameters.get(STATE_PARAMETER);
  if (encoded === null) return freshDefaultArchiveURLState();

  try {
    return normalizeArchiveURLState(JSON.parse(encoded));
  } catch {
    return freshDefaultArchiveURLState();
  }
}
