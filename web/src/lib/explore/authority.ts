import { canonicalFingerprint } from './selection';

interface AnalyticalAuthorityWire {
  cache_revision: string;
  search_provenance?: unknown;
  candidate_snapshot_id?: string;
}

export function analyticalAuthority(value: AnalyticalAuthorityWire): string {
  return canonicalFingerprint({
    revision: value.cache_revision,
    provenance: value.search_provenance ?? {},
    snapshot: value.candidate_snapshot_id ?? ''
  });
}
