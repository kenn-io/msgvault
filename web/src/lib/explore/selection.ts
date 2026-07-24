import type {
  AllMatchingExploreSelection,
  ExplorePredicate,
  ExploreResult,
  SearchProvenance
} from './models';

function canonical(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(canonical);
  if (typeof value !== 'object' || value === null) return value;
  return Object.fromEntries(
    Object.entries(value)
      .filter(([, item]) => item !== undefined)
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([key, item]) => [key, canonical(item)])
  );
}

export function canonicalFingerprint(value: unknown): string {
  return JSON.stringify(canonical(value));
}

export function predicateFingerprint(predicate: ExplorePredicate): string {
  const { cursor: _cursor, candidate_snapshot_id: _snapshot, ...base } = predicate;
  return canonicalFingerprint(base);
}

export function hasValidSearchAuthority(
  predicate: ExplorePredicate,
  provenance: SearchProvenance,
  candidateSnapshotId?: string
): boolean {
  const lexical = provenance.lexical_index_revision;
  const vector = provenance.vector_generation;
  if (!predicate.query) return !lexical && vector === undefined && !candidateSnapshotId;
  if (predicate.search_mode === 'full_text') {
    return Boolean(lexical) && vector === undefined && !candidateSnapshotId;
  }
  if (predicate.search_mode === 'semantic') {
    return !lexical && vector !== undefined && Boolean(candidateSnapshotId);
  }
  if (predicate.search_mode === 'hybrid') {
    return Boolean(lexical) && vector !== undefined && Boolean(candidateSnapshotId);
  }
  return false;
}

export function createAllMatchingSelection(
  predicate: ExplorePredicate,
  result: ExploreResult,
  resultGeneration: number
): AllMatchingExploreSelection | undefined {
  if (result.candidatePoolSaturated) return undefined;
  if (!hasValidSearchAuthority(predicate, result.searchProvenance, result.candidateSnapshotId)) {
    return undefined;
  }
  return {
    mode: 'all_matching',
    predicate,
    exclusions: [],
    cacheRevision: result.cacheRevision,
    searchProvenance: result.searchProvenance,
    ...(result.candidateSnapshotId ? { candidateSnapshotId: result.candidateSnapshotId } : {}),
    predicateFingerprint: predicateFingerprint(predicate),
    resultGeneration
  };
}
