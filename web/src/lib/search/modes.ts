import type { ExploreSearchMode } from '../explore/models';
import type { components } from '../api/generated/schema';

export type SearchCoverageValue = components['schemas']['SearchCoverageResponse'];
export type SearchCoverageStatus = SearchCoverageValue['status'];
export type SearchCoverageAction = SearchCoverageValue['actions'][number];

const SEARCH_COVERAGE_STATUSES = new Set<SearchCoverageStatus>([
  'disabled', 'initializing', 'stale', 'incomplete', 'unavailable', 'ready'
]);
const SEARCH_COVERAGE_ACTIONS = new Set<SearchCoverageAction>(['retry', 'build_index']);

export function parseSearchCoverage(value: unknown): SearchCoverageValue | undefined {
  if (typeof value !== 'object' || value === null) return undefined;
  const candidate = value as Partial<SearchCoverageValue>;
  if (typeof candidate.status !== 'string' ||
    !SEARCH_COVERAGE_STATUSES.has(candidate.status as SearchCoverageStatus) ||
    typeof candidate.eligible_count !== 'number' || !Number.isInteger(candidate.eligible_count) ||
    typeof candidate.embedded_count !== 'number' || !Number.isInteger(candidate.embedded_count) ||
    typeof candidate.percentage !== 'number' || !Number.isFinite(candidate.percentage) ||
    typeof candidate.cache_revision !== 'string' || !Array.isArray(candidate.actions) ||
    !candidate.actions.every((action) => typeof action === 'string' &&
      SEARCH_COVERAGE_ACTIONS.has(action as SearchCoverageAction)) ||
    (candidate.vector_generation !== undefined &&
      (typeof candidate.vector_generation !== 'number' || !Number.isInteger(candidate.vector_generation))) ||
    (candidate.vector_fingerprint !== undefined && typeof candidate.vector_fingerprint !== 'string') ||
    (candidate.detail !== undefined && typeof candidate.detail !== 'string')) return undefined;
  return candidate as SearchCoverageValue;
}

export type SearchModeStorage = Pick<Storage, 'getItem' | 'setItem'>;

export const SEARCH_MODE_PREFERENCE_KEY = 'msgvault-search-mode';
export const SEARCH_MODES: readonly ExploreSearchMode[] = ['full_text', 'semantic', 'hybrid'];

export function availableSearchModeStorage(): SearchModeStorage | null {
  try {
    return typeof localStorage === 'undefined' ? null : localStorage;
  } catch {
    return null;
  }
}

export function loadRememberedSearchMode(storage: SearchModeStorage | null): ExploreSearchMode {
  try {
    const value = storage?.getItem(SEARCH_MODE_PREFERENCE_KEY);
    return SEARCH_MODES.includes(value as ExploreSearchMode)
      ? value as ExploreSearchMode
      : 'full_text';
  } catch {
    return 'full_text';
  }
}

export function rememberSearchMode(mode: ExploreSearchMode, storage: SearchModeStorage | null): void {
  try {
    storage?.setItem(SEARCH_MODE_PREFERENCE_KEY, mode);
  } catch {
    // Storage may be disabled. URL state remains authoritative for this view.
  }
}

export function resolveInitialSearchMode(
  explicitURLMode: ExploreSearchMode | undefined,
  storage: SearchModeStorage | null
): ExploreSearchMode {
  return explicitURLMode ?? loadRememberedSearchMode(storage);
}

export function explicitSearchModeFromURL(search: string, parameter = 'explore'): ExploreSearchMode | undefined {
  const encoded = new URLSearchParams(search.startsWith('?') ? search.slice(1) : search).get(parameter);
  if (encoded === null) return undefined;
  try {
    const value = JSON.parse(encoded) as { searchMode?: unknown };
    return SEARCH_MODES.includes(value.searchMode as ExploreSearchMode)
      ? value.searchMode as ExploreSearchMode
      : undefined;
  } catch {
    return undefined;
  }
}

interface LexicalCountKeyInput {
  query: string;
  canonicalQueryHash?: string;
  cacheRevision: string;
  lexicalRevision: string;
  rowKeys: string[];
  predicateFingerprint?: string;
}

interface LexicalCountEntry {
  counts: Record<string, number>;
  revision: string;
}

function canonicalQuery(query: string): string {
  const tokens = query.trim().match(/"(?:\\.|[^"\\])*"|\S+/g) ?? [];
  return tokens.map((token) => token.replace(/\s+/g, ' ')).sort().join(' ');
}

export class VisibleLexicalCountCache {
  private readonly entries = new Map<string, LexicalCountEntry>();
  private activeRevision = '';

  constructor(private readonly maximum = 128) {}

  get size(): number { return this.entries.size; }

  key(input: LexicalCountKeyInput): string {
    return JSON.stringify([
      input.canonicalQueryHash ?? canonicalQuery(input.query),
      input.cacheRevision,
      input.lexicalRevision,
      input.predicateFingerprint ?? '',
      [...new Set(input.rowKeys)].sort()
    ]);
  }

  get(key: string): Record<string, number> | undefined {
    const entry = this.entries.get(key);
    if (!entry) return undefined;
    this.entries.delete(key);
    this.entries.set(key, entry);
    return { ...entry.counts };
  }

  set(key: string, counts: Record<string, number>, revision = this.activeRevision): void {
    if (this.entries.has(key)) this.entries.delete(key);
    while (this.entries.size >= this.maximum) {
      const oldest = this.entries.keys().next().value as string | undefined;
      if (oldest === undefined) break;
      this.entries.delete(oldest);
    }
    this.entries.set(key, { counts: { ...counts }, revision });
  }

  invalidateRevision(revision: string): void {
    if (this.activeRevision === revision) return;
    this.activeRevision = revision;
    for (const [key, entry] of this.entries) {
      if (entry.revision !== revision) this.entries.delete(key);
    }
  }
}
