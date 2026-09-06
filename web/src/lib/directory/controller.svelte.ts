import {
  createPerson as generatedCreatePerson,
  getPersonContactState as generatedGetPersonContactState,
  getPersonProfile as generatedGetPersonProfile,
  getPersonStructuredProfile as generatedGetPersonStructuredProfile,
  listDirectoryPeople as generatedListDirectoryPeople,
  listPersonActivityDays as generatedListPersonActivityDays,
  listPersonAttributes as generatedListPersonAttributes,
} from '../api/generated/api/api';
import { searchPersonFiles as generatedSearchPersonFiles } from '../api/generated/exploration/exploration';
import type { APIClient } from '../api/client';
import type {
  DirectoryPerson,
  DirectoryPersonSummaryUpdate,
  DirectoryPromotionResult,
  DirectoryReadBundle,
  DirectoryReadSection,
  DirectoryURLState,
} from './models';
import { DirectoryProfileController } from './profile-controller.svelte';
import { DirectoryEntityController } from './entity-controller.svelte';
import type { PersonSplitCommittedContext } from './person-merge-history-controller.svelte';
const PAGE_LIMIT = 50;
const REPEATED_CURSOR_MESSAGE = 'Pagination stopped because the server repeated a cursor without progress.';
/**
 * 'push' adds a history entry (selects, selection changes). 'replace' rewrites
 * the current entry so typed text filters do not leave one entry per keystroke
 * burst for Back to walk through.
 */
export type DirectoryHistoryMode = 'push' | 'replace';
type DirectoryCommit = (patch: Partial<DirectoryURLState>, history: DirectoryHistoryMode) => void;
type DirectoryFilters = Omit<DirectoryURLState, 'directoryPersonID'>;
interface DirectorySummaryOverlay {
  displayName?: string | null;
  revision?: number;
  update?: DirectoryPersonSummaryUpdate;
}
/**
 * Browser-independent data owner for the Directory. URL state stays in
 * ExploreState; this class deliberately keeps cursors, requests and errors
 * ephemeral so a back/forward restoration always starts at page one.
 */
export class DirectoryController {
  query = $state('');
  contactState = $state('');
  category = $state('');
  organization = $state('');
  primaryChannel = $state('');
  lastContactAfter = $state('');
  lastContactBefore = $state('');
  sort = $state<DirectoryURLState['directorySort']>('name');
  rows = $state<DirectoryPerson[]>([]);
  cursor = $state<string | null>(null);
  loading = $state(false);
  loadingMore = $state(false);
  error = $state<string | null>(null);
  pageError = $state<string | null>(null);
  pageRecovery = $state<'retry' | 'reload' | null>(null);
  selectedPersonID = $state<number | null>(null);
  detail = $state<DirectoryReadBundle | null>(null);
  detailLoading = $state(false);
  profile = $state<DirectoryProfileController | null>(null);
  entity = $state<DirectoryEntityController | null>(null);
  promotionError = $state<string | null>(null);
  promotionResult = $state<DirectoryPromotionResult | null>(null);
  private readonly client: APIClient;
  private readonly commit: DirectoryCommit;
  private pageAbort: AbortController | undefined;
  private detailAbort: AbortController | undefined;
  private promotionAbort: AbortController | undefined;
  private pageGeneration = 0;
  private detailGeneration = 0;
  private promotionGeneration = 0;
  private seenCursors = new Set<string>();
  constructor(client: APIClient, commit: DirectoryCommit = () => undefined) {
    this.client = client;
    this.commit = commit;
  }
  /**
   * Applies Directory URL state. History restoration always starts a new page
   * one request because no cursor or accumulated rows belong to a URL entry.
   */
  applyURLState(state: DirectoryURLState, historyRestoration = false): void {
    const filtersChanged =
      this.query !== state.directoryQuery ||
      this.contactState !== state.directoryContactState ||
      this.category !== state.directoryCategory ||
      this.organization !== state.directoryOrganization ||
      this.primaryChannel !== state.directoryPrimaryChannel ||
      this.lastContactAfter !== state.directoryLastContactAfter ||
      this.lastContactBefore !== state.directoryLastContactBefore ||
      this.sort !== state.directorySort;
    this.assignFilters(state);
    // The initial restoration can equal the controller's empty defaults; it
    // still needs page one, just like a back/forward restoration does.
    if (filtersChanged || this.pageGeneration === 0 || historyRestoration) void this.loadFirstPage();
    if (this.selectedPersonID !== state.directoryPersonID) {
      void this.loadSelection(state.directoryPersonID, false);
    }
  }
  /** Updates URL-owned filters and discards the old cursor/request context. */
  setFilters(patch: Partial<DirectoryFilters>, history: DirectoryHistoryMode = 'push'): void {
    const next = { ...this.urlState(), ...patch };
    const filtersChanged =
      this.query !== next.directoryQuery ||
      this.contactState !== next.directoryContactState ||
      this.category !== next.directoryCategory ||
      this.organization !== next.directoryOrganization ||
      this.primaryChannel !== next.directoryPrimaryChannel ||
      this.lastContactAfter !== next.directoryLastContactAfter ||
      this.lastContactBefore !== next.directoryLastContactBefore ||
      this.sort !== next.directorySort;
    if (!filtersChanged) return;
    this.assignFilters(next);
    this.commit(patch, history);
    void this.loadFirstPage();
  }
  async loadFirstPage(): Promise<void> {
    return this.loadPageOne(false);
  }
  /** Explicit same-query Reload is the only caller allowed to keep rows after a failure. */
  async reloadFirstPage(): Promise<void> {
    return this.loadPageOne(true);
  }
  /**
   * The previous rows stay visible while page one is in flight so a filter
   * edit does not flash an empty list; they are replaced on success. On
   * failure only a same-query Reload keeps them, because rows from other
   * filters would misrepresent the failed query.
   */
  private async loadPageOne(allowRetainedRows: boolean): Promise<void> {
    const retainRows = allowRetainedRows && this.pageRecovery === 'reload' && this.rows.length > 0;
    const { controller, generation } = this.beginPageSequence();
    this.loading = true;
    this.loadingMore = false;
    this.error = null;
    this.pageError = null;
    this.pageRecovery = null;
    this.cursor = null;
    this.seenCursors = new Set<string>();
    try {
      const response = await this.getPage(undefined, controller.signal);
      if (controller.signal.aborted || generation !== this.pageGeneration) return;
      if (response.data) {
        this.rows = response.data.people ?? [];
        this.cursor = response.data.next_cursor ?? null;
        return;
      }
      this.failPageOne(errorMessage(response.error, response.response.status), retainRows);
    } catch (cause: unknown) {
      if (!controller.signal.aborted && generation === this.pageGeneration) {
        this.failPageOne(errorMessage(cause, 0), retainRows);
      }
    } finally {
      if (generation === this.pageGeneration) this.loading = false;
    }
  }
  private failPageOne(message: string, retainRows: boolean): void {
    if (retainRows) {
      this.pageError = message;
      this.pageRecovery = 'reload';
      return;
    }
    this.rows = [];
    this.error = message;
  }
  async loadNextPage(): Promise<void> {
    if (this.loading || this.loadingMore || this.pageRecovery === 'reload' || !this.cursor) return;
    const cursor = this.cursor;
    if (this.seenCursors.has(cursor)) {
      this.cursor = null;
      this.pageError = REPEATED_CURSOR_MESSAGE;
      this.pageRecovery = 'reload';
      return;
    }
    const controller = this.pageAbort;
    if (!controller) return;
    const generation = this.pageGeneration;
    this.loadingMore = true;
    this.pageError = null;
    this.pageRecovery = null;
    try {
      const response = await this.getPage(cursor, controller.signal);
      if (controller.signal.aborted || generation !== this.pageGeneration) return;
      if (response.data) {
        this.seenCursors.add(cursor);
        this.rows = mergeRows(this.rows, response.data.people ?? []);
        this.cursor = response.data.next_cursor ?? null;
        return;
      }
      if (isRetryableStatus(response.response.status)) {
        this.pageRecovery = 'retry';
      } else {
        this.cursor = null;
        this.pageRecovery = 'reload';
      }
      this.pageError = errorMessage(response.error, response.response.status);
    } catch (cause: unknown) {
      if (!controller.signal.aborted && generation === this.pageGeneration) {
        // Network failures are retryable: preserve rows and cursor.
        this.pageError = errorMessage(cause, 0);
        this.pageRecovery = 'retry';
      }
    } finally {
      if (generation === this.pageGeneration) this.loadingMore = false;
    }
  }
  async selectPerson(personID: number | null): Promise<void> {
    await this.loadSelection(personID, true);
  }
  /** Re-read server-owned Directory projections after a committed split. */
  async reconcilePersonSplit(_context: PersonSplitCommittedContext): Promise<void> {
    await this.reloadFirstPage();
    const failure = this.pageError ?? this.error;
    if (failure) throw new Error(failure);
  }
  async promote(participantID: number): Promise<DirectoryPromotionResult> {
    this.promotionAbort?.abort();
    const controller = new AbortController();
    this.promotionAbort = controller;
    const generation = ++this.promotionGeneration;
    this.promotionError = null;
    this.promotionResult = null;
    try {
      const response = await generatedCreatePerson(
        { participant_id: participantID },
        {
          ...this.client,
          signal: controller.signal,
        },
      );
      if (!this.isCurrentPromotion(generation, controller)) return stalePromotionResult();
      if (response.data && (response.response.status === 200 || response.response.status === 201)) {
        const personID = response.data.id;
        await Promise.all([this.selectPerson(personID), this.loadFirstPage()]);
        if (!this.isCurrentPromotion(generation, controller)) return stalePromotionResult();
        const result = { ok: true, personID } satisfies DirectoryPromotionResult;
        this.promotionResult = result;
        return result;
      }
      const code = errorCode(response.error);
      const message = errorMessage(response.error, response.response.status);
      this.promotionError = message;
      const result: DirectoryPromotionResult =
        code === 'person_binding_conflict' ? { ok: false, code, message } : { ok: false, code: 'error', message };
      this.promotionResult = result;
      return result;
    } catch (cause: unknown) {
      if (!this.isCurrentPromotion(generation, controller)) return stalePromotionResult();
      const message = errorMessage(cause, 0);
      this.promotionError = message;
      const result = { ok: false, code: 'error', message } satisfies DirectoryPromotionResult;
      this.promotionResult = result;
      return result;
    }
  }
  /** Clears person-specific state before a new relationship promotion context. */
  resetForPromotion(): void {
    this.promotionAbort?.abort();
    this.promotionAbort = undefined;
    ++this.promotionGeneration;
    this.detailAbort?.abort();
    ++this.detailGeneration;
    this.profile?.destroy();
    this.profile = null;
    this.entity?.destroy();
    this.entity = null;
    this.selectedPersonID = null;
    this.detail = null;
    this.detailLoading = false;
    this.promotionError = null;
    this.promotionResult = null;
  }
  destroy(): void {
    this.pageAbort?.abort();
    this.detailAbort?.abort();
    this.promotionAbort?.abort();
    this.profile?.destroy();
    this.entity?.destroy();
  }
  private urlState(): DirectoryURLState {
    return {
      directoryQuery: this.query,
      directoryContactState: this.contactState,
      directoryCategory: this.category,
      directoryOrganization: this.organization,
      directoryPrimaryChannel: this.primaryChannel,
      directoryLastContactAfter: this.lastContactAfter,
      directoryLastContactBefore: this.lastContactBefore,
      directorySort: this.sort,
      directoryPersonID: this.selectedPersonID,
    };
  }
  private assignFilters(state: DirectoryFilters): void {
    this.query = state.directoryQuery;
    this.contactState = state.directoryContactState;
    this.category = state.directoryCategory;
    this.organization = state.directoryOrganization;
    this.primaryChannel = state.directoryPrimaryChannel;
    this.lastContactAfter = state.directoryLastContactAfter;
    this.lastContactBefore = state.directoryLastContactBefore;
    this.sort = state.directorySort;
  }
  private getPage(cursor: string | undefined, signal: AbortSignal) {
    return this.getPageForFilters(this.urlState(), cursor, signal);
  }
  private getPageForFilters(filters: DirectoryURLState, cursor: string | undefined, signal: AbortSignal) {
    const lastContactAfter = directoryDateBoundary(filters.directoryLastContactAfter, false);
    const lastContactBefore = directoryDateBoundary(filters.directoryLastContactBefore, true);
    const query = {
      ...(filters.directoryQuery.trim() ? { q: filters.directoryQuery.trim() } : {}),
      ...(cursor ? { cursor } : {}),
      limit: PAGE_LIMIT,
      ...(filters.directoryContactState ? { contact_state: filters.directoryContactState } : {}),
      ...(filters.directoryCategory ? { category: filters.directoryCategory } : {}),
      ...(filters.directoryOrganization ? { organization: filters.directoryOrganization } : {}),
      ...(filters.directoryPrimaryChannel ? { primary_channel: filters.directoryPrimaryChannel } : {}),
      ...(lastContactAfter ? { last_contact_after: lastContactAfter } : {}),
      ...(lastContactBefore ? { last_contact_before: lastContactBefore } : {}),
      ...(filters.directorySort !== 'name' ? { sort: filters.directorySort } : {}),
    };
    return generatedListDirectoryPeople(query, {
      ...this.client,
      signal,
    });
  }
  private async loadSelection(personID: number | null, shouldCommit: boolean): Promise<void> {
    this.detailAbort?.abort();
    ++this.detailGeneration;
    if (personID === null) {
      this.profile?.destroy();
      this.profile = null;
      this.entity?.destroy();
      this.entity = null;
      this.selectedPersonID = null;
      this.detail = null;
      this.detailLoading = false;
      if (shouldCommit) this.commit({ directoryPersonID: null }, 'push');
      return;
    }
    const controller = new AbortController();
    this.profile?.destroy();
    this.profile = null;
    this.entity?.destroy();
    // Employments, relationships and the network belong to the entity
    // controller; the read bundle never duplicates those requests.
    this.entity = new DirectoryEntityController(this.client, personID, {
      onDirectoryChange: () => this.refreshLoadedPagesAfterSummaryWrite(personID, {}),
    });
    void this.entity.load();
    this.detailAbort = controller;
    const generation = this.detailGeneration;
    this.selectedPersonID = personID;
    this.detail = null;
    this.detailLoading = true;
    if (shouldCommit) this.commit({ directoryPersonID: personID }, 'push');
    const requestOptions = { ...this.client, signal: controller.signal };
    try {
      const [person, structuredProfile, attributes, contactState, activity, files] = await Promise.all([
        settleSection(generatedGetPersonProfile({ id: personID }, requestOptions)),
        settleSection(generatedGetPersonStructuredProfile({ id: personID }, requestOptions)),
        settleSection(
          generatedListPersonAttributes(
            { id: personID },
            { history: true },
            {
              ...requestOptions,
            },
          ),
        ),
        settleSection(generatedGetPersonContactState({ id: personID }, requestOptions)),
        settleSection(
          generatedListPersonActivityDays(
            { id: personID },
            { limit: 1 },
            {
              ...requestOptions,
            },
          ),
        ),
        settleSection(
          generatedSearchPersonFiles(
            { id: personID },
            {
              predicate: {
                filters: [],
                grouping: [],
                presentation: 'files',
                sort: [{ field: 'occurred_at', direction: 'desc' }],
                limit: 1,
              },
              sort: { field: 'occurred_at', direction: 'desc' },
              limit: 1,
            },
            {
              ...requestOptions,
            },
          ),
        ),
      ]);
      if (controller.signal.aborted || generation !== this.detailGeneration) return;
      if (!person.data && person.response?.status === 404) {
        this.entity?.destroy();
        this.entity = null;
        this.selectedPersonID = null;
        this.detail = null;
        this.commit({ directoryPersonID: null }, 'push');
        return;
      }
      const bundle: DirectoryReadBundle = { etags: {}, errors: {} };
      applySection(bundle, 'person', person);
      applySection(bundle, 'structuredProfile', structuredProfile);
      applySection(bundle, 'attributes', attributes);
      applySection(bundle, 'contactState', contactState);
      applySection(bundle, 'activity', activity);
      applySection(bundle, 'files', files);
      this.detail = bundle;
      let profileController: DirectoryProfileController;
      profileController = new DirectoryProfileController(this.client, personID, bundle, {
        invalidateRow: (id, update, refreshDirectory) =>
          this.reconcileDirectoryRow(
            id,
            {
              displayName:
                refreshDirectory && profileController.person
                  ? (profileController.person.display_name ?? null)
                  : undefined,
              revision: profileController.person?.revision,
              update,
            },
            refreshDirectory,
          ),
        onDetailChange: (updated) => {
          if (this.profile !== profileController || this.selectedPersonID !== personID) return;
          this.detail = {
            ...updated,
            etags: { ...updated.etags },
            errors: { ...updated.errors },
          };
        },
        onDeleted: (id) => this.removeDeletedPerson(id),
      });
      this.profile = profileController;
    } catch (cause: unknown) {
      if (!controller.signal.aborted && generation === this.detailGeneration) {
        this.detail = { etags: {}, errors: { person: errorMessage(cause, 0) } };
      }
    } finally {
      if (generation === this.detailGeneration) this.detailLoading = false;
    }
  }
  private async reconcileDirectoryRow(
    personID: number,
    overlay: DirectorySummaryOverlay,
    refreshDirectory = false,
  ): Promise<void> {
    const { update } = overlay;
    const filteredCategoryChanged = update?.categories !== undefined && this.category.trim() !== '';
    if (refreshDirectory || filteredCategoryChanged) {
      await this.refreshLoadedPagesAfterSummaryWrite(personID, overlay);
      return;
    }
    this.rows = this.rows.map((row) =>
      row.id === personID
        ? updateDirectorySummary(row, overlay.displayName, overlay.revision ?? row.revision, update)
        : row,
    );
  }
  private removeDeletedPerson(personID: number): void {
    this.rows = this.rows.filter((row) => row.id !== personID);
    if (this.selectedPersonID !== personID) return;
    this.detailAbort?.abort();
    ++this.detailGeneration;
    this.profile?.destroy();
    this.profile = null;
    this.entity?.destroy();
    this.entity = null;
    this.selectedPersonID = null;
    this.detail = null;
    this.detailLoading = false;
    this.commit({ directoryPersonID: null }, 'push');
  }
  /**
   * Category filters use Go-normalized projection keys. Re-read the already
   * loaded page depth instead of approximating that normalization or
   * collapsing pagination to page one.
   */
  private async refreshLoadedPagesAfterSummaryWrite(personID: number, overlay: DirectorySummaryOverlay): Promise<void> {
    const pageCount = Math.max(1, this.seenCursors.size + 1);
    const filters = this.urlState();
    const { controller, generation } = this.beginPageSequence();
    this.loadingMore = true;
    const rows: DirectoryPerson[] = [];
    const consumedCursors = new Set<string>();
    let cursor: string | undefined;
    let nextCursor: string | null = null;
    const ownsReconciliation = () =>
      this.ownsPageSequence(generation, controller) && sameDirectoryFilters(filters, this.urlState());
    try {
      for (let page = 0; page < pageCount; page += 1) {
        const response = await this.getPageForFilters(filters, cursor, controller.signal);
        if (!ownsReconciliation()) return;
        if (!response.data) {
          this.pageError = errorMessage(response.error, response.response.status);
          this.pageRecovery = 'reload';
          return;
        }
        rows.push(...(response.data.people ?? []).filter((row) => !rows.some((candidate) => candidate.id === row.id)));
        nextCursor = response.data.next_cursor ?? null;
        if (!nextCursor) break;
        cursor = nextCursor;
        if (page + 1 < pageCount) consumedCursors.add(nextCursor);
      }
    } catch (cause: unknown) {
      if (ownsReconciliation()) {
        this.pageError = errorMessage(cause, 0);
        this.pageRecovery = 'reload';
      }
      return;
    } finally {
      if (generation === this.pageGeneration) this.loadingMore = false;
    }
    if (!ownsReconciliation()) return;
    const reconciledRows = rows.map((row) => {
      if (row.id !== personID) return row;
      if (overlay.revision !== undefined && row.revision > overlay.revision) return row;
      return updateDirectorySummary(row, overlay.displayName, overlay.revision ?? row.revision, overlay.update);
    });
    this.rows = reconciledRows;
    this.cursor = nextCursor;
    this.seenCursors = consumedCursors;
    this.pageError = null;
    this.pageRecovery = null;
  }
  private beginPageSequence(): {
    controller: AbortController;
    generation: number;
  } {
    this.pageAbort?.abort();
    const controller = new AbortController();
    this.pageAbort = controller;
    return { controller, generation: ++this.pageGeneration };
  }
  private ownsPageSequence(generation: number, controller: AbortController): boolean {
    return !controller.signal.aborted && generation === this.pageGeneration && this.pageAbort === controller;
  }
  private isCurrentPromotion(generation: number, controller: AbortController): boolean {
    return !controller.signal.aborted && generation === this.promotionGeneration;
  }
}
function updateDirectorySummary(
  row: DirectoryPerson,
  displayName: string | null | undefined,
  revision: number,
  update?: DirectoryPersonSummaryUpdate,
): DirectoryPerson {
  return {
    ...row,
    display_name: displayName === undefined ? row.display_name : (displayName ?? undefined),
    revision,
    ...(update?.categories === undefined ? {} : { categories: update.categories }),
  };
}
function sameDirectoryFilters(left: DirectoryURLState, right: DirectoryURLState): boolean {
  return (
    left.directoryQuery === right.directoryQuery &&
    left.directoryContactState === right.directoryContactState &&
    left.directoryCategory === right.directoryCategory &&
    left.directoryOrganization === right.directoryOrganization &&
    left.directoryPrimaryChannel === right.directoryPrimaryChannel &&
    left.directoryLastContactAfter === right.directoryLastContactAfter &&
    left.directoryLastContactBefore === right.directoryLastContactBefore &&
    left.directorySort === right.directorySort
  );
}
function directoryDateBoundary(value: string, endOfDay: boolean): string | undefined {
  const trimmed = value.trim();
  if (!/^\d{4}-\d{2}-\d{2}$/.test(trimmed)) return undefined;
  return `${trimmed}T${endOfDay ? '23:59:59.999999999' : '00:00:00'}Z`;
}
function applySection(bundle: DirectoryReadBundle, section: DirectoryReadSection, result: SectionResult): void {
  if (result.data !== undefined && result.response) {
    const etag = result.response.headers.get('ETag');
    if (etag) bundle.etags[section] = etag;
    Object.assign(bundle, { [section]: result.data });
    return;
  }
  bundle.errors[section] = errorMessage(result.rejection ?? result.error, result.response?.status ?? 0);
}
interface SectionResult {
  data?: unknown;
  error?: unknown;
  response?: Response;
  rejection?: unknown;
}
async function settleSection<
  T extends {
    data?: unknown;
    error?: unknown;
    response: Response;
  },
>(request: Promise<T>): Promise<SectionResult> {
  try {
    return await request;
  } catch (rejection: unknown) {
    return { rejection };
  }
}
function mergeRows(current: DirectoryPerson[], incoming: DirectoryPerson[]): DirectoryPerson[] {
  const seen = new Set(current.map((row) => row.id));
  return [...current, ...incoming.filter((row) => !seen.has(row.id))];
}
function isRetryableStatus(status: number): boolean {
  return status === 0 || status === 408 || status === 429 || status >= 500;
}
function errorCode(error: unknown): string | undefined {
  return typeof error === 'object' && error !== null && 'error' in error && typeof error.error === 'string'
    ? error.error
    : undefined;
}
function errorMessage(error: unknown, status: number): string {
  if (typeof error === 'object' && error !== null && 'message' in error && typeof error.message === 'string') {
    return error.message;
  }
  if (error instanceof Error && error.message) return error.message;
  return status ? `Request failed (${status})` : 'Request failed';
}
function stalePromotionResult(): DirectoryPromotionResult {
  return { ok: false, code: 'error', message: 'Promotion was superseded.' };
}
