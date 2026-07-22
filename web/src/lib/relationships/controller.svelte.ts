import type { APIClient } from '../api/client';
import type {
  DomainSummary,
  ExploreCacheUnavailable,
  ExploreFilter,
  ExplorePredicate,
  IdentitySearchSort,
  PersonSummary
} from '../explore/models';
import { predicateFingerprint } from '../explore/selection';
import type { RelationshipFacet, RelationshipRow, RelationshipTimelineRow } from './models';

export type LinkOutcome =
  | { ok: true; identityRevision: number; cacheState: 'ready' | 'stale' }
  | { ok: false; code: 'already_linked' | 'invalid' | 'error'; message: string };

type ListRow = RelationshipRow | PersonSummary | DomainSummary;
type ListRows = RelationshipRow[] | PersonSummary[] | DomainSummary[];

/** Snapshot of the query context a list page belongs to, captured by
 * loadList so loadMoreList replays the exact same endpoint and body (plus
 * the cursor) even if `facet`/`query`/`showAll` have since been reassigned. */
type ListPageRequest =
  | { kind: 'relationships'; filters: ExplorePredicate['filters']; showAll: boolean }
  | { kind: 'people' | 'domains'; context: ExplorePredicate; query: string };

interface ListPageResponse {
  data?: { rows?: unknown[] | null; next_cursor?: string; total_count?: number };
  error?: unknown;
  response: Response;
}

const RANKED_LIST_LIMIT = 200;
const SEARCH_LIST_LIMIT = 500;
const TIMELINE_PAGE_LIMIT = 200;
const DEFAULT_IDENTITY_SORT: IdentitySearchSort = { field: 'activity_count', direction: 'desc' };
const REPEATED_CURSOR_MESSAGE = 'Pagination stopped because the server repeated a cursor without progress.';
const TIMELINE_RESTART_MESSAGE = 'Timeline restarted: the archive changed.';

/**
 * Data controller for the Relationships hub's left list (ranked/searched
 * people or domains), center detail (cluster or domain header + timeline),
 * and identity link/unlink mutations.
 *
 * Ports the proven mechanics from PeopleWorkspace.svelte/DomainWorkspace.svelte
 * as plain class methods: a generation counter per independent load (list,
 * detail) that discards stale async completions, an AbortController per load
 * that `destroy()` sweeps, and a seen-cursors set that stops runaway
 * pagination if the server ever repeats a cursor without progress.
 *
 * `openTarget` dispatches on the target's prefix (`cluster:` vs `domain:`),
 * never on `facet` — a target string can legitimately disagree with the
 * facet that produced it (e.g. restored from a URL), and the prefix is the
 * only value that determines which endpoints to call.
 */
export class RelationshipsController {
  facet = $state<RelationshipFacet>('people');
  query = $state('');
  showAll = $state(false);
  listRows = $state<ListRows>([]);
  listLoading = $state(false);
  listLoadingMore = $state(false);
  listError = $state<string | null>(null);
  /** Cursor for the next list page, scoped to the context loadList captured
   * in `listPageRequest`; null when the last page has been reached. */
  listCursor = $state<string | null>(null);
  listTotalCount = $state<number | null>(null);
  degraded = $state<'cache_unavailable' | null>(null);

  target = $state<string | null>(null);
  /** Fingerprint of the predicate `target` was last opened with — lets a
   * caller (AppShell's URL-hydration effect) tell "this target is already
   * open" apart from "this target is open, but for a stale predicate" (e.g.
   * filters picked up from Everything/Files while the URL-carried target
   * itself didn't change) without keeping its own shadow copy that could
   * fall out of sync with a call made through a different path (the
   * in-hub click handler calls openTarget directly). Set by openTarget,
   * cleared by clearTarget. */
  lastPredicateFingerprint = $state<string | null>(null);
  detail = $state<PersonSummary | DomainSummary | null>(null);
  timelineRows = $state<RelationshipTimelineRow[]>([]);
  timelineCursor = $state<string | null>(null);
  timelineLoading = $state(false);
  timelineLoadingMore = $state(false);
  timelineError = $state<string | null>(null);
  /** One-line notice set when a cursor_invalidated 409 silently restarted the
   * timeline from page 1; cleared on the next openTarget navigation. */
  timelineRestartNotice = $state<string | null>(null);
  canonicalID = $state<number | null>(null);
  identityRevision = $state<number | null>(null);

  private readonly client: APIClient;
  private readonly timezone: () => string;
  private listAbort: AbortController | undefined;
  private detailAbort: AbortController | undefined;
  private listGeneration = 0;
  private detailGeneration = 0;
  private listPageRequest: ListPageRequest | undefined;
  private listSeenCursors = new Set<string>();
  private seenCursors = new Set<string>();
  private lastPredicate: ExplorePredicate | undefined;
  private lastListPredicate: ExplorePredicate | undefined;

  constructor(client: APIClient, timezone: () => string) {
    this.client = client;
    this.timezone = timezone;
  }

  async loadList(predicate: ExplorePredicate): Promise<void> {
    this.listAbort?.abort();
    const controller = new AbortController();
    this.listAbort = controller;
    const generation = ++this.listGeneration;
    this.lastListPredicate = predicate;
    this.listLoading = true;
    this.listLoadingMore = false;
    this.listError = null;
    this.degraded = null;
    // A cursor belongs to the context that minted it: drop it (and the stale
    // total) the moment a new context load starts, so the scroll sentinel can
    // never fetch an old context's page into the new list.
    this.listCursor = null;
    this.listTotalCount = null;
    this.listSeenCursors = new Set<string>();
    const query = this.query.trim();
    const request: ListPageRequest = this.facet === 'domains'
      ? { kind: 'domains', context: contextPredicate(predicate), query }
      : query === ''
        ? { kind: 'relationships', filters: predicate.filters, showAll: this.showAll }
        : { kind: 'people', context: contextPredicate(predicate), query };
    this.listPageRequest = request;
    try {
      const response = await this.postListPage(request, undefined, controller.signal);
      this.applyListResponse(generation, controller.signal, response, false);
    } catch (cause: unknown) {
      if (generation === this.listGeneration && !controller.signal.aborted) this.listError = errorMessage(cause, 0);
    } finally {
      if (generation === this.listGeneration) this.listLoading = false;
    }
  }

  /** Fetches the next list page for the context loadList captured, appending
   * deduped rows. Guarded no-op while a load is in flight or at the end. */
  async loadMoreList(): Promise<void> {
    if (this.listLoading || this.listLoadingMore || !this.listCursor) return;
    const request = this.listPageRequest;
    const abort = this.listAbort;
    if (!request || !abort) return;
    const cursor = this.listCursor;
    if (this.listSeenCursors.has(cursor)) {
      this.listError = REPEATED_CURSOR_MESSAGE;
      this.listCursor = null;
      return;
    }
    const generation = this.listGeneration;
    const signal = abort.signal;
    this.listLoadingMore = true;
    try {
      const response = await this.postListPage(request, cursor, signal);
      if (signal.aborted || generation !== this.listGeneration) return;
      this.listSeenCursors.add(cursor);
      this.applyListResponse(generation, signal, response, true);
    } catch (cause: unknown) {
      if (!signal.aborted && generation === this.listGeneration) {
        this.listError = errorMessage(cause, 0);
        this.listCursor = null;
      }
    } finally {
      if (generation === this.listGeneration) this.listLoadingMore = false;
    }
  }

  private postListPage(
    request: ListPageRequest,
    cursor: string | undefined,
    signal: AbortSignal
  ): Promise<ListPageResponse> {
    const page = cursor === undefined ? {} : { cursor };
    if (request.kind === 'relationships') {
      return this.client.POST('/api/v1/relationships', {
        body: { filters: request.filters, show_all: request.showAll, limit: RANKED_LIST_LIMIT, ...page },
        signal
      });
    }
    const body = {
      predicate: request.context,
      identity_query: request.query,
      sort: DEFAULT_IDENTITY_SORT,
      limit: SEARCH_LIST_LIMIT,
      ...page
    };
    return request.kind === 'domains'
      ? this.client.POST('/api/v1/domains/search', { body, signal })
      : this.client.POST('/api/v1/people/search', { body, signal });
  }

  private applyListResponse(
    generation: number,
    signal: AbortSignal,
    response: ListPageResponse,
    append: boolean
  ): void {
    if (signal.aborted || generation !== this.listGeneration) return;
    const { data, error, response: res } = response;
    if (data) {
      const rows = (data.rows ?? []) as ListRow[];
      this.listRows = (append ? mergeListRows(this.listRows as ListRow[], rows) : rows) as ListRows;
      this.listCursor = data.next_cursor ?? null;
      this.listTotalCount = data.total_count ?? null;
      return;
    }
    this.listCursor = null;
    if (res.status === 503 && isCacheUnavailable(error)) {
      this.degraded = 'cache_unavailable';
      return;
    }
    this.listError = errorMessage(error, res.status);
  }

  async openTarget(target: string, predicate: ExplorePredicate): Promise<void> {
    this.detailAbort?.abort();
    const controller = new AbortController();
    this.detailAbort = controller;
    const generation = ++this.detailGeneration;
    this.target = target;
    this.lastPredicate = predicate;
    this.lastPredicateFingerprint = predicateFingerprint(predicate);
    this.detail = null;
    this.timelineRows = [];
    this.timelineCursor = null;
    this.timelineError = null;
    this.timelineRestartNotice = null;
    this.timelineLoadingMore = false;
    this.canonicalID = null;
    this.identityRevision = null;
    this.seenCursors = new Set<string>();

    const clusterID = parseClusterID(target);
    const domainName = parseDomainName(target);
    if (clusterID === undefined && domainName === undefined) {
      this.timelineLoading = false;
      return;
    }
    this.timelineLoading = true;
    try {
      if (clusterID !== undefined) await this.openCluster(clusterID, predicate, generation, controller.signal);
      else if (domainName !== undefined) await this.openDomain(domainName, predicate, generation, controller.signal);
    } finally {
      if (generation === this.detailGeneration) this.timelineLoading = false;
    }
  }

  private async openCluster(
    id: number,
    predicate: ExplorePredicate,
    generation: number,
    signal: AbortSignal
  ): Promise<void> {
    const context = contextPredicate(predicate);
    try {
      // The unfiltered GET stays the source of cluster metadata (identifiers,
      // members, edges) for the link/unlink UI; when filters are active the
      // contextual /summary carries the header metrics so they agree with the
      // filtered timeline and files below instead of the whole archive.
      const [personResponse, , summaryResponse] = await Promise.all([
        this.client.GET('/api/v1/people/{id}', { params: { path: { id } }, signal }),
        this.fetchClusterPage(id, predicate.filters ?? undefined, generation, undefined, signal),
        hasActiveFilters(context)
          ? this.client.POST('/api/v1/people/{id}/summary', { params: { path: { id } }, body: context, signal })
          : undefined
      ]);
      if (signal.aborted || generation !== this.detailGeneration) return;
      const summary = summaryResponse?.data?.summary;
      const base = personResponse.data;
      if (base) this.detail = summary ? mergePersonDetail(base, summary) : base;
      else if (summary) this.detail = summary;
      if (!base) this.timelineError ||= errorMessage(personResponse.error, personResponse.response.status);
      if (summaryResponse && !summaryResponse.data) {
        this.timelineError ||= errorMessage(summaryResponse.error, summaryResponse.response.status);
      }
    } catch (cause: unknown) {
      if (!signal.aborted && generation === this.detailGeneration) this.timelineError ||= errorMessage(cause, 0);
    }
  }

  private async openDomain(
    domain: string,
    predicate: ExplorePredicate,
    generation: number,
    signal: AbortSignal
  ): Promise<void> {
    const context = contextPredicate(predicate);
    try {
      const [detailResponse, , summaryResponse] = await Promise.all([
        this.client.GET('/api/v1/domains/{domain}', { params: { path: { domain } }, signal }),
        this.fetchDomainPage(domain, context, generation, undefined, signal),
        hasActiveFilters(context)
          ? this.client.POST('/api/v1/domains/{domain}/summary', { params: { path: { domain } }, body: context, signal })
          : undefined
      ]);
      if (signal.aborted || generation !== this.detailGeneration) return;
      const summary = summaryResponse?.data?.summary;
      const base = detailResponse.data;
      if (base) this.detail = summary ? { ...base, ...summary } : base;
      else if (summary) this.detail = summary;
      if (!base) this.timelineError ||= errorMessage(detailResponse.error, detailResponse.response.status);
      if (summaryResponse && !summaryResponse.data) {
        this.timelineError ||= errorMessage(summaryResponse.error, summaryResponse.response.status);
      }
    } catch (cause: unknown) {
      if (!signal.aborted && generation === this.detailGeneration) this.timelineError ||= errorMessage(cause, 0);
    }
  }

  /**
   * Drops the open target and its detail/timeline state (cluster or domain
   * header, rows, canonical ID, revision) back to the hub's empty state.
   * Called when the URL-carried target becomes null (Esc, Back/Forward) so
   * the center pane never keeps showing a stale person/domain after the
   * target it belonged to is gone. Aborts any in-flight detail fetch and
   * bumps the generation counter so a late response cannot resurrect it.
   */
  clearTarget(): void {
    this.detailAbort?.abort();
    this.detailAbort = undefined;
    ++this.detailGeneration;
    this.target = null;
    this.lastPredicate = undefined;
    this.lastPredicateFingerprint = null;
    this.detail = null;
    this.timelineRows = [];
    this.timelineCursor = null;
    this.timelineLoading = false;
    this.timelineLoadingMore = false;
    this.timelineError = null;
    this.timelineRestartNotice = null;
    this.canonicalID = null;
    this.identityRevision = null;
    this.seenCursors = new Set<string>();
  }

  async loadMoreTimeline(): Promise<void> {
    if (this.timelineLoadingMore || !this.timelineCursor || !this.target || !this.lastPredicate || !this.detailAbort) {
      return;
    }
    const clusterID = parseClusterID(this.target);
    const domainName = parseDomainName(this.target);
    if (clusterID === undefined && domainName === undefined) return;
    this.timelineLoadingMore = true;
    const generation = this.detailGeneration;
    const signal = this.detailAbort.signal;
    const cursor = this.timelineCursor;
    if (clusterID !== undefined) {
      await this.fetchClusterPage(clusterID, this.lastPredicate.filters ?? undefined, generation, cursor, signal);
    } else if (domainName !== undefined) {
      await this.fetchDomainPage(domainName, contextPredicate(this.lastPredicate), generation, cursor, signal);
    }
  }

  private async fetchClusterPage(
    id: number,
    filters: ExploreFilter[] | undefined,
    generation: number,
    cursor: string | undefined,
    signal: AbortSignal
  ): Promise<void> {
    try {
      if (cursor && this.seenCursors.has(cursor)) {
        this.applyTimelineFailure(generation, REPEATED_CURSOR_MESSAGE);
        return;
      }
      const response = await this.client.POST('/api/v1/relationships/{id}/timeline', {
        params: { path: { id } },
        body: { timezone: this.timezone(), filters, limit: TIMELINE_PAGE_LIMIT, ...(cursor ? { cursor } : {}) },
        signal
      });
      if (signal.aborted || generation !== this.detailGeneration) return;
      const { data, error, response: res } = response;
      if (!data) {
        if (cursor && res.status === 409 && isErrorCode(error, 'cursor_invalidated')) {
          this.timelineRows = [];
          this.timelineCursor = null;
          this.seenCursors = new Set<string>();
          this.timelineRestartNotice = TIMELINE_RESTART_MESSAGE;
          await this.fetchClusterPage(id, filters, generation, undefined, signal);
          return;
        }
        this.applyTimelineFailure(generation, errorMessage(error, res.status));
        return;
      }
      this.timelineError = null;
      if (cursor) this.seenCursors.add(cursor);
      this.timelineRows = mergeTimelineRows(this.timelineRows, data.rows ?? []);
      this.canonicalID = data.canonical_id;
      this.identityRevision = data.identity_revision;
      this.timelineCursor = data.next_cursor ?? null;
    } catch (cause: unknown) {
      if (!signal.aborted && generation === this.detailGeneration) this.timelineError = errorMessage(cause, 0);
    } finally {
      if (generation === this.detailGeneration) this.timelineLoadingMore = false;
    }
  }

  private async fetchDomainPage(
    domain: string,
    context: ExplorePredicate,
    generation: number,
    cursor: string | undefined,
    signal: AbortSignal
  ): Promise<void> {
    try {
      if (cursor && this.seenCursors.has(cursor)) {
        this.applyTimelineFailure(generation, REPEATED_CURSOR_MESSAGE);
        return;
      }
      const response = await this.client.POST('/api/v1/domains/{domain}/timeline', {
        params: { path: { domain } },
        body: { ...context, limit: TIMELINE_PAGE_LIMIT, ...(cursor ? { cursor } : {}) },
        signal
      });
      if (signal.aborted || generation !== this.detailGeneration) return;
      const { data, error, response: res } = response;
      if (!data) {
        this.applyTimelineFailure(generation, errorMessage(error, res.status));
        return;
      }
      this.timelineError = null;
      if (cursor) this.seenCursors.add(cursor);
      this.timelineRows = mergeTimelineRows(this.timelineRows, data.rows);
      this.timelineCursor = data.next_cursor ?? null;
    } catch (cause: unknown) {
      if (!signal.aborted && generation === this.detailGeneration) this.timelineError = errorMessage(cause, 0);
    } finally {
      if (generation === this.detailGeneration) this.timelineLoadingMore = false;
    }
  }

  private applyTimelineFailure(generation: number, message: string): void {
    if (generation !== this.detailGeneration) return;
    this.timelineError = message;
    this.timelineCursor = null;
  }

  async linkParticipants(a: number, b: number): Promise<LinkOutcome> {
    try {
      const response = await this.client.POST('/api/v1/identity/links', { body: { participant_a: a, participant_b: b } });
      return await this.applyLinkResponse(response);
    } catch (cause: unknown) {
      return { ok: false, code: 'error', message: errorMessage(cause, 0) };
    }
  }

  async unlinkParticipants(a: number, b: number): Promise<LinkOutcome> {
    try {
      const response = await this.client.POST('/api/v1/identity/unlinks', { body: { participant_a: a, participant_b: b } });
      return await this.applyLinkResponse(response);
    } catch (cause: unknown) {
      return { ok: false, code: 'error', message: errorMessage(cause, 0) };
    }
  }

  private async applyLinkResponse(response: {
    data?: { identity_revision: number; cache_state: 'ready' | 'stale' };
    error?: unknown;
    response: Response;
  }): Promise<LinkOutcome> {
    const { data, error, response: res } = response;
    if (data) {
      // A null identityRevision means the prior timeline load failed, so
      // there is nothing to compare against — treat that as "changed" and
      // refresh anyway rather than silently skipping the reopen. The ranked
      // list reloads unconditionally: a link/unlink can move rows (merge or
      // split a cluster) regardless of whether the currently open target's
      // own revision moved.
      const changed = this.identityRevision === null || this.identityRevision !== data.identity_revision;
      const reloads: Promise<void>[] = [];
      if (changed && this.target && this.lastPredicate) reloads.push(this.openTarget(this.target, this.lastPredicate));
      if (this.lastListPredicate) reloads.push(this.loadList(this.lastListPredicate));
      await Promise.all(reloads);
      return { ok: true, identityRevision: data.identity_revision, cacheState: data.cache_state };
    }
    const message = errorMessage(error, res.status);
    if (res.status === 409 && isErrorCode(error, 'already_linked')) return { ok: false, code: 'already_linked', message };
    if (res.status === 400) return { ok: false, code: 'invalid', message };
    return { ok: false, code: 'error', message };
  }

  destroy(): void {
    this.listAbort?.abort();
    this.detailAbort?.abort();
  }
}

// Also drops the workspace text query: the relationships ranking and
// cluster-timeline endpoints accept no text query, so the hub applies none on
// ANY surface — a predicate that still carries one (a deep link minted before
// the workspace transition started clearing it) must not half-apply to the
// domain/people search surfaces only.
function contextPredicate(predicate: ExplorePredicate): ExplorePredicate {
  const {
    cursor: _cursor, grouping: _grouping, candidate_snapshot_id: _snapshot,
    query: _query, search_mode: _searchMode, ...context
  } = predicate;
  return { ...context, presentation: 'table' };
}

/** True when the workspace context carries filters that narrow the archive —
 * the only predicate fields (after contextPredicate stripping) that change
 * what the timeline and files show, and therefore the only case where the
 * unfiltered GET header would contradict the filtered body. */
function hasActiveFilters(context: ExplorePredicate): boolean {
  return (context.filters ?? []).length > 0;
}

/** Contextual summary metrics win; the unfiltered GET remains the fallback
 * source of cluster metadata (identifiers, member/edge graph) that the
 * link/unlink UI needs and the summary row may omit. */
function mergePersonDetail(base: PersonSummary, summary: PersonSummary): PersonSummary {
  return {
    ...base,
    ...summary,
    cluster: summary.cluster ?? base.cluster,
    identifiers: summary.identifiers ?? base.identifiers
  };
}

// The generated summary types carry `[key: string]: unknown` index
// signatures, which defeat `in`-based narrowing (same limitation noted in
// RelationshipList.svelte) — hence the casts after each runtime check.
function listRowKey(row: ListRow): string {
  if ('canonical_id' in row) return `cluster:${(row as RelationshipRow).canonical_id}`;
  if ('domain' in row) return `domain:${(row as DomainSummary).domain}`;
  return `cluster:${(row as PersonSummary).id}`;
}

/** Appends a page without duplicating a boundary row the backend re-served,
 * keyed by the same canonical id / domain the list keys its rows with. */
function mergeListRows(existing: ListRow[], incoming: ListRow[]): ListRow[] {
  const merged = new Map(existing.map((row) => [listRowKey(row), row]));
  for (const row of incoming) merged.set(listRowKey(row), row);
  return [...merged.values()];
}

function mergeTimelineRows<Row extends { key: string }>(existing: Row[], incoming: Row[]): Row[] {
  const merged = new Map(existing.map((row) => [row.key, row]));
  for (const row of incoming) merged.set(row.key, row);
  return [...merged.values()];
}

function parseClusterID(target: string | null): number | undefined {
  const match = target ? /^cluster:([1-9][0-9]*)$/.exec(target) : null;
  if (!match?.[1]) return undefined;
  const id = Number(match[1]);
  return Number.isSafeInteger(id) ? id : undefined;
}

function parseDomainName(target: string | null): string | undefined {
  return target?.startsWith('domain:') ? target.slice('domain:'.length) : undefined;
}

function isCacheUnavailable(value: unknown): value is ExploreCacheUnavailable {
  return typeof value === 'object' && value !== null && 'readiness' in value && 'recovery_action' in value;
}

function isErrorCode(value: unknown, code: string): boolean {
  return typeof value === 'object' && value !== null && 'error' in value && (value as { error?: unknown }).error === code;
}

function errorMessage(value: unknown, status: number): string {
  if (typeof value === 'object' && value !== null && 'message' in value) {
    const message = (value as { message?: unknown }).message;
    if (typeof message === 'string' && message) return message;
  }
  return status ? `Request failed (${status})` : 'Request failed';
}
