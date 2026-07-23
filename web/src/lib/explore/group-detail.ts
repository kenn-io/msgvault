import type { ExploreAPI } from './api';
import type {
  ExploreCacheUnavailable,
  ExploreGroupDimension,
  ExploreGroupRow,
  ExplorePredicate
} from './models';

/** Page size for the bounded exact-key lookup behind group detail hydration. */
export const GROUP_DETAIL_PAGE_SIZE = 100;

/** Upper bound on pages walked before the lookup degrades to "missing". */
export const GROUP_DETAIL_MAX_PAGES = 5;

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
 * one of them can outrank the requested key. The groups endpoint has no
 * exact-key parameter, so this pages through the listing until the key is
 * found, the listing ends, or the bounded page cap is reached.
 */
export async function findGroupDetail(
  api: Pick<ExploreAPI, 'groups'>,
  predicate: ExplorePredicate,
  dimension: ExploreGroupDimension,
  key: string,
  signal?: AbortSignal
): Promise<GroupDetailLookup> {
  let cursor: string | undefined;
  for (let page = 0; page < GROUP_DETAIL_MAX_PAGES; page += 1) {
    const loaded = await api.groups(
      { ...predicate, limit: GROUP_DETAIL_PAGE_SIZE, ...(cursor === undefined ? {} : { cursor }) },
      dimension,
      signal
    );
    if (loaded.status === 'unavailable') {
      return { status: 'unavailable', unavailable: loaded.unavailable };
    }
    const row = loaded.result.rows.find((candidate) => candidate.key === key);
    if (row) return { status: 'found', row };
    cursor = loaded.result.nextCursor;
    if (cursor === undefined) return { status: 'missing' };
  }
  return { status: 'missing' };
}
