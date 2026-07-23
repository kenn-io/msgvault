import { tick, untrack } from 'svelte';

import type { APIClient } from '../api/client';
import type {
  EntryRow,
  ExploreCacheUnavailable,
  ExploreFileFact,
  ExploreFilesResult,
  ExploreGroupDimension,
  ExploreGroupResult,
  ExploreGroupRow,
  ExploreResult,
  FileMIMEFamily
} from './models';
import { createExploreAPI, type ExploreAPI } from './api';
import { parseAttachmentSelection } from './attachment-authority';
import { parseGroupSelection } from './group-context';
import { LOAD_THROUGH_END_MAX_PAGES } from './paging';
import { canonicalFingerprint, predicateFingerprint } from './selection';
import type { ExploreState } from './state.svelte';

export type PageLoadOutcome =
  | { status: 'advanced' }
  | { status: 'exhausted' }
  | { status: 'failed' | 'unavailable' | 'aborted' | 'stale' };

/** Cross-cutting seams the loader cannot own itself: `sortNotice` is shared
 * with the Files-shell grouped view and the reverse-sort-attempt message
 * (AppShell), `selection` is a shell-wide object, and grid focus restoration
 * depends on AppShell's `currentGrid()` DOM query (shared across workspaces).
 * The loader calls these at the exact points the original AppShell effect
 * wrote to that shared state, preserving imperative last-write-wins order. */
export interface ExploreLoaderCallbacks {
  isEnabled: () => boolean;
  onPredicateChange: () => void;
  onPagingNotice: (message: string | null) => void;
  onRestorationFocus: () => void;
}

const REPEATED_CURSOR_NOTICE = 'Pagination stopped because the server repeated a cursor without progress.';
const NO_PROGRESS_NOTICE = 'Pagination stopped because the next page made no row progress.';
const REVISION_CHANGED_NOTICE = 'Results changed while loading another page. Reload this view.';
export const END_PAUSE_NOTICE =
  'End paused loading to keep the table responsive; press End again to continue or refine the filters.';

function samePageAuthority(
  next: Pick<ExploreResult, 'cacheRevision' | 'searchProvenance' | 'candidateSnapshotId'>,
  first: Pick<ExploreResult, 'cacheRevision' | 'searchProvenance' | 'candidateSnapshotId'>
): boolean {
  return next.cacheRevision === first.cacheRevision &&
    canonicalFingerprint(next.searchProvenance) === canonicalFingerprint(first.searchProvenance) &&
    next.candidateSnapshotId === first.candidateSnapshotId;
}

/**
 * Owns the Everything workspace's row/group/file loading: the main
 * per-predicate load, cursor paging (including the End-key drain-to-end),
 * and deep-link restoration walk that pages forward until a durable
 * selection/scroll/active key becomes visible again.
 *
 * The same load path also serves the Files-shell grouped view (AppShell
 * gates on `workspace === 'files' && groupingChain.length > 0` in addition
 * to `workspace === 'everything'`), so this class is shared by both call
 * sites rather than owned exclusively by EverythingWorkspace.
 */
export class ExploreLoader {
  rows = $state<EntryRow[]>([]);
  groupRows = $state<ExploreGroupRow[]>([]);
  fileFacts = $state<ExploreFileFact[]>([]);
  resultGeneration = $state(0);
  result = $state<ExploreResult>();
  unavailable = $state<ExploreCacheUnavailable>();
  loading = $state(false);
  loadingMore = $state(false);
  error = $state('');
  nextCursor = $state<string>();
  resultFingerprint = $state('');
  restoring = $state(false);

  private readonly state: ExploreState;
  private readonly api: ExploreAPI;
  private readonly callbacks: ExploreLoaderCallbacks;
  private requestGeneration = 0;
  private requestController: AbortController | undefined;
  private pagePredicate: ReturnType<ExploreState['predicate']> | undefined;
  private pageGrouping: ExploreGroupDimension | undefined;
  private pageGroupsFiles = false;
  private pageKind: 'entries' | 'groups' | 'files' = 'entries';
  private pageFileFilenameQuery = '';
  private pageFileMIMEFamilies: FileMIMEFamily[] = [];
  private pageAuthority: Pick<ExploreResult, 'cacheRevision' | 'searchProvenance' | 'candidateSnapshotId'> | undefined;
  private pageRequest: Promise<PageLoadOutcome> | undefined;
  private seenCursors = new Set<string>();
  private restorationCycleCompleted = false;
  private selectionFingerprint = '';
  #retryRevision = $state(0);

  constructor(client: APIClient, state: ExploreState, callbacks: ExploreLoaderCallbacks) {
    this.state = state;
    this.api = createExploreAPI(client);
    this.callbacks = callbacks;

    // Declared as a constructor-local (rather than a class field) because a
    // $derived field initializer referencing `this.state` would run before
    // the constructor body above assigns it; TS's field-initializer-order
    // check flags that even though $derived itself is evaluated lazily.
    const requestFingerprint = $derived(predicateFingerprint(this.state.predicate()));

    $effect(() => {
      if (this.state.peekRestorationEpoch() === undefined) this.restoring = false;
    });

    $effect(() => this.runLoad(requestFingerprint));
  }

  private runLoad(requestFingerprint: string): void {
    const workspace = this.state.current.workspace;
    const restorationEpoch = this.state.restorationEpoch;
    void this.#retryRevision;
    const predicate = untrack(() => this.state.predicate());
    const fingerprint = requestFingerprint;
    if (fingerprint !== this.selectionFingerprint) {
      this.callbacks.onPredicateChange();
      this.selectionFingerprint = fingerprint;
    }
    const filesGrouping = workspace === 'files' && this.state.current.groupingChain.length > 0;
    const fileFilenameQuery = this.state.current.fileFilenameQuery;
    const fileMIMEFamilies = this.state.current.fileMIMEFamilies;
    if (!this.callbacks.isEnabled() || (workspace !== 'everything' && !filesGrouping)) {
      this.requestGeneration += 1;
      this.requestController?.abort();
      this.loading = false;
      // Session bootstrap temporarily disables the shell before its first
      // request. Preserve that initial URL restoration until authority is
      // available. Enabled workspaces without their own analytical grid have
      // no row-loading phase, so acknowledge their history epoch here.
      if (this.callbacks.isEnabled() && workspace !== 'files') {
        const epoch = untrack(() => this.state.peekRestorationEpoch());
        if (epoch !== undefined) this.state.acknowledgeRestoration(epoch);
        this.restoring = false;
        this.restorationCycleCompleted = true;
      }
      return;
    }
    this.requestController?.abort();
    this.requestController = new AbortController();
    const controller = this.requestController;
    const generation = ++this.requestGeneration;
    this.resultGeneration = generation;
    this.nextCursor = undefined;
    this.pageAuthority = undefined;
    this.pagePredicate = predicate;
    this.pageGrouping = undefined;
    this.pageGroupsFiles = filesGrouping;
    const presentation = this.state.current.presentation;
    const grouping = this.state.current.groupingChain;
    this.pageKind = grouping.length > 0 ? 'groups' : presentation === 'files' ? 'files' : 'entries';
    this.pageFileFilenameQuery = fileFilenameQuery;
    this.pageFileMIMEFamilies = fileMIMEFamilies;
    this.pageRequest = undefined;
    this.seenCursors = new Set<string>();
    const restoreEpoch = untrack(() => this.state.peekRestorationEpoch());
    const restoreTargets = restoreEpoch === restorationEpoch ? untrack(() => this.restorationKeys()) : [];
    this.restoring = restoreTargets.length > 0;
    this.loadingMore = false;
    this.loading = true;
    this.error = '';
    this.unavailable = undefined;
    if (this.pageKind === 'groups') this.groupRows = [];
    if (this.pageKind === 'files') this.fileFacts = [];
    const request = grouping.length > 0
      ? (filesGrouping
        ? this.api.fileGroups(
          predicate, fileFilenameQuery, fileMIMEFamilies,
          (this.pageGrouping = grouping[0]!), controller.signal
        )
        : this.api.groups(predicate, (this.pageGrouping = grouping[0]!), controller.signal))
      : presentation === 'files'
        ? this.api.files({ ...predicate, grouping: undefined }, controller.signal)
        : this.api.explore({ ...predicate, grouping: undefined, presentation: 'table' }, controller.signal);
    void request
      .then((loaded) => {
        if (generation !== this.requestGeneration) return;
        if (loaded.status === 'unavailable') {
          this.rows = [];
          this.groupRows = [];
          this.fileFacts = [];
          this.result = undefined;
          this.unavailable = loaded.unavailable;
          return;
        }
        if (this.pageKind === 'entries') {
          const entryResult = loaded.result as ExploreResult;
          this.result = entryResult;
          this.rows = entryResult.rows;
          this.groupRows = [];
          this.fileFacts = [];
          this.resultFingerprint = fingerprint;
          this.pageAuthority = entryResult;
        } else if (this.pageKind === 'groups') {
          const groupResult = loaded.result as ExploreGroupResult;
          this.result = {
            rows: [],
            totalCount: groupResult.totalCount,
            cacheRevision: groupResult.cacheRevision,
            searchProvenance: groupResult.searchProvenance,
            candidateSnapshotId: groupResult.candidateSnapshotId,
            candidatePoolSaturated: false,
            searchDeletionScope: groupResult.searchDeletionScope,
            nextCursor: groupResult.nextCursor
          };
          this.rows = [];
          this.groupRows = groupResult.rows;
          this.fileFacts = [];
          this.resultFingerprint = '';
          this.pageAuthority = groupResult;
        } else {
          const filesResult = loaded.result as ExploreFilesResult;
          this.result = {
            rows: [], totalCount: filesResult.totalCount,
            cacheRevision: filesResult.cacheRevision,
            searchProvenance: filesResult.searchProvenance,
            candidateSnapshotId: filesResult.candidateSnapshotId,
            candidatePoolSaturated: false,
            nextCursor: filesResult.nextCursor
          };
          this.rows = [];
          this.groupRows = [];
          this.fileFacts = filesResult.files;
          this.resultFingerprint = fingerprint;
          this.pageAuthority = filesResult;
        }
        this.nextCursor = loaded.result.nextCursor;
      })
      .catch((cause: unknown) => {
        if (generation !== this.requestGeneration) return;
        if (cause instanceof DOMException && cause.name === 'AbortError') return;
        this.rows = [];
        this.groupRows = [];
        this.result = undefined;
        this.error = cause instanceof Error ? cause.message : 'Could not load Everything.';
      })
      .finally(() => {
        if (generation === this.requestGeneration) {
          this.loading = false;
          void this.restoreDeepState(generation, restoreEpoch, restoreTargets);
        }
      });
  }

  private failPaging(message: string): PageLoadOutcome {
    this.error = message;
    this.nextCursor = undefined;
    return { status: 'failed' };
  }

  private async performLoadMore(): Promise<PageLoadOutcome> {
    if (!this.nextCursor || !this.pagePredicate || !this.requestController || !this.pageAuthority) {
      return { status: 'exhausted' };
    }
    const generation = this.requestGeneration;
    const cursor = this.nextCursor;
    if (this.seenCursors.has(cursor)) {
      return this.failPaging(REPEATED_CURSOR_NOTICE);
    }
    const first = this.pageAuthority;
    const previousCount = this.pageKind === 'groups'
      ? this.groupRows.length
      : this.pageKind === 'files' ? this.fileFacts.length : this.rows.length;
    this.loadingMore = true;
    this.error = '';
    try {
      const predicate = { ...this.pagePredicate, cursor };
      const loaded = this.pageKind === 'groups' && this.pageGrouping
        ? (this.pageGroupsFiles
          ? await this.api.fileGroups(
            predicate, this.pageFileFilenameQuery, this.pageFileMIMEFamilies,
            this.pageGrouping, this.requestController.signal
          )
          : await this.api.groups(predicate, this.pageGrouping, this.requestController.signal))
        : this.pageKind === 'files'
          ? await this.api.files({ ...predicate, grouping: undefined }, this.requestController.signal)
          : await this.api.explore({ ...predicate, grouping: undefined, presentation: 'table' }, this.requestController.signal);
      if (generation !== this.requestGeneration) return { status: 'stale' };
      if (loaded.status === 'unavailable') {
        this.unavailable = loaded.unavailable;
        this.nextCursor = undefined;
        return { status: 'unavailable' };
      }
      if (!samePageAuthority(loaded.result, first)) {
        return this.failPaging(REVISION_CHANGED_NOTICE);
      }
      this.seenCursors.add(cursor);
      const followingCursor = loaded.result.nextCursor;
      if (this.pageKind === 'entries') {
        const entryResult = loaded.result as ExploreResult;
        const merged = new Map(this.rows.map((row) => [row.key, row]));
        for (const row of entryResult.rows) merged.set(row.key, row);
        this.rows = [...merged.values()];
        this.nextCursor = followingCursor;
        this.result = { ...entryResult, rows: this.rows, nextCursor: followingCursor };
      } else if (this.pageKind === 'groups') {
        const groupResult = loaded.result as ExploreGroupResult;
        const merged = new Map(this.groupRows.map((row) => [row.key, row]));
        for (const row of groupResult.rows) merged.set(row.key, row);
        this.groupRows = [...merged.values()];
        this.nextCursor = followingCursor;
      } else {
        const filesResult = loaded.result as ExploreFilesResult;
        const merged = new Map(this.fileFacts.map((file) => [file.key, file]));
        for (const file of filesResult.files) merged.set(file.key, file);
        this.fileFacts = [...merged.values()];
        this.nextCursor = followingCursor;
        this.result = this.result ? { ...this.result, nextCursor: followingCursor } : this.result;
      }
      const currentCount = this.pageKind === 'groups'
        ? this.groupRows.length
        : this.pageKind === 'files' ? this.fileFacts.length : this.rows.length;
      if (followingCursor && (followingCursor === cursor || this.seenCursors.has(followingCursor))) {
        return this.failPaging(REPEATED_CURSOR_NOTICE);
      }
      if (followingCursor && currentCount === previousCount) {
        return this.failPaging(NO_PROGRESS_NOTICE);
      }
      return followingCursor ? { status: 'advanced' } : { status: 'exhausted' };
    } catch (cause: unknown) {
      if (generation !== this.requestGeneration) return { status: 'stale' };
      if (cause instanceof DOMException && cause.name === 'AbortError') return { status: 'aborted' };
      this.error = cause instanceof Error ? cause.message : 'Could not load more Everything results.';
      return { status: 'failed' };
    } finally {
      if (generation === this.requestGeneration) this.loadingMore = false;
    }
  }

  /** Dedupes concurrent callers (e.g. a keyboard repeat and a scroll-edge
   * trigger firing in the same tick) onto one in-flight page request. */
  loadMore = (): Promise<PageLoadOutcome> => {
    if (this.pageRequest) return this.pageRequest;
    const pending = this.performLoadMore();
    this.pageRequest = pending;
    void pending.finally(() => {
      if (this.pageRequest === pending) this.pageRequest = undefined;
    });
    return pending;
  };

  /** Drains cursor pages until exhausted, capped at
   * `LOAD_THROUGH_END_MAX_PAGES` per press so a huge view cannot spiral into
   * thousands of round-trips; pressing End again continues from the cursor. */
  loadThroughEnd = async (): Promise<void> => {
    let pages = 0;
    while (this.nextCursor && !this.requestController?.signal.aborted) {
      if (pages >= LOAD_THROUGH_END_MAX_PAGES) {
        this.callbacks.onPagingNotice(END_PAUSE_NOTICE);
        return;
      }
      const outcome = await this.loadMore();
      if (outcome.status !== 'advanced') break;
      pages += 1;
    }
    if (!this.nextCursor) this.callbacks.onPagingNotice(null);
  };

  private restorationKeys(): string[] {
    const current = this.state.current;
    const selectedAttachmentID = parseAttachmentSelection(current.selectedRow);
    const selected = selectedAttachmentID === undefined ? current.selectedRow : null;
    return [...new Set([
      current.activeRow,
      current.scrollAnchor?.key,
      this.pageKind !== 'files' && selected && !parseGroupSelection(selected) ? selected : undefined
    ].filter((key): key is string => Boolean(key)))];
  }

  private hasRestorationKey(key: string): boolean {
    if (this.pageKind === 'groups' && this.pageGrouping) {
      return this.groupRows.some((row) => `group:${this.pageGrouping}:${row.key}` === key);
    }
    if (this.pageKind === 'files') return this.fileFacts.some((file) => file.key === key);
    return this.rows.some((row) => row.key === key);
  }

  private async restoreDeepState(
    generation: number,
    epoch: number | undefined,
    keys: string[]
  ): Promise<void> {
    if (epoch === undefined || keys.length === 0) {
      if (generation === this.requestGeneration) {
        const shouldRestoreFocus = epoch !== undefined && this.restorationCycleCompleted &&
          this.state.current.selectedRow === null;
        if (epoch !== undefined) this.state.acknowledgeRestoration(epoch);
        this.restoring = false;
        this.restorationCycleCompleted = true;
        if (shouldRestoreFocus) {
          await tick();
          this.callbacks.onRestorationFocus();
        }
      }
      return;
    }
    while (
      generation === this.requestGeneration &&
      this.state.peekRestorationEpoch() === epoch &&
      this.nextCursor &&
      keys.some((key) => !this.hasRestorationKey(key))
    ) {
      const outcome = await this.loadMore();
      if (outcome.status === 'advanced') continue;
      if (outcome.status !== 'exhausted') return;
      break;
    }
    if (
      generation === this.requestGeneration &&
      this.state.peekRestorationEpoch() === epoch &&
      (keys.every((key) => this.hasRestorationKey(key)) ||
        (this.pageAuthority !== undefined && !this.nextCursor && !this.error && !this.unavailable))
    ) {
      await tick();
      if (generation === this.requestGeneration && this.state.peekRestorationEpoch() === epoch) {
        this.state.acknowledgeRestoration(epoch);
        this.restoring = false;
        this.restorationCycleCompleted = true;
        if (this.state.current.selectedRow === null) this.callbacks.onRestorationFocus();
      }
    }
  }

  /** Forces a fresh load of the current predicate (e.g. a named retry
   * action after a failed request), without changing any URL state. */
  retry = (): void => {
    this.#retryRevision += 1;
  };

  destroy = (): void => {
    this.requestGeneration += 1;
    this.requestController?.abort();
  };
}
