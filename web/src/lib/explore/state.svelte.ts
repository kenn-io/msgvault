import { SvelteSet } from 'svelte/reactivity';

import type {
  AllMatchingExploreSelection,
  ExploreColumn,
  ExploreFilter,
  ExploreGroupDimension,
  ExplorePredicate,
  ExploreScrollAnchor,
  ExploreSearchMode,
  ExploreSelection,
  ExploreSort,
  FileSearchSort,
  FileMIMEFamily,
  ExploreURLState,
  ExploreWorkspace,
  RelationshipFacet
} from './models';
import { DEFAULT_EXPLORE_COLUMNS } from './models';
import { isGroupingDimension, validateGroupingChain } from '../grouping/catalog';
import { hasValidSearchAuthority, predicateFingerprint } from './selection';
import { parseAttachmentSelection } from './attachment-authority';
import {
  availableSearchModeStorage,
  explicitSearchModeFromURL,
  rememberSearchMode,
  resolveInitialSearchMode,
  type SearchModeStorage
} from '../search/modes';

const STATE_PARAMETER = 'explore';
const FILTER_DIMENSIONS = new Set([
  'source',
  'participant',
  'domain',
  'message_type',
  'after',
  'before',
  'deletion'
]);
const COLUMNS = new Set(['kind', 'people', 'title', 'excerpt', 'time', 'attachments', 'size']);
const TRANSIENT_HISTORY_FIELDS = [
  'columns',
  'columnWidths',
  'activeRow',
  'scrollAnchor'
] as const satisfies ReadonlyArray<keyof ExploreURLState>;
const RESTORATION_INVALIDATING_FIELDS = new Set<keyof ExploreURLState>([
  'workspace',
  'query',
  'searchMode',
  'filters',
  'groupingChain',
  'presentation',
  'sort',
  'fileSort',
  'fileFilenameQuery',
  'fileMIMEFamilies',
  'identityQuery',
  'identitySort',
  'analysisTarget',
  'selectedIdentifier',
  'relationshipFacet',
  'relationshipTarget'
]);
const FILE_MIME_FAMILIES = new Set<FileMIMEFamily>([
  'image', 'pdf', 'audio', 'video', 'text', 'document', 'archive', 'other'
]);

export const defaultExploreURLState: ExploreURLState = {
  schemaVersion: 2,
  workspace: 'relationships',
  query: '',
  searchMode: 'full_text',
  filters: [],
  groupingChain: [],
  presentation: 'table',
  sort: [{ field: 'occurred_at', direction: 'desc' }],
  fileSort: { field: 'occurred_at', direction: 'desc' },
  fileFilenameQuery: '',
  fileMIMEFamilies: [],
  identityQuery: '',
  identitySort: { field: 'activity_count', direction: 'desc' },
  analysisTarget: null,
  selectedIdentifier: null,
  relationshipFacet: 'people',
  relationshipTarget: null,
  relationshipShowAll: false,
  relationshipFiles: false,
  columns: [...DEFAULT_EXPLORE_COLUMNS],
  columnWidths: {},
  activeRow: null,
  selectedRow: null,
  inspectorPinned: true,
  conversationAnchor: null,
  scrollAnchor: null
};

interface ExploreWindow {
  location: Pick<Location, 'href' | 'pathname' | 'search' | 'hash'>;
  history: Pick<History, 'pushState' | 'replaceState'>;
  addEventListener(type: 'popstate', listener: () => void): void;
  removeEventListener(type: 'popstate', listener: () => void): void;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function freshDefaults(): ExploreURLState {
  return {
    ...defaultExploreURLState,
    filters: defaultExploreURLState.filters.map((filter) => ({
      ...filter,
      values: [...filter.values]
    })),
    groupingChain: [...defaultExploreURLState.groupingChain],
    sort: defaultExploreURLState.sort.map((sort) => ({ ...sort })),
    fileSort: defaultExploreURLState.fileSort ? { ...defaultExploreURLState.fileSort } : undefined,
    fileMIMEFamilies: [...defaultExploreURLState.fileMIMEFamilies],
    columns: [...defaultExploreURLState.columns],
    columnWidths: { ...defaultExploreURLState.columnWidths }
  };
}

function isFilter(value: unknown): value is ExploreFilter {
  return (
    isRecord(value) &&
    typeof value.dimension === 'string' &&
    FILTER_DIMENSIONS.has(value.dimension) &&
    Array.isArray(value.values) &&
    value.values.every((item) => typeof item === 'string')
  );
}

function filters(value: unknown): ExploreFilter[] {
  return Array.isArray(value) && value.every(isFilter)
    ? value.map((filter) => ({ ...filter, values: [...filter.values] }))
    : [];
}

function groups(value: unknown): ExploreGroupDimension[] {
  return validateGroupingChain(value);
}

function columns(value: unknown): ExploreColumn[] {
  return Array.isArray(value) && value.every((item) => COLUMNS.has(String(item)))
    ? ([...value] as ExploreColumn[])
    : [...DEFAULT_EXPLORE_COLUMNS];
}

function sorts(value: unknown): ExploreSort[] {
  return Array.isArray(value) &&
    value.every(
      (item) =>
        isRecord(item) && item.field === 'occurred_at' && item.direction === 'desc'
    )
    ? (value.map((item) => ({ ...item })) as ExploreSort[])
    : defaultExploreURLState.sort.map((sort) => ({ ...sort }));
}

function fileSort(value: unknown): FileSearchSort {
  return isRecord(value) &&
    (value.field === 'occurred_at' || value.field === 'filename' || value.field === 'size') &&
    (value.direction === 'asc' || value.direction === 'desc')
    ? { field: value.field, direction: value.direction }
    : { field: 'occurred_at', direction: 'desc' };
}

function fileMIMEFamilies(value: unknown): FileMIMEFamily[] {
  return Array.isArray(value) && value.every((item) =>
    typeof item === 'string' && FILE_MIME_FAMILIES.has(item as FileMIMEFamily))
    ? [...new Set(value)] as FileMIMEFamily[]
    : [];
}

function widths(value: unknown): Partial<Record<ExploreColumn, number>> {
  if (!isRecord(value)) return {};
  const result: Partial<Record<ExploreColumn, number>> = {};
  for (const [key, width] of Object.entries(value)) {
    if (COLUMNS.has(key) && typeof width === 'number' && Number.isFinite(width) && width > 0) {
      result[key as ExploreColumn] = width;
    }
  }
  return result;
}

function scrollAnchor(value: unknown): ExploreScrollAnchor | null {
  if (value === null) return null;
  return isRecord(value) && typeof value.key === 'string' && typeof value.offset === 'number'
    ? { key: value.key, offset: value.offset }
    : null;
}

function selectedRow(value: unknown): string | null {
  if (value === null) return null;
  if (typeof value !== 'string') return null;
  if (!value.startsWith('attachment:')) return value;
  return parseAttachmentSelection(value) === undefined ? null : value;
}

function relationshipTargetValue(value: unknown): string | null {
  return typeof value === 'string' &&
    (/^cluster:\d+$/.test(value) || /^domain:\S+$/.test(value))
    ? value
    : null;
}

function legacyRelationshipTarget(
  analysisTarget: string | null,
  facet: RelationshipFacet
): string | null {
  if (analysisTarget === null) return null;
  if (facet === 'people' && analysisTarget.startsWith('person:')) {
    return relationshipTargetValue(`cluster:${analysisTarget.slice('person:'.length)}`);
  }
  if (facet === 'domains' && analysisTarget.startsWith('domain:')) {
    return relationshipTargetValue(analysisTarget);
  }
  return null;
}

function normalize(value: unknown): ExploreURLState {
  if (!isRecord(value)) return freshDefaults();
  const { selection: _selection, bulkSelection: _bulkSelection, ...knownAndFuture } = value;
  const searchMode =
    value.searchMode === 'full_text' ||
    value.searchMode === 'semantic' ||
    value.searchMode === 'hybrid'
      ? value.searchMode
      : defaultExploreURLState.searchMode;
  const presentation =
    value.presentation === 'table' ||
    value.presentation === 'timeline' ||
    value.presentation === 'files'
      ? value.presentation
      : defaultExploreURLState.presentation;
  const legacyFacet: RelationshipFacet | undefined =
    value.workspace === 'people' ? 'people' : value.workspace === 'domains' ? 'domains' : undefined;
  const workspace = value.workspace === 'everything' || value.workspace === 'settings' ||
    value.workspace === 'files' || value.workspace === 'relationships' ||
    value.workspace === 'saved_views' || value.workspace === 'sources' ||
    value.workspace === 'deletions'
    ? value.workspace
    : 'relationships';
  const analysisTarget = typeof value.analysisTarget === 'string' &&
    (/^person:[1-9][0-9]*$/.test(value.analysisTarget) || /^domain:[a-z0-9.-]+$/.test(value.analysisTarget))
    ? value.analysisTarget : null;
  const relationshipFacet: RelationshipFacet = legacyFacet ??
    (value.relationshipFacet === 'people' || value.relationshipFacet === 'domains'
      ? value.relationshipFacet
      : 'people');
  const relationshipTarget = legacyFacet
    ? legacyRelationshipTarget(analysisTarget, legacyFacet)
    : relationshipTargetValue(value.relationshipTarget);

  return {
    ...knownAndFuture,
    schemaVersion: value.schemaVersion === 1
      ? defaultExploreURLState.schemaVersion
      : typeof value.schemaVersion === 'number' && Number.isSafeInteger(value.schemaVersion)
        ? value.schemaVersion
        : defaultExploreURLState.schemaVersion,
    workspace,
    query: typeof value.query === 'string' ? value.query : '',
    searchMode,
    filters: filters(value.filters),
    groupingChain: groups(value.groupingChain),
    presentation,
    sort: sorts(value.sort),
    fileSort: fileSort(value.fileSort),
    fileFilenameQuery: value.schemaVersion === 2 && typeof value.fileFilenameQuery === 'string'
      ? value.fileFilenameQuery
      : '',
    fileMIMEFamilies: value.schemaVersion === 2 ? fileMIMEFamilies(value.fileMIMEFamilies) : [],
    identityQuery: typeof value.identityQuery === 'string' ? value.identityQuery : '',
    identitySort: isRecord(value.identitySort) &&
      (value.identitySort.field === 'activity_count' || value.identitySort.field === 'latest_at' || value.identitySort.field === 'display_label') &&
      (value.identitySort.direction === 'asc' || value.identitySort.direction === 'desc')
      ? { field: value.identitySort.field, direction: value.identitySort.direction }
      : { field: 'activity_count', direction: 'desc' },
    analysisTarget,
    selectedIdentifier: typeof value.selectedIdentifier === 'string' ? value.selectedIdentifier : null,
    relationshipFacet,
    relationshipTarget,
    relationshipShowAll: value.relationshipShowAll === true,
    relationshipFiles: value.relationshipFiles === true,
    columns: columns(value.columns),
    columnWidths: widths(value.columnWidths),
    activeRow:
      typeof value.activeRow === 'string' || value.activeRow === null
        ? value.activeRow
        : null,
    selectedRow: selectedRow(value.selectedRow),
    inspectorPinned: true,
    conversationAnchor:
      typeof value.conversationAnchor === 'string' || value.conversationAnchor === null
        ? value.conversationAnchor
        : null,
    scrollAnchor: scrollAnchor(value.scrollAnchor)
  } as ExploreURLState;
}

export function serializeExploreURLState(state: ExploreURLState, baseSearch = ''): string {
  const parameters = new URLSearchParams(baseSearch.startsWith('?') ? baseSearch.slice(1) : baseSearch);
  parameters.set(STATE_PARAMETER, JSON.stringify(normalize(state)));
  return `?${parameters.toString()}`;
}

export function parseExploreURLState(search: string): ExploreURLState {
  const parameters = new URLSearchParams(search.startsWith('?') ? search.slice(1) : search);
  const encoded = parameters.get(STATE_PARAMETER);
  if (encoded === null) return freshDefaults();
  try {
    return normalize(JSON.parse(encoded));
  } catch {
    return freshDefaults();
  }
}

export class ExploreState {
  current = $state<ExploreURLState>(freshDefaults());
  restorationEpoch = $state(1);
  private readonly browser: ExploreWindow;
  private readonly preferenceStorage: SearchModeStorage | null;
  private committed: ExploreURLState;
  private pendingRestorationEpoch = $state<number | undefined>(1);
  private pendingSearchPriorFocus?: Pick<ExploreURLState, 'activeRow' | 'scrollAnchor'>;
  private readonly handlePopState = (): void => {
    this.current = this.readURLState();
    this.committed = normalize(this.current);
    this.pendingSearchPriorFocus = undefined;
    this.restorationEpoch += 1;
    this.pendingRestorationEpoch = this.restorationEpoch;
  };

  constructor(
    browser: ExploreWindow = window,
    preferenceStorage: SearchModeStorage | null = browser === globalThis.window
      ? availableSearchModeStorage()
      : null
  ) {
    this.browser = browser;
    this.preferenceStorage = preferenceStorage;
    this.current = this.readURLState();
    this.committed = normalize(this.current);
    browser.addEventListener('popstate', this.handlePopState);
  }

  replaceTransient(patch: Partial<ExploreURLState>): void {
    this.navigate(patch, 'replace');
  }

  replaceCommittedNavigation(patch: Partial<ExploreURLState>): void {
    this.navigate(patch, 'replace');
    this.committed = normalize(this.current);
    this.pendingSearchPriorFocus = undefined;
  }

  replaceCommittedDraft(patch: Partial<ExploreURLState>): void {
    const priorCommitted = this.committed;
    this.navigate(patch, 'replace');
    this.committed = normalize({ ...priorCommitted, ...patch });
  }

  peekRestorationEpoch(): number | undefined {
    return this.pendingRestorationEpoch;
  }

  acknowledgeRestoration(epoch: number): void {
    if (this.pendingRestorationEpoch === epoch) this.pendingRestorationEpoch = undefined;
  }

  replaceSearchDraft(query: string, searchMode: ExploreSearchMode): void {
	rememberSearchMode(searchMode, this.preferenceStorage);
    this.pendingSearchPriorFocus ??= {
      activeRow: this.current.activeRow,
      scrollAnchor: this.current.scrollAnchor
        ? { ...this.current.scrollAnchor }
        : null
    };
    this.navigate({ query, searchMode, activeRow: null, scrollAnchor: null }, 'replace');
  }

  commitSearch(query: string, searchMode: ExploreSearchMode): void {
	rememberSearchMode(searchMode, this.preferenceStorage);
    this.navigate({ query, searchMode, selectedRow: null, conversationAnchor: null, activeRow: null, scrollAnchor: null }, 'push');
  }

  commitWorkspace(workspace: ExploreWorkspace): void {
    this.navigate({
      workspace,
      analysisTarget: null,
      selectedIdentifier: null,
      activeRow: null,
      selectedRow: null,
      conversationAnchor: null,
      scrollAnchor: null
    }, 'push');
  }

  commitNavigation(patch: Partial<ExploreURLState>): void {
    const selectionChanged = 'selectedRow' in patch && patch.selectedRow !== this.current.selectedRow;
    this.navigate(selectionChanged && !('conversationAnchor' in patch)
      ? { ...patch, conversationAnchor: null }
      : patch, 'push');
  }

  commitRestorableNavigation(patch: Partial<ExploreURLState>): void {
    this.navigate(patch, 'push');
    this.restorationEpoch += 1;
    this.pendingRestorationEpoch = this.restorationEpoch;
  }

  replaceCommittedRestorableNavigation(patch: Partial<ExploreURLState>): void {
    this.navigate(patch, 'replace');
    this.committed = normalize(this.current);
    this.pendingSearchPriorFocus = undefined;
    this.restorationEpoch += 1;
    this.pendingRestorationEpoch = this.restorationEpoch;
  }

  commitGrouping(dimension: ExploreGroupDimension): void {
    if (!isGroupingDimension(dimension) || this.current.groupingChain.includes(dimension)) return;
    this.navigate({
      groupingChain: [...this.current.groupingChain, dimension],
      activeRow: null,
      scrollAnchor: null
    }, 'push');
  }

  commitUngroup(): void {
    if (this.current.groupingChain.length === 0) return;
    this.navigate({
      groupingChain: this.current.groupingChain.slice(0, -1),
      activeRow: null,
      scrollAnchor: null
    }, 'push');
  }

  predicate(): ExplorePredicate {
    const query = this.current.query.trim();
    return {
      ...(query ? { query, search_mode: this.current.searchMode } : {}),
      filters: this.current.filters,
      grouping: this.current.groupingChain,
      presentation: this.current.presentation,
      sort: this.current.sort,
      limit: 500
    };
  }

  destroy(): void {
    this.browser.removeEventListener('popstate', this.handlePopState);
  }

  private readURLState(): ExploreURLState {
    const parsed = parseExploreURLState(this.browser.location.search);
    parsed.searchMode = resolveInitialSearchMode(
      explicitSearchModeFromURL(this.browser.location.search),
      this.preferenceStorage
    );
    return parsed;
  }

  private navigate(
    patch: Partial<ExploreURLState>,
    mode: 'push' | 'replace'
  ): void {
    if (
      mode === 'push' ||
      Object.keys(patch).some((key) => RESTORATION_INVALIDATING_FIELDS.has(key as keyof ExploreURLState))
    ) {
      this.pendingRestorationEpoch = undefined;
    }
    const baseSearch = this.browser.location.search;
    if (mode === 'push') {
      const priorFocus = this.pendingSearchPriorFocus;
      const transient = Object.fromEntries(
        TRANSIENT_HISTORY_FIELDS
          .filter((key) => !priorFocus || (key !== 'activeRow' && key !== 'scrollAnchor'))
          .map((key) => [key, this.current[key]])
      ) as Partial<ExploreURLState>;
      const priorEntry = normalize({ ...this.committed, ...transient, ...priorFocus });
      const committedURL = `${this.browser.location.pathname}${serializeExploreURLState(priorEntry, baseSearch)}${this.browser.location.hash}`;
      this.browser.history.replaceState(null, '', committedURL);
    }
    const next = normalize({ ...this.current, ...patch });
    // Preserve per-field reactivity: transient scroll/column changes must not
    // invalidate consumers that only read the canonical server predicate.
    for (const key of Object.keys(patch)) {
      if (key in next) this.current[key] = next[key];
    }
    const url = `${this.browser.location.pathname}${serializeExploreURLState(this.current, baseSearch)}${this.browser.location.hash}`;
    if (mode === 'push') {
      this.browser.history.pushState(null, '', url);
      this.committed = normalize(this.current);
      this.pendingSearchPriorFocus = undefined;
    } else this.browser.history.replaceState(null, '', url);
  }
}

export class ExploreSelectionState {
  mode = $state<'explicit' | 'all_matching'>('explicit');
  readonly explicitKeys = new SvelteSet<string>();
  readonly exclusions = new SvelteSet<string>();
  private allMatching?: Omit<AllMatchingExploreSelection, 'exclusions'>;
  private rangeAnchorKey: string | undefined;

  get count(): number {
    return this.mode === 'explicit' ? this.explicitKeys.size : 0;
  }

  isSelected(key: string): boolean {
    return this.mode === 'explicit' ? this.explicitKeys.has(key) : !this.exclusions.has(key);
  }

  selectedKeys(keys: string[]): string[] {
    return keys.filter((key) => this.isSelected(key));
  }

  toggle(key: string, index: number, orderedKeys: string[], range = false): void {
    if (this.mode === 'all_matching') {
      if (this.exclusions.has(key)) this.exclusions.delete(key);
      else this.exclusions.add(key);
      return;
    }
    const rangeAnchor = this.rangeAnchorKey ? orderedKeys.indexOf(this.rangeAnchorKey) : -1;
    if (range && rangeAnchor >= 0) {
      const start = Math.min(rangeAnchor, index);
      const end = Math.max(rangeAnchor, index);
      for (let cursor = start; cursor <= end; cursor += 1) {
        const next = orderedKeys[cursor];
        if (next !== undefined) this.explicitKeys.add(next);
      }
      return;
    }
    if (this.explicitKeys.has(key)) this.explicitKeys.delete(key);
    else this.explicitKeys.add(key);
    this.rangeAnchorKey = key;
  }

  selectVisible(keys: string[]): void {
    if (this.mode === 'all_matching') {
      for (const key of keys) this.exclusions.delete(key);
      return;
    }
    for (const key of keys) this.explicitKeys.add(key);
    if (keys.length > 0) this.rangeAnchorKey = keys[0];
  }

  selectAllMatching(selection: AllMatchingExploreSelection): void {
    if (selection.predicateFingerprint !== predicateFingerprint(selection.predicate)) {
      throw new Error('All-matching selection predicate fingerprint does not match');
    }
    if (selection.resultGeneration < 1) {
      throw new Error('All-matching selection requires a result generation');
    }
    if (
      (selection.predicate.search_mode === 'semantic' ||
        selection.predicate.search_mode === 'hybrid') &&
      !selection.candidateSnapshotId
    ) {
      throw new Error('Semantic all-matching selection requires a server candidate snapshot');
    }
    if (!hasValidSearchAuthority(
      selection.predicate,
      selection.searchProvenance,
      selection.candidateSnapshotId
    )) {
      throw new Error('All-matching selection search provenance does not match its mode');
    }
    this.clear();
    this.mode = 'all_matching';
    const { exclusions, ...pinned } = selection;
    this.allMatching = pinned;
    for (const key of exclusions) this.exclusions.add(key);
  }

  clear(): void {
    this.mode = 'explicit';
    this.explicitKeys.clear();
    this.exclusions.clear();
    this.allMatching = undefined;
    this.rangeAnchorKey = undefined;
  }

  snapshot(): ExploreSelection {
    if (this.mode === 'all_matching' && this.allMatching) {
      return { ...this.allMatching, exclusions: [...this.exclusions] };
    }
    return { mode: 'explicit', rowKeys: [...this.explicitKeys] };
  }
}
