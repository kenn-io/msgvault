import type { APIClient } from '../api/client';
import type {
  ExploreCacheUnavailable,
  ExploreFilter,
  ExploreFilesLoadResult,
  ExploreFilesResponse,
  ExploreGroupDimension,
  ExploreGroupLoadResult,
  ExploreGroupsResponse,
  ExploreLoadResult,
  ExplorePredicate,
  ExploreResponse,
  ExploreResult,
  FileGroupsResponse,
  FileMIMEFamily
} from './models';
import { parseSearchCoverage, type SearchCoverageAction, type SearchCoverageStatus, type SearchCoverageValue } from '../search/modes';

export interface VisibleLexicalCounts {
  counts: Record<string, number>;
  cacheRevision: string;
  lexicalRevision: string;
  canonicalQueryHash: string;
}

function isCacheUnavailable(value: unknown): value is ExploreCacheUnavailable {
  if (typeof value !== 'object' || value === null) return false;
  const candidate = value as Partial<ExploreCacheUnavailable>;
  return (
    typeof candidate.error === 'string' &&
    typeof candidate.message === 'string' &&
    typeof candidate.readiness === 'string' &&
    typeof candidate.recovery_action === 'string'
  );
}

function normalize(data: ExploreResponse): ExploreResult {
  return {
    rows: data.rows,
    totalCount: data.total_count,
    cacheRevision: data.cache_revision,
    searchProvenance: data.search_provenance,
    candidateSnapshotId: data.candidate_snapshot_id,
    candidatePoolSaturated: data.candidate_pool_saturated ?? false,
    nextCursor: data.next_cursor
  };
}

function normalizeGroups(data: ExploreGroupsResponse | FileGroupsResponse): Extract<ExploreGroupLoadResult, { status: 'ready' }>['result'] {
  return {
    rows: data.rows,
    totalCount: data.total_count,
    cacheRevision: data.cache_revision,
    searchProvenance: data.search_provenance,
    candidateSnapshotId: data.candidate_snapshot_id,
    nextCursor: data.next_cursor
  };
}

function normalizeFiles(data: ExploreFilesResponse): Extract<ExploreFilesLoadResult, { status: 'ready' }>['result'] {
  return {
    files: data.files,
    totalCount: data.total_count,
    cacheRevision: data.cache_revision,
    searchProvenance: data.search_provenance,
    candidateSnapshotId: data.candidate_snapshot_id,
    nextCursor: data.next_cursor
  };
}

function messageFor(error: unknown, status: number): string {
  return typeof error === 'object' && error !== null && 'message' in error
    ? String(error.message)
    : `Exploration request failed (${status})`;
}

export interface ExploreAPI {
  explore(predicate: ExplorePredicate, signal?: AbortSignal): Promise<ExploreLoadResult>;
  groups(
    predicate: ExplorePredicate,
    dimension: ExploreGroupDimension,
    signal?: AbortSignal
  ): Promise<ExploreGroupLoadResult>;
  fileGroups(
    predicate: ExplorePredicate,
    filenameQuery: string,
    mimeFamilies: FileMIMEFamily[],
    dimension: ExploreGroupDimension,
    signal?: AbortSignal
  ): Promise<ExploreGroupLoadResult>;
  files(predicate: ExplorePredicate, signal?: AbortSignal): Promise<ExploreFilesLoadResult>;
  coverage(filters: ExploreFilter[], signal?: AbortSignal): Promise<SearchCoverageValue>;
  matchCounts(predicate: ExplorePredicate, rowKeys: string[], signal?: AbortSignal): Promise<VisibleLexicalCounts>;
  runCoverageAction(
    action: SearchCoverageAction,
    status: SearchCoverageStatus,
    signal?: AbortSignal
  ): Promise<void>;
}

function requireCompletedCLIRun(stream: string): void {
  let completed = false;
  for (const line of stream.split('\n')) {
    if (!line.trim()) continue;
    let event: { type?: unknown; error?: unknown };
    try {
      event = JSON.parse(line) as { type?: unknown; error?: unknown };
    } catch {
      throw new Error('Semantic index action returned an invalid event stream');
    }
    if (event.type === 'error' || event.type === 'failed' ||
      (event.type === 'complete' && typeof event.error === 'string' && event.error)) {
      throw new Error(typeof event.error === 'string' && event.error
        ? event.error
        : 'Semantic index action failed');
    }
    if (event.type === 'complete') completed = true;
  }
  if (!completed) throw new Error('Semantic index action did not complete');
}

export function createExploreAPI(client: APIClient): ExploreAPI {
  return {
    async explore(predicate, signal) {
      const { data, error, response } = await client.POST('/api/v1/explore', {
        body: predicate,
        signal
      });
      if (data) return { status: 'ready', result: normalize(data) };
      if (response.status === 503 && isCacheUnavailable(error)) {
        return { status: 'unavailable', unavailable: error };
      }
      throw new Error(messageFor(error, response.status));
    },
    async groups(predicate, dimension, signal) {
      const body = {
        ...(predicate.cursor ? { cursor: predicate.cursor } : {}),
        ...(predicate.filters ? { filters: predicate.filters } : {}),
        ...(predicate.query ? { query: predicate.query, search_mode: predicate.search_mode } : {}),
        grouping: [dimension],
        limit: predicate.limit,
        presentation: 'table' as const
      };
      const { data, error, response } = await client.POST('/api/v1/explore/groups', {
        body,
        signal
      });
      if (data) return { status: 'ready', result: normalizeGroups(data) };
      if (response.status === 503 && isCacheUnavailable(error)) {
        return { status: 'unavailable', unavailable: error };
      }
      throw new Error(messageFor(error, response.status));
    },
    async fileGroups(predicate, filenameQuery, mimeFamilies, dimension, signal) {
      const { cursor, ...context } = predicate;
      const { data, error, response } = await client.POST('/api/v1/files/groups', {
        body: {
          predicate: context,
          ...(filenameQuery ? { filename_query: filenameQuery } : {}),
          ...(mimeFamilies.length ? { mime_families: mimeFamilies } : {}),
          grouping: [dimension],
          limit: predicate.limit,
          ...(cursor ? { cursor } : {})
        },
        signal
      });
      if (data) return { status: 'ready', result: normalizeGroups(data) };
      if (response.status === 503 && isCacheUnavailable(error)) {
        return { status: 'unavailable', unavailable: error };
      }
      throw new Error(messageFor(error, response.status));
    },
    async files(predicate, signal) {
      const { cursor, ...context } = predicate;
      const { data, error, response } = await client.POST('/api/v1/explore/files', {
        body: { predicate: context, limit: 100, ...(cursor ? { cursor } : {}) },
        signal
      });
      if (data) return { status: 'ready', result: normalizeFiles(data) };
      if (response.status === 503 && isCacheUnavailable(error)) {
        return { status: 'unavailable', unavailable: error };
      }
      throw new Error(messageFor(error, response.status));
    },
    async coverage(filters, signal) {
      const { data, error, response } = await client.POST('/api/v1/search/coverage', {
        body: { filters }, signal
      });
      const coverage = parseSearchCoverage(data);
      if (coverage) return coverage;
      if (data) throw new Error('Semantic coverage response is incompatible with this browser');
      throw new Error(messageFor(error, response.status));
    },
    async matchCounts(predicate, rowKeys, signal) {
      const { data, error, response } = await client.POST('/api/v1/explore/match-counts', {
        body: { predicate, row_keys: rowKeys }, signal
      });
      if (!data) throw new Error(messageFor(error, response.status));
      return {
        counts: Object.fromEntries(data.counts.map((entry) => [entry.row_key, entry.count])),
        cacheRevision: data.cache_revision,
        lexicalRevision: data.lexical_index_revision,
        canonicalQueryHash: data.canonical_query_hash
      };
    },
    async runCoverageAction(action, _status, signal) {
      if (action === 'retry') return;
      const { data, error, response } = await client.POST('/api/v1/cli/run', {
        body: { args: ['embeddings', 'build', '--full-rebuild', '--yes'] }, signal, parseAs: 'text'
      });
      if (!response.ok) throw new Error(messageFor(error, response.status));
      requireCompletedCLIRun(data ?? '');
    }
  };
}
