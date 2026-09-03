import type { APIClient } from '../api/client';
import type { components } from '../api/generated/schema';
import { isMatchingPersonRevisionETag } from './person-merge';

export const PERSON_MERGE_HISTORY_LIMIT = 100;

type Person = components['schemas']['Person'];
type MergeSummary = components['schemas']['PersonMergeSummary'];
type MergeDetail = components['schemas']['PersonMergeDetail'];
type MergeSnapshot = components['schemas']['PersonMergeSnapshotResponse'];
type SplitResult = components['schemas']['PersonSplitResult'];

export interface PersonSplitCommittedContext {
  sourcePersonID: number;
  newPersonID: number;
}

export interface CommittedPersonSplit {
  result: SplitResult;
  receiptETags: { source: string | null; created: string | null };
}

type SplitReconciler = (context: PersonSplitCommittedContext) => void | Promise<void>;

interface ConfirmedSplitSnapshot {
  sourcePersonID: number;
  mergeID: number;
  participantIDs: number[];
  sourceETag: string;
  sourceRevision: number;
}

interface StaleSplitReloadTarget {
  sourcePersonID: number;
  mergeID: number;
}

export class PersonMergeHistoryController {
  personID = $state(0);
  history = $state<MergeSummary[]>([]);
  historyOffset = $state(0);
  historyLoading = $state(false);
  historyError = $state<string | null>(null);
  pendingHistoryOffset = $state<number | null>(null);

  selectedMergeID = $state<number | null>(null);
  detail = $state<MergeDetail | null>(null);
  detailLoading = $state(false);
  detailError = $state<string | null>(null);

  snapshot = $state<MergeSnapshot | null>(null);
  snapshotLoading = $state(false);
  snapshotError = $state<string | null>(null);

  splitOpen = $state(false);
  sourcePerson = $state<Person | null>(null);
  sourceETag = $state<string | null>(null);
  sourceLoading = $state(false);
  sourceError = $state<string | null>(null);
  selectedParticipantIDs = $state<number[]>([]);
  confirmedParticipantIDs = $state<number[] | null>(null);
  splitPending = $state(false);
  splitNeedsFreshState = $state(false);
  splitError = $state<string | null>(null);
  committedResult = $state<CommittedPersonSplit | null>(null);
  reconciliationError = $state<string | null>(null);

  private readonly client: APIClient;
  private readonly reconcile: SplitReconciler;
  private historyAbort: AbortController | undefined;
  private detailAbort: AbortController | undefined;
  private snapshotAbort: AbortController | undefined;
  private splitAbort: AbortController | undefined;
  private historyGeneration = 0;
  private detailGeneration = 0;
  private snapshotGeneration = 0;
  private splitGeneration = 0;
  private contextGeneration = 0;
  private retryKey: string | null = null;
  private retrySnapshot: ConfirmedSplitSnapshot | null = null;
  private confirmedSnapshot: ConfirmedSplitSnapshot | null = null;
  private staleReloadTarget: StaleSplitReloadTarget | null = null;
  private destroyed = false;
  private committedReconciled = false;

  constructor(client: APIClient, personID: number, reconcile: SplitReconciler = () => undefined) {
    this.client = client;
    this.personID = personID;
    this.reconcile = reconcile;
  }

  get hasPreviousHistoryPage(): boolean {
    return this.historyOffset > 0;
  }

  get hasNextHistoryPage(): boolean {
    return this.history.length === PERSON_MERGE_HISTORY_LIMIT;
  }

  get absorbedParticipants() {
    return (this.detail?.participants ?? []).filter((participant) => participant.origin_side === 'absorbed');
  }

  get eligibleParticipantIDs(): number[] {
    return this.absorbedParticipants
      .filter((participant) => participant.split_id === undefined)
      .map((participant) => participant.participant_id)
      .sort((left, right) => left - right);
  }

  get isZeroParticipantLineage(): boolean {
    return this.absorbedParticipants.length === 0;
  }

  get canOfferSplit(): boolean {
    return !!this.detail?.merge.current_person_id && !this.committedResult;
  }

  get splitBusy(): boolean {
    return this.splitPending || this.sourceLoading;
  }

  setPerson(personID: number): void {
    if (personID === this.personID && !this.destroyed) return;
    this.abortAll();
    this.destroyed = false;
    ++this.contextGeneration;
    this.personID = personID;
    this.history = [];
    this.historyOffset = 0;
    this.historyError = null;
    this.historyLoading = false;
    this.pendingHistoryOffset = null;
    this.clearMergeContext();
    void this.loadHistory();
  }

  async loadHistory(offset = this.historyOffset): Promise<void> {
    this.historyAbort?.abort();
    const request = new AbortController();
    this.historyAbort = request;
    const generation = ++this.historyGeneration;
    const context = this.contextGeneration;
    const retainedHistory = this.history;
    this.historyLoading = true;
    this.historyError = null;
    this.pendingHistoryOffset = offset;
    try {
      const response = await this.client.GET('/api/v1/people/{id}/merges', {
        params: { path: { id: this.personID }, query: { limit: PERSON_MERGE_HISTORY_LIMIT, offset } },
        signal: request.signal
      });
      if (!this.ownsHistory(request, generation, context)) return;
      if (response.data) {
        this.history = response.data.merges ?? [];
        this.historyOffset = response.data.offset;
        this.pendingHistoryOffset = null;
      } else {
        this.history = retainedHistory;
        this.historyError = failureMessage(response.error, response.response.status);
      }
    } catch (cause) {
      if (this.ownsHistory(request, generation, context)) {
        this.history = retainedHistory;
        this.historyError = failureMessage(cause, 0);
      }
    } finally {
      if (generation === this.historyGeneration) this.historyLoading = false;
    }
  }

  async nextHistoryPage(): Promise<void> {
    if (!this.hasNextHistoryPage || this.historyLoading) return;
    await this.loadHistory(this.historyOffset + PERSON_MERGE_HISTORY_LIMIT);
  }

  async previousHistoryPage(): Promise<void> {
    if (!this.hasPreviousHistoryPage || this.historyLoading) return;
    await this.loadHistory(Math.max(0, this.historyOffset - PERSON_MERGE_HISTORY_LIMIT));
  }

  async retryHistory(): Promise<void> {
    await this.loadHistory(this.pendingHistoryOffset ?? this.historyOffset);
  }

  async selectMerge(mergeID: number): Promise<void> {
    this.detailAbort?.abort();
    this.snapshotAbort?.abort();
    this.splitAbort?.abort();
    ++this.snapshotGeneration;
    ++this.splitGeneration;
    this.selectedMergeID = mergeID;
    this.detailLoading = true;
    this.detailError = null;
    this.snapshot = null;
    this.snapshotError = null;
    this.snapshotLoading = false;
    this.resetSplit();

    const request = new AbortController();
    this.detailAbort = request;
    const generation = ++this.detailGeneration;
    const context = this.contextGeneration;
    const retainedDetail = this.detail?.merge.id === mergeID ? this.detail : null;
    try {
      const response = await this.client.GET('/api/v1/person-merges/{merge_id}', {
        params: { path: { merge_id: mergeID } }, signal: request.signal
      });
      if (!this.ownsDetail(request, generation, context, mergeID)) return;
      if (response.data) this.detail = response.data;
      else {
        this.detail = retainedDetail;
        this.detailError = failureMessage(response.error, response.response.status);
      }
    } catch (cause) {
      if (this.ownsDetail(request, generation, context, mergeID)) {
        this.detail = retainedDetail;
        this.detailError = failureMessage(cause, 0);
      }
    } finally {
      if (generation === this.detailGeneration) this.detailLoading = false;
    }
  }

  async retryDetail(): Promise<void> {
    if (this.selectedMergeID !== null) await this.selectMerge(this.selectedMergeID);
  }

  async revealSnapshot(): Promise<void> {
    if (this.selectedMergeID === null || this.snapshot || this.snapshotLoading) return;
    this.snapshotAbort?.abort();
    const request = new AbortController();
    this.snapshotAbort = request;
    const generation = ++this.snapshotGeneration;
    const context = this.contextGeneration;
    const mergeID = this.selectedMergeID;
    this.snapshotLoading = true;
    this.snapshotError = null;
    try {
      const response = await this.client.GET('/api/v1/person-merges/{merge_id}/snapshot', {
        params: { path: { merge_id: mergeID } }, signal: request.signal
      });
      if (!this.ownsSnapshot(request, generation, context, mergeID)) return;
      if (response.data) this.snapshot = response.data;
      else this.snapshotError = failureMessage(response.error, response.response.status);
    } catch (cause) {
      if (this.ownsSnapshot(request, generation, context, mergeID)) this.snapshotError = failureMessage(cause, 0);
    } finally {
      if (generation === this.snapshotGeneration) this.snapshotLoading = false;
    }
  }

  async openSplit(): Promise<boolean> {
    const detail = this.detail;
    const sourceID = detail?.merge.current_person_id;
    if (!sourceID || this.committedResult) return false;
    this.splitOpen = true;
    if (this.splitNeedsFreshState && this.staleReloadTarget) return this.retryStaleSplitState();
    this.sourceLoading = true;
    this.sourceError = null;
    this.splitError = null;
    this.sourcePerson = null;
    this.sourceETag = null;
    this.clearConfirmation();
    this.splitAbort?.abort();
    const request = new AbortController();
    this.splitAbort = request;
    const generation = ++this.splitGeneration;
    const context = this.contextGeneration;
    const mergeID = detail.merge.id;
    try {
      const response = await this.client.GET('/api/v1/people/{id}', {
        params: { path: { id: sourceID } }, signal: request.signal
      });
      if (!this.ownsSplit(request, generation, context, mergeID)) return false;
      const etag = response.response.headers.get('ETag');
      if (!response.data || response.data.id !== sourceID || !isMatchingPersonRevisionETag(etag, response.data)) {
        this.sourceError = 'The current source profile and its revision could not be loaded. Try again.';
        return false;
      }
      this.sourcePerson = response.data;
      this.sourceETag = etag;
      return true;
    } catch (cause) {
      if (this.ownsSplit(request, generation, context, mergeID)) this.sourceError = failureMessage(cause, 0);
      return false;
    } finally {
      if (generation === this.splitGeneration) this.sourceLoading = false;
    }
  }

  closeSplit(): void {
    if (this.splitBusy) return;
    this.splitAbort?.abort();
    ++this.splitGeneration;
    this.splitOpen = false;
    this.sourceLoading = false;
    this.clearConfirmation();
  }

  setParticipantSelected(participantID: number, selected: boolean): void {
    if (!this.eligibleParticipantIDs.includes(participantID) || this.splitBusy ||
      this.splitNeedsFreshState || this.committedResult) return;
    const next = new Set(this.selectedParticipantIDs);
    if (selected) next.add(participantID);
    else next.delete(participantID);
    this.selectedParticipantIDs = [...next].sort((left, right) => left - right);
    this.clearConfirmation();
    this.splitError = null;
  }

  confirmSplit(): boolean {
    if (!this.sourcePerson || !this.sourceETag || !this.detail || this.splitBusy ||
      this.splitNeedsFreshState || this.committedResult) return false;
    if (!this.isZeroParticipantLineage && this.selectedParticipantIDs.length === 0) {
      this.splitError = 'Select at least one eligible absorbed-lineage participant.';
      return false;
    }
    if (this.selectedParticipantIDs.some((id) => !this.eligibleParticipantIDs.includes(id))) return false;
    const snapshot = this.currentSplitSnapshot();
    if (!snapshot) return false;
    this.confirmedSnapshot = snapshot;
    this.confirmedParticipantIDs = [...snapshot.participantIDs];
    this.splitError = null;
    return true;
  }

  clearSplitConfirmation(): void {
    if (this.splitBusy || this.committedResult) return;
    this.clearConfirmation();
  }

  async retryStaleSplitState(): Promise<boolean> {
    const target = this.staleReloadTarget;
    if (!this.splitNeedsFreshState || !target || this.splitBusy || this.committedResult) return false;
    this.splitAbort?.abort();
    const request = new AbortController();
    this.splitAbort = request;
    const generation = ++this.splitGeneration;
    const context = this.contextGeneration;
    this.sourceLoading = true;
    this.splitError = null;
    try {
      return await this.loadFreshSplitState(target, generation, context, request);
    } finally {
      if (generation === this.splitGeneration) this.sourceLoading = false;
    }
  }

  async submitSplit(): Promise<void> {
    const snapshot = this.confirmedSnapshot;
    if (!snapshot || this.splitBusy || this.splitNeedsFreshState || this.committedResult ||
      !sameSnapshot(snapshot, this.currentSplitSnapshot())) return;
    const key = this.retryKey && sameSnapshot(this.retrySnapshot, snapshot) ? this.retryKey : crypto.randomUUID();
    this.retryKey = key;
    this.retrySnapshot = cloneSnapshot(snapshot);
    this.splitPending = true;
    this.splitError = null;
    this.splitAbort?.abort();
    const request = new AbortController();
    this.splitAbort = request;
    const generation = ++this.splitGeneration;
    const context = this.contextGeneration;
    try {
      const response = await this.client.POST('/api/v1/people/{id}/split', {
        params: {
          path: { id: snapshot.sourcePersonID },
          header: { 'If-Match': snapshot.sourceETag, 'Idempotency-Key': key }
        },
        body: { merge_id: snapshot.mergeID, participant_ids: snapshot.participantIDs },
        signal: request.signal
      });
      if (!this.ownsSplit(request, generation, context, snapshot.mergeID)) return;
      if (response.data) {
        this.retryKey = null;
        this.retrySnapshot = null;
        this.confirmedSnapshot = null;
        this.confirmedParticipantIDs = null;
        const sourceTag = response.response.headers.get('ETag');
        const createdTag = response.response.headers.get('X-New-Person-ETag');
        this.committedResult = {
          result: response.data,
          receiptETags: {
            source: isMatchingPersonRevisionETag(sourceTag, response.data.source_person) ? sourceTag : null,
            created: isMatchingPersonRevisionETag(createdTag, response.data.new_person) ? createdTag : null
          }
        };
        const committed = this.committedResult;
        await this.reconcileCommitted(response.data, committed, () =>
          this.ownsSplit(request, generation, context, snapshot.mergeID));
        return;
      }

      this.clearApplicationRetry();
      if (response.response.status === 409 && isExactErrorCode(response.error, 'person_merge_revision_conflict')) {
        const target = { sourcePersonID: snapshot.sourcePersonID, mergeID: snapshot.mergeID };
        this.splitNeedsFreshState = true;
        this.staleReloadTarget = target;
        await this.loadFreshSplitState(target, generation, context, request);
      } else {
        this.splitError = failureMessage(response.error, response.response.status);
      }
    } catch (cause) {
      if (!this.ownsSplit(request, generation, context, snapshot.mergeID)) return;
      // Fetch reports network transport failures as TypeError. Parsing,
      // protocol, and other thrown failures rotate the key and require a new
      // confirmation just like an HTTP application failure.
      if (cause instanceof TypeError) {
        this.retryKey = key;
        this.retrySnapshot = cloneSnapshot(snapshot);
      } else {
        this.clearApplicationRetry();
      }
      this.splitError = failureMessage(cause, 0);
    } finally {
      if (generation === this.splitGeneration) this.splitPending = false;
    }
  }

  destroy(): void {
    this.destroyed = true;
    ++this.contextGeneration;
    this.abortAll();
    this.snapshot = null;
    this.snapshotError = null;
    this.clearMergeContext();
  }

  private async loadFreshSplitState(
    target: StaleSplitReloadTarget,
    generation: number,
    context: number,
    owner: AbortController
  ): Promise<boolean> {
    try {
      const [source, merge] = await Promise.all([
        this.client.GET('/api/v1/people/{id}', {
          params: { path: { id: target.sourcePersonID } }, signal: owner.signal
        }),
        this.client.GET('/api/v1/person-merges/{merge_id}', {
          params: { path: { merge_id: target.mergeID } }, signal: owner.signal
        })
      ]);
      if (!this.ownsSplit(owner, generation, context, target.mergeID)) return false;
      const etag = source.response.headers.get('ETag');
      const mergeSourceID = merge.data?.merge.current_person_id;
      if (!source.data || source.data.id !== target.sourcePersonID ||
        !isMatchingPersonRevisionETag(etag, source.data) || !merge.data ||
        merge.data.merge.id !== target.mergeID || mergeSourceID !== target.sourcePersonID) {
        this.splitError = 'The split was stale, but the current source and merge detail could not both be reloaded. Try again.';
        return false;
      }
      this.sourcePerson = source.data;
      this.sourceETag = etag;
      this.detail = merge.data;
      this.selectedParticipantIDs = [];
      this.clearConfirmation();
      this.splitNeedsFreshState = false;
      this.staleReloadTarget = null;
      this.splitError = 'The source changed while you were reviewing it. Confirm the current lineage before splitting.';
      return true;
    } catch {
      if (this.ownsSplit(owner, generation, context, target.mergeID)) {
        this.splitError = 'The split was stale, but the current source and merge detail could not both be reloaded. Try again.';
      }
      return false;
    }
  }

  private async reconcileCommitted(
    result: SplitResult,
    committed: CommittedPersonSplit,
    ownsContext: () => boolean
  ): Promise<void> {
    if (this.committedReconciled) return;
    this.committedReconciled = true;
    try {
      await this.reconcile({ sourcePersonID: result.source_person.id, newPersonID: result.new_person.id });
    } catch {
      if (ownsContext() && this.committedResult === committed) {
        this.reconciliationError = 'The split completed, but the Directory refresh failed. Open either profile to load fresh data.';
      }
    }
  }

  private currentSplitSnapshot(): ConfirmedSplitSnapshot | null {
    if (!this.sourcePerson || !this.sourceETag || !this.detail) return null;
    return {
      sourcePersonID: this.sourcePerson.id,
      mergeID: this.detail.merge.id,
      participantIDs: [...this.selectedParticipantIDs].sort((left, right) => left - right),
      sourceETag: this.sourceETag,
      sourceRevision: this.sourcePerson.revision
    };
  }

  private clearApplicationRetry(): void {
    this.retryKey = null;
    this.retrySnapshot = null;
    this.confirmedSnapshot = null;
    this.confirmedParticipantIDs = null;
  }

  private clearConfirmation(): void {
    this.confirmedSnapshot = null;
    this.confirmedParticipantIDs = null;
    this.retryKey = null;
    this.retrySnapshot = null;
  }

  private resetSplit(): void {
    this.splitOpen = false;
    this.sourcePerson = null;
    this.sourceETag = null;
    this.sourceLoading = false;
    this.sourceError = null;
    this.selectedParticipantIDs = [];
    this.splitPending = false;
    this.splitNeedsFreshState = false;
    this.staleReloadTarget = null;
    this.splitError = null;
    this.committedResult = null;
    this.reconciliationError = null;
    this.committedReconciled = false;
    this.clearConfirmation();
  }

  private clearMergeContext(): void {
    this.selectedMergeID = null;
    this.detail = null;
    this.detailLoading = false;
    this.detailError = null;
    this.snapshot = null;
    this.snapshotLoading = false;
    this.snapshotError = null;
    this.resetSplit();
  }

  private abortAll(): void {
    this.historyAbort?.abort();
    this.detailAbort?.abort();
    this.snapshotAbort?.abort();
    this.splitAbort?.abort();
    ++this.historyGeneration;
    ++this.detailGeneration;
    ++this.snapshotGeneration;
    ++this.splitGeneration;
  }

  private ownsHistory(owner: AbortController, generation: number, context: number): boolean {
    return !this.destroyed && !owner.signal.aborted && this.historyAbort === owner &&
      generation === this.historyGeneration && context === this.contextGeneration;
  }

  private ownsDetail(owner: AbortController, generation: number, context: number, mergeID: number): boolean {
    return !this.destroyed && !owner.signal.aborted && this.detailAbort === owner &&
      generation === this.detailGeneration && context === this.contextGeneration && this.selectedMergeID === mergeID;
  }

  private ownsSnapshot(owner: AbortController, generation: number, context: number, mergeID: number): boolean {
    return !this.destroyed && !owner.signal.aborted && this.snapshotAbort === owner &&
      generation === this.snapshotGeneration && context === this.contextGeneration && this.selectedMergeID === mergeID;
  }

  private ownsSplit(owner: AbortController, generation: number, context: number, mergeID: number): boolean {
    return !this.destroyed && !owner.signal.aborted && this.splitAbort === owner &&
      generation === this.splitGeneration && context === this.contextGeneration && this.selectedMergeID === mergeID;
  }
}

function cloneSnapshot(snapshot: ConfirmedSplitSnapshot): ConfirmedSplitSnapshot {
  return { ...snapshot, participantIDs: [...snapshot.participantIDs] };
}

function sameSnapshot(
  left: ConfirmedSplitSnapshot | null,
  right: ConfirmedSplitSnapshot | null
): boolean {
  return !!left && !!right && left.sourcePersonID === right.sourcePersonID && left.mergeID === right.mergeID &&
    left.sourceETag === right.sourceETag && left.sourceRevision === right.sourceRevision &&
    left.participantIDs.length === right.participantIDs.length &&
    left.participantIDs.every((id, index) => id === right.participantIDs[index]);
}

function isExactErrorCode(error: unknown, code: string): boolean {
  return typeof error === 'object' && error !== null && 'error' in error && error.error === code;
}

function failureMessage(error: unknown, status: number): string {
  if (typeof error === 'object' && error !== null && 'message' in error && typeof error.message === 'string') return error.message;
  if (error instanceof Error && error.message) return error.message;
  return status ? `Request failed (${status})` : 'Request failed';
}
