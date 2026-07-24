import type { ExploreAPI } from './api';
import type {
  ExploreCacheUnavailable,
  ExploreGroupDimension,
  ExploreGroupRow,
  ExplorePredicate
} from './models';

export type GroupDetailLookup =
  | { status: 'found'; row: ExploreGroupRow }
  | { status: 'missing' }
  | { status: 'unavailable'; unavailable: ExploreCacheUnavailable };

/**
 * Finds the group row matching an exact key under the given predicate.
 *
 * Filtering by a multi-valued dimension (participant, domain) does not make
 * the requested group the sole — or even the first — row: every
 * co-participant/co-domain of the matching entries forms a group too, and
 * one of them can outrank the requested key. The groups endpoint's group_key
 * filter resolves the key server-side regardless of its rank, so an empty
 * response is authoritative absence rather than a paging cap.
 */
export async function findGroupDetail(
  api: Pick<ExploreAPI, 'groups'>,
  predicate: ExplorePredicate,
  dimension: ExploreGroupDimension,
  key: string,
  signal?: AbortSignal
): Promise<GroupDetailLookup> {
  const loaded = await api.groups({ ...predicate, limit: 1, group_key: key }, dimension, signal);
  if (loaded.status === 'unavailable') {
    return { status: 'unavailable', unavailable: loaded.unavailable };
  }
  const row = loaded.result.rows.find((candidate) => candidate.key === key);
  return row ? { status: 'found', row } : { status: 'missing' };
}
