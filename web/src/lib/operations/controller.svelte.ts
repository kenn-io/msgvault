import {
  getOperationRun as generatedGetOperationRun,
  getOperationStatus as generatedGetOperationStatus,
  listOperationRuns as generatedListOperationRuns,
  resumeVisualAttachmentBuild as generatedResumeVisualAttachmentBuild,
  startVisualAttachmentBuild as generatedStartVisualAttachmentBuild,
  syncCardDAV as generatedSyncCardDAV,
} from '../api/generated/api/api';
import type { APIClient } from '../api/client';
import type {
  OperationAction,
  OperationActionOutcome,
  OperationLane,
  OperationLaneStatus,
  OperationRunDetail,
  OperationRunSummary,
  OperationsSnapshot,
  OperationsURLState,
  OperationUnavailableKind
} from './models';

const PAGE_LIMIT = 25;
const LANE_ORDER: readonly OperationLane[] = [
  'messages', 'person_facts', 'contacts', 'documents', 'visual_attachments'
];
const HISTORY_CONFLICT = 'Operation history changed. Restart from the first page.';

export class OperationsController {
  private statusKinds = $state<OperationLaneStatus[]>([]);
  private rows = $state<OperationRunSummary[]>([]);
  private unavailableKinds = $state<OperationUnavailableKind[]>([]);
  private detail = $state<OperationRunDetail>();
  private membershipRevision = $state<number>();
  private nextCursor = $state<string>();
  private statusReadable = $state(false);
  private historyReadable = $state(false);

  private initialLoading = $state(false);
  private backgroundLoading = $state(false);
  private paging = $state(false);
  private detailLoading = $state(false);
  private statusError = $state<string | null>(null);
  private runsError = $state<string | null>(null);
  private detailError = $state<string | null>(null);
  private conflict = $state<string | null>(null);
  private restartRequired = $state(false);
  private actionPending = $state<OperationAction>();
  private actionConflict = $state<string | null>(null);
  private actionError = $state<string | null>(null);

  private readonly client: APIClient;
  private readonly commitURLState: (patch: Pick<OperationsURLState, 'operationRunID'>) => void;
  private filters: OperationsURLState = emptyURLState();
  private filterSignature = '';
  private archiveContext = '';
  private selectedRunID: string | null = null;
  private loaded = false;
  private disposed = false;
  private contextGeneration = 0;
  private readGeneration = 0;
  private detailGeneration = 0;
  private pageGeneration = 0;
  private actionGeneration = 0;
  private readAbort?: AbortController;
  private detailAbort?: AbortController;
  private pageAbort?: AbortController;
  private actionAbort?: AbortController;

  constructor(
    client: APIClient,
    commitURLState: (patch: Pick<OperationsURLState, 'operationRunID'>) => void = () => undefined
  ) {
    this.client = client;
    this.commitURLState = commitURLState;
  }

  get snapshot(): Readonly<OperationsSnapshot> {
    return Object.freeze({
      statusLanes: groupedStatus(this.statusKinds),
      rows: this.rows,
      unavailableKinds: this.unavailableKinds,
      detail: this.detail ?? null,
      membershipRevision: this.membershipRevision ?? null,
      nextCursor: this.nextCursor ?? null,
      statusReadable: this.statusReadable,
      historyReadable: this.historyReadable,
      initialLoading: this.initialLoading,
      backgroundLoading: this.backgroundLoading,
      paging: this.paging,
      detailLoading: this.detailLoading,
      statusError: this.statusError,
      runsError: this.runsError,
      detailError: this.detailError,
      conflict: this.conflict,
      restartRequired: this.restartRequired,
      actionPending: this.actionPending ?? null,
      actionConflict: this.actionConflict,
      actionError: this.actionError
    });
  }

  async applyURLState(state: OperationsURLState, archiveContext = ''): Promise<void> {
    if (this.disposed) return;
    const filters = copyURLState(state);
    const signature = filterFingerprint(filters);
    const archiveChanged = archiveContext !== this.archiveContext;
    const clearSelectionForArchive = archiveChanged && this.filterSignature !== '' &&
      filters.operationRunID !== null;
    if (clearSelectionForArchive) filters.operationRunID = null;
    const filtersChanged = signature !== this.filterSignature;
    const selectionChanged = filters.operationRunID !== this.selectedRunID;

    if (archiveChanged || filtersChanged || !this.loaded) {
      this.invalidateContext();
      this.filters = filters;
      this.filterSignature = signature;
      this.archiveContext = archiveContext;
      this.selectedRunID = filters.operationRunID;
      this.rows = [];
      this.unavailableKinds = [];
      this.detail = undefined;
      this.membershipRevision = undefined;
      this.nextCursor = undefined;
      this.historyReadable = false;
      this.conflict = null;
      this.restartRequired = false;
      if (archiveChanged) {
        this.statusKinds = [];
        this.statusReadable = false;
      }
      if (clearSelectionForArchive) this.commitURLState({ operationRunID: null });
      await Promise.all([
        this.loadPageOne(false),
        filters.operationRunID ? this.loadDetail(filters.operationRunID) : Promise.resolve()
      ]);
      return;
    }

    if (selectionChanged) {
      this.selectedRunID = filters.operationRunID;
      if (filters.operationRunID) await this.loadDetail(filters.operationRunID);
      else this.clearDetail();
    }
  }

  async refresh(): Promise<boolean> {
    if (this.disposed) return false;
    return this.loadPageOne(this.loaded);
  }

  async loadMore(): Promise<void> {
    const cursor = this.nextCursor;
    if (this.disposed || this.initialLoading || this.backgroundLoading || this.paging || !cursor) return;
    this.cancelPage();
    const request = new AbortController();
    this.pageAbort = request;
    const context = this.contextGeneration;
    const generation = ++this.pageGeneration;
    const revision = this.membershipRevision;
    this.paging = true;
    this.runsError = null;
    try {
      const result = await generatedListOperationRuns(operationQuery(this.filters, cursor), {
        ...this.client,
        signal: request.signal,
      });
      if (!this.ownsPage(request, generation, context)) return;
      if (result.response.status === 409) {
        this.markHistoryConflict();
        return;
      }
      if (!result.data) {
        this.runsError = 'Unable to load more operation history.';
        return;
      }
      if (revision !== undefined && result.data.membership_revision !== revision) {
        this.markHistoryConflict();
        return;
      }
      const seen = new Set(this.rows.map((row) => row.id));
      this.rows = [...this.rows, ...result.data.runs.filter((row) => !seen.has(row.id))];
      this.unavailableKinds = result.data.unavailable_kinds;
      this.membershipRevision = result.data.membership_revision;
      this.nextCursor = result.data.next_cursor;
    } catch {
      if (this.ownsPage(request, generation, context)) {
        this.runsError = 'Unable to load more operation history.';
      }
    } finally {
      if (this.ownsPage(request, generation, context)) {
        if (this.pageAbort === request) this.pageAbort = undefined;
        this.paging = false;
      }
    }
  }

  async restart(): Promise<void> {
    if (!this.restartRequired || this.disposed) return;
    this.conflict = null;
    this.restartRequired = false;
    await this.refresh();
  }

  async runAction(action: OperationAction): Promise<OperationActionOutcome> {
    if (this.disposed || this.actionPending) return 'discarded';
    if (!this.advertisedActions().has(action)) {
      this.actionError = 'This operation is not currently available.';
      return 'failed';
    }
    this.actionAbort?.abort();
    const request = new AbortController();
    this.actionAbort = request;
    const context = this.contextGeneration;
    const generation = ++this.actionGeneration;
    this.actionPending = action;
    this.actionConflict = null;
    this.actionError = null;
    let reconcile = false;
    let uncertain = false;
    let outcome: OperationActionOutcome = 'succeeded';
    try {
      const result = action === 'carddav_sync'
        ? await generatedSyncCardDAV({ full: false }, {
          ...this.client,
          signal: request.signal,
        })
        : action === 'visual_build'
          ? await generatedStartVisualAttachmentBuild({ consent: true }, {
            ...this.client,
            signal: request.signal,
          })
          : await generatedResumeVisualAttachmentBuild({
            ...this.client,
            signal: request.signal,
          });
      if (!this.ownsAction(request, generation, context)) return 'discarded';
      reconcile = true;
      if (result.response.status === 409) {
        this.actionConflict = 'The operation could not start because current server state changed.';
        outcome = 'conflict';
      } else if (!result.response.ok) {
        this.actionError = 'Unable to start the operation.';
        outcome = 'failed';
      }
    } catch {
      if (this.ownsAction(request, generation, context)) {
        reconcile = true;
        uncertain = true;
        this.actionError = 'The operation result is uncertain.';
        outcome = 'failed';
      }
    }

    if (reconcile && this.ownsAction(request, generation, context)) {
      const readable = await this.refresh();
      const detailReadable = this.selectedRunID ? await this.loadDetail(this.selectedRunID) : undefined;
      if (!this.ownsAction(request, generation, context)) return 'discarded';
      const reconciled = readable && detailReadable !== false;
      if (uncertain) {
        this.actionError = reconciled
          ? 'The operation result is uncertain. Current state was refreshed.'
          : 'The operation result is uncertain, and current state could not be refreshed.';
      } else if (outcome === 'succeeded' && !reconciled) {
        this.actionError = 'The operation started, but current state could not be refreshed.';
        outcome = 'failed';
      }
    }
    if (this.ownsAction(request, generation, context)) {
      if (this.actionAbort === request) this.actionAbort = undefined;
      this.actionPending = undefined;
      return outcome;
    }
    return 'discarded';
  }

  destroy(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.contextGeneration += 1;
    this.readAbort?.abort();
    this.detailAbort?.abort();
    this.cancelPage();
    this.actionAbort?.abort();
  }

  private async loadPageOne(background: boolean): Promise<boolean> {
    this.readAbort?.abort();
    this.cancelPage();
    const request = new AbortController();
    this.readAbort = request;
    const context = this.contextGeneration;
    const generation = ++this.readGeneration;
    this.nextCursor = undefined;
    this.conflict = null;
    this.restartRequired = false;
    this.statusError = null;
    this.runsError = null;
    if (background) this.backgroundLoading = true;
    else this.initialLoading = true;

    const [statusResult, runsResult] = await Promise.allSettled([
      generatedGetOperationStatus({ ...this.client, signal: request.signal }),
      generatedListOperationRuns(operationQuery(this.filters), {
        ...this.client,
        signal: request.signal,
      }),
    ]);
    if (!this.ownsRead(request, generation, context)) return false;

    let statusSucceeded = false;
    if (statusResult.status === 'fulfilled' && statusResult.value.data) {
      this.statusKinds = statusResult.value.data.lanes;
      this.statusReadable = true;
      statusSucceeded = true;
    } else {
      this.statusError = 'Unable to load operation status.';
    }

    let historySucceeded = false;
    if (runsResult.status === 'fulfilled' && runsResult.value.response.status === 409) {
      this.markHistoryConflict();
    } else if (runsResult.status === 'fulfilled' && runsResult.value.data) {
      // Refreshed fields and row positions cannot prove run identity when opaque references rotate.
      this.rows = runsResult.value.data.runs;
      this.unavailableKinds = runsResult.value.data.unavailable_kinds;
      this.membershipRevision = runsResult.value.data.membership_revision;
      this.nextCursor = runsResult.value.data.next_cursor;
      this.historyReadable = true;
      historySucceeded = true;
    } else {
      this.runsError = 'Unable to load operation history.';
    }
    this.loaded = true;
    if (this.readAbort === request) this.readAbort = undefined;
    this.initialLoading = false;
    this.backgroundLoading = false;
    return statusSucceeded && historySucceeded;
  }

  private async loadDetail(id: string): Promise<boolean> {
    this.detailAbort?.abort();
    const request = new AbortController();
    this.detailAbort = request;
    const context = this.contextGeneration;
    const generation = ++this.detailGeneration;
    this.detail = undefined;
    this.detailError = null;
    this.detailLoading = true;
    try {
      const result = await generatedGetOperationRun({ id }, {
        ...this.client,
        signal: request.signal,
      });
      if (!this.ownsDetail(request, generation, context, id)) return false;
      if (result.response.status === 409) {
        this.markHistoryConflict();
      } else if (result.data) {
        this.detail = result.data;
        return true;
      } else {
        this.detailError = 'Unable to load operation detail.';
      }
    } catch {
      if (this.ownsDetail(request, generation, context, id)) {
        this.detailError = 'Unable to load operation detail.';
      }
    } finally {
      if (this.ownsDetail(request, generation, context, id)) {
        if (this.detailAbort === request) this.detailAbort = undefined;
        this.detailLoading = false;
      }
    }
    return false;
  }

  private clearDetail(): void {
    this.detailAbort?.abort();
    this.detailAbort = undefined;
    this.detailGeneration += 1;
    this.detail = undefined;
    this.detailError = null;
    this.detailLoading = false;
  }

  private invalidateContext(): void {
    this.contextGeneration += 1;
    this.readAbort?.abort();
    this.detailAbort?.abort();
    this.cancelPage();
    this.actionAbort?.abort();
    this.initialLoading = false;
    this.backgroundLoading = false;
    this.paging = false;
    this.detailLoading = false;
    this.detailError = null;
    this.actionPending = undefined;
    this.actionConflict = null;
    this.actionError = null;
  }

  private cancelPage(): void {
    const request = this.pageAbort;
    this.pageAbort = undefined;
    this.pageGeneration += 1;
    request?.abort();
    this.paging = false;
  }

  private advertisedActions(): Set<OperationAction> {
    return new Set([
      ...this.statusKinds.flatMap((item) => item.supported_actions),
      ...(this.detail?.supported_actions ?? [])
    ]);
  }

  private markHistoryConflict(): void {
    this.conflict = HISTORY_CONFLICT;
    this.restartRequired = true;
    this.nextCursor = undefined;
  }

  private ownsRead(owner: AbortController, generation: number, context: number): boolean {
    return !this.disposed && !owner.signal.aborted && this.readAbort === owner &&
      this.readGeneration === generation && this.contextGeneration === context;
  }

  private ownsPage(owner: AbortController, generation: number, context: number): boolean {
    return !this.disposed && !owner.signal.aborted && this.pageAbort === owner &&
      this.pageGeneration === generation && this.contextGeneration === context;
  }

  private ownsDetail(owner: AbortController, generation: number, context: number, id: string): boolean {
    return !this.disposed && !owner.signal.aborted && this.detailAbort === owner &&
      this.detailGeneration === generation && this.contextGeneration === context && this.selectedRunID === id;
  }

  private ownsAction(owner: AbortController, generation: number, context: number): boolean {
    return !this.disposed && !owner.signal.aborted && this.actionAbort === owner &&
      this.actionGeneration === generation && this.contextGeneration === context;
  }
}

function emptyURLState(): OperationsURLState {
  return {
    operationLane: '',
    operationKind: '',
    operationState: '',
    operationStartedFrom: '',
    operationStartedBefore: '',
    operationRunID: null,
    operationStatus: ''
  };
}

function copyURLState(state: OperationsURLState): OperationsURLState {
  return {
    operationLane: state.operationLane,
    operationKind: state.operationKind,
    operationState: state.operationState,
    operationStartedFrom: state.operationStartedFrom,
    operationStartedBefore: state.operationStartedBefore,
    operationRunID: state.operationRunID,
    operationStatus: state.operationStatus
  };
}

function filterFingerprint(state: OperationsURLState): string {
  return JSON.stringify([
    state.operationLane,
    state.operationKind,
    state.operationState,
    state.operationStartedFrom,
    state.operationStartedBefore
  ]);
}

function operationQuery(state: OperationsURLState, cursor?: string) {
  return {
    ...(state.operationLane ? { lane: state.operationLane } : {}),
    ...(state.operationKind ? { kind: state.operationKind } : {}),
    ...(state.operationState ? { state: state.operationState } : {}),
    ...(state.operationStartedFrom ? { started_from: state.operationStartedFrom } : {}),
    ...(state.operationStartedBefore ? { started_before: state.operationStartedBefore } : {}),
    limit: PAGE_LIMIT,
    ...(cursor ? { cursor } : {})
  };
}

function groupedStatus(statuses: readonly OperationLaneStatus[]) {
  return LANE_ORDER.map((lane) => Object.freeze({
    lane,
    kinds: statuses.filter((status) => status.lane === lane)
  }));
}
