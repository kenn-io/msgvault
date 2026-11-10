import {
  getPersonMerge as generatedGetPersonMerge,
  getPersonMergeSnapshot as generatedGetPersonMergeSnapshot,
  getPersonProfile as generatedGetPersonProfile,
  listPersonMerges as generatedListPersonMerges,
  splitPersonMerge as generatedSplitPersonMerge,
} from '../api/generated/api/api';
import { mergedInAddresses, mergeSummaryRow, workspaceLabels, type AddressIdentity, type MergedInAddress, type MergeLabels } from './merge-labels';
import { createBrowserRequestID } from '../api/request-id';
import type { APIClient } from '../api/client';
import type {
  Person as GeneratedPerson,
  PersonMergeDetail as GeneratedPersonMergeDetail,
  PersonMergeSnapshotResponse as GeneratedPersonMergeSnapshotResponse,
  PersonMergeSummary as GeneratedPersonMergeSummary,
  PersonSplitResult as GeneratedPersonSplitResult,
} from '../api/generated/models';
import { isMatchingPersonRevisionETag } from './person-merge';
export const PERSON_MERGE_HISTORY_LIMIT = 100;
type Person = GeneratedPerson;
type MergeSummary = GeneratedPersonMergeSummary;
type MergeDetail = GeneratedPersonMergeDetail;
type MergeSnapshot = GeneratedPersonMergeSnapshotResponse;
type SplitResult = GeneratedPersonSplitResult;
export interface PersonSplitCommittedContext {
  sourcePersonID: number;
  newPersonID: number;
}
export interface CommittedPersonSplit {
  result: SplitResult;
  receiptETags: {
    source: string | null;
    created: string | null;
  };
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
  private historyHasNext = $state(false);
  historyLoading = $state(false);
  historyError = $state<string | null>(null);
  pendingHistoryOffset = $state<number | null>(null);
  historyRows = $state(new Map<number, Awaited<ReturnType<typeof mergeSummaryRow>>>());
  scopes = $state(new Map<string, string>());
  selectedMergeID = $state<number | null>(null);
  detail = $state<MergeDetail | null>(null);
  labels = $state<({ for: MergeDetail } & MergeLabels) | null>(null);
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
  addScopes(identities: Omit<AddressIdentity, 'identity_key'>[]): void {
    const next = workspaceLabels(identities, this.scopes);
    if (next.size === this.scopes.size) return;
    this.scopes = next;
    this.historyRows = new Map(this.history.map(item => [item.merge.id, mergeSummaryRow(item, next)]));
    if (this.detail && this.labels?.for === this.detail) {
      this.labels = { ...mergeSummaryRow(this.detail, next), for: this.detail, names: this.labels.names };
    }
  }
  get hasPreviousHistoryPage(): boolean {
    return this.historyOffset > 0;
  }
  get hasNextHistoryPage(): boolean {
    return this.historyHasNext;
  }
  /** What each merge on this page brought into the profile and it still holds: the newest merge per address. */
  get mergedIn(): MergedInAddress[] {
    const seen = new Set<number>();
    return mergedInAddresses(this.history.filter(item => item.merge.current_person_id === this.personID).flatMap(({ merge, labels }) =>
      (labels.participants ?? []).filter(participant => participant.origin_side === 'absorbed' && participant.split_id === undefined && !seen.has(participant.participant_id)).map(participant => {
        seen.add(participant.participant_id);
        return {
          merge_id: merge.id, participant_id: participant.participant_id, merged_at: merge.created_at,
          addresses: labels.addresses.filter(address => address.participant_id === participant.participant_id),
        };
      })), this.scopes);
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
    return !!this.detail?.merge.current_person_id && !this.committedResult &&
      (this.isZeroParticipantLineage || this.eligibleParticipantIDs.length > 0);
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
    this.historyRows = new Map();
    this.scopes = new Map();
    this.historyOffset = 0;
    this.historyHasNext = false;
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
      const response = await generatedListPersonMerges(
        { id: this.personID },
        { limit: PERSON_MERGE_HISTORY_LIMIT + 1, offset },
        {
          ...this.client,
          signal: request.signal,
        },
      );
      if (!this.ownsHistory(request, generation, context)) return;
      if (response.data) {
        const merges = response.data.merges ?? [];
        this.historyHasNext = merges.length > PERSON_MERGE_HISTORY_LIMIT;
        this.history = merges.slice(0, PERSON_MERGE_HISTORY_LIMIT);
        this.historyOffset = response.data.offset;
        this.pendingHistoryOffset = null;
        this.addScopes(this.history.flatMap(item => [...item.labels.addresses, ...item.labels.profiles.flatMap(profile => profile.contact_points ?? [])]));
        this.historyRows = new Map(this.history.map(item => [item.merge.id, mergeSummaryRow(item, this.scopes)]));
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
      const response = await generatedGetPersonMerge(
        { mergeId: mergeID },
        {
          ...this.client,
          signal: request.signal,
        },
      );
      if (!this.ownsDetail(request, generation, context, mergeID)) return;
      if (response.data) {
        this.detail = response.data;
        this.detailLoading = false;
        void this.refreshLabels(this.detail);
      } else {
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
    if (this.splitBusy) return;
    const mergeID = this.selectedMergeID, splitWasOpen = this.splitOpen, selected = this.selectedParticipantIDs;
    if (mergeID !== null) await this.selectMerge(mergeID);
    if (splitWasOpen && this.selectedMergeID === mergeID && this.detail && await this.openSplit()) {
      this.selectedParticipantIDs = selected.filter(id => this.eligibleParticipantIDs.includes(id));
    }
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
      const response = await generatedGetPersonMergeSnapshot(
        { mergeId: mergeID },
        {
          ...this.client,
          signal: request.signal,
        },
      );
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
      const state = await this.readSplitState({ sourcePersonID: sourceID, mergeID }, request);
      if (!this.ownsSplit(request, generation, context, mergeID)) return false;
      if (!state) {
        this.sourceError = "Couldn't load this profile.";
        return false;
      }
      this.applySplitState(state);
      this.selectedParticipantIDs = this.selectedParticipantIDs.filter(id => this.eligibleParticipantIDs.includes(id));
      return true;
    } catch (cause) {
      if (this.ownsSplit(request, generation, context, mergeID)) this.sourceError = failureMessage(cause, 0);
      return false;
    } finally {
      if (generation === this.splitGeneration) this.sourceLoading = false;
    }
  }
  closeSplit(): void {
    if (this.splitPending) return;
    this.splitAbort?.abort();
    ++this.splitGeneration;
    this.splitOpen = false;
    this.sourceLoading = false;
    this.clearConfirmation();
  }
  setParticipantSelected(participantID: number, selected: boolean): void {
    if (
      !this.eligibleParticipantIDs.includes(participantID) ||
      this.splitBusy ||
      this.splitNeedsFreshState ||
      this.committedResult
    )
      return;
    const next = new Set(this.selectedParticipantIDs);
    if (selected) next.add(participantID);
    else next.delete(participantID);
    this.selectedParticipantIDs = [...next].sort((left, right) => left - right);
    this.clearConfirmation();
    this.splitError = null;
  }
  confirmSplit(): boolean {
    if (
      !this.sourcePerson ||
      !this.sourceETag ||
      !this.detail ||
      this.splitBusy ||
      this.splitNeedsFreshState ||
      this.committedResult
    )
      return false;
    if (!this.isZeroParticipantLineage && this.selectedParticipantIDs.length === 0) {
      this.splitError = 'Choose at least one address.';
      return false;
    }
    if (this.selectedParticipantIDs.some((id) => !this.eligibleParticipantIDs.includes(id))) return false;
    const snapshot = this.currentSplitSnapshot();
    if (!snapshot) return false;
    this.confirmedSnapshot = snapshot;
    this.splitError = null;
    return true;
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
    if (
      !snapshot ||
      this.splitBusy ||
      this.splitNeedsFreshState ||
      this.committedResult ||
      !sameSnapshot(snapshot, this.currentSplitSnapshot())
    )
      return;
    const key = this.retryKey && sameSnapshot(this.retrySnapshot, snapshot) ? this.retryKey : createBrowserRequestID();
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
      const response = await generatedSplitPersonMerge(
        { id: snapshot.sourcePersonID },
        { merge_id: snapshot.mergeID, participant_ids: snapshot.participantIDs },
        {
          ...this.client,
          signal: request.signal,
          headers: { 'If-Match': snapshot.sourceETag, 'Idempotency-Key': key },
        },
      );
      if (!this.ownsSplit(request, generation, context, snapshot.mergeID)) return;
      if (response.data) {
        this.retryKey = null;
        this.retrySnapshot = null;
        this.confirmedSnapshot = null;
        const sourceTag = response.response.headers.get('ETag');
        const createdTag = response.response.headers.get('X-New-Person-ETag');
        this.committedResult = {
          result: response.data,
          receiptETags: {
            source: isMatchingPersonRevisionETag(sourceTag, response.data.source_person) ? sourceTag : null,
            created: isMatchingPersonRevisionETag(createdTag, response.data.new_person) ? createdTag : null,
          },
        };
        const committed = this.committedResult;
        const reconciled = this.reconcileCommitted(response.data, committed, () =>
          this.ownsSplit(request, generation, context, snapshot.mergeID),
        );
        this.splitPending = false;
        await reconciled;
        return;
      }
      this.clearConfirmation();
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
        this.clearConfirmation();
      }
      this.splitError = failureMessage(cause, 0);
    } finally {
      if (generation === this.splitGeneration) this.splitPending = false;
    }
  }
  async retryReconciliation(): Promise<void> {
    const committed = this.committedResult;
    if (!committed || this.splitBusy || this.destroyed) return;
    this.reconciliationError = null;
    this.committedReconciled = false;
    await this.reconcileCommitted(committed.result, committed, () => !this.destroyed && this.committedResult === committed);
  }
  destroy(): void {
    this.destroyed = true;
    ++this.contextGeneration;
    this.abortAll();
    this.snapshot = null;
    this.snapshotError = null;
    this.clearMergeContext();
    this.historyRows = new Map();
  }
  private async loadFreshSplitState(
    target: StaleSplitReloadTarget,
    generation: number,
    context: number,
    owner: AbortController,
  ): Promise<boolean> {
    try {
      const state = await this.readSplitState(target, owner);
      if (!this.ownsSplit(owner, generation, context, target.mergeID)) return false;
      if (!state) {
        this.splitError = "This profile changed, and couldn't be reloaded with its merge.";
        return false;
      }
      this.applySplitState(state);
      this.selectedParticipantIDs = [];
      this.splitNeedsFreshState = false;
      this.staleReloadTarget = null;
      this.splitError = 'This profile changed since you opened it. Check its addresses, then split again.';
      return true;
    } catch {
      if (this.ownsSplit(owner, generation, context, target.mergeID)) {
        this.splitError =
          "This profile changed, and couldn't be reloaded with its merge.";
      }
      return false;
    }
  }
  private async readSplitState(target: StaleSplitReloadTarget, owner: AbortController): Promise<{ person: Person; etag: string; detail: MergeDetail } | null> {
    const options = { ...this.client, signal: owner.signal };
    // Labels read after the revision can only be newer; intervening edits then fail If-Match.
    const source = await generatedGetPersonProfile({ id: target.sourcePersonID }, options);
    if (owner.signal.aborted) return null;
    const merge = await generatedGetPersonMerge({ mergeId: target.mergeID }, options);
    const etag = source.response.headers.get('ETag');
    if (!source.data || source.data.id !== target.sourcePersonID || !etag || !isMatchingPersonRevisionETag(etag, source.data) ||
      !merge.data || merge.data.merge.id !== target.mergeID || merge.data.merge.current_person_id !== target.sourcePersonID) return null;
    return { person: source.data, etag, detail: merge.data };
  }
  private applySplitState(state: { person: Person; etag: string; detail: MergeDetail }): void {
    this.sourcePerson = state.person;
    this.sourceETag = state.etag;
    this.detail = state.detail;
    void this.refreshLabels(this.detail);
    this.clearConfirmation();
  }
  private async refreshLabels(detail: MergeDetail): Promise<void> {
    const signal = this.detailAbort?.signal;
    if (!signal) return;
    const owns = () => !this.destroyed && !signal.aborted && this.detail === detail;
    if (!owns()) return;
    // What the merge recorded shows at once; each restored profile's name fills in as it loads.
    this.addScopes([...(detail.labels?.addresses ?? []), ...(detail.labels?.profiles ?? []).flatMap(profile => profile.contact_points ?? [])]);
    const row = mergeSummaryRow(detail, this.scopes);
    this.historyRows = new Map(this.historyRows).set(detail.merge.id, row);
    this.labels = { for: detail, ...row, names: new Map(row.names) };
    await Promise.all([...new Set((detail.splits ?? []).map(split => split.new_person_id))].map(async id => {
      let name: string | undefined, incomplete = false;
      try {
        const response = await generatedGetPersonProfile({ id }, { ...this.client, signal });
        if (response.data) name = response.data.display_name?.trim() ?? '';
        else incomplete = response.response.status !== 404;
      } catch { incomplete = true; }
      const labels = this.labels;
      if (!owns() || labels?.for !== detail) return;
      this.labels = { ...labels, names: name === undefined ? labels.names : new Map(labels.names).set(id, name), incomplete: labels.incomplete || incomplete };
    }));
  }
  private async reconcileCommitted(
    result: SplitResult,
    committed: CommittedPersonSplit,
    ownsContext: () => boolean,
  ): Promise<void> {
    if (this.committedReconciled) return;
    this.committedReconciled = true;
    try {
      await this.reconcile({ sourcePersonID: result.source_person.id, newPersonID: result.new_person.id });
    } catch {
      if (ownsContext() && this.committedResult === committed) {
        this.reconciliationError =
          'The split is done. Open either profile to load it fresh.';
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
      sourceRevision: this.sourcePerson.revision,
    };
  }
  private clearConfirmation(): void {
    this.confirmedSnapshot = null;
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
    this.labels = null;
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
    return (
      !this.destroyed &&
      !owner.signal.aborted &&
      this.historyAbort === owner &&
      generation === this.historyGeneration &&
      context === this.contextGeneration
    );
  }
  private ownsDetail(owner: AbortController, generation: number, context: number, mergeID: number): boolean {
    return (
      !this.destroyed &&
      !owner.signal.aborted &&
      this.detailAbort === owner &&
      generation === this.detailGeneration &&
      context === this.contextGeneration &&
      this.selectedMergeID === mergeID
    );
  }
  private ownsSnapshot(owner: AbortController, generation: number, context: number, mergeID: number): boolean {
    return (
      !this.destroyed &&
      !owner.signal.aborted &&
      this.snapshotAbort === owner &&
      generation === this.snapshotGeneration &&
      context === this.contextGeneration &&
      this.selectedMergeID === mergeID
    );
  }
  private ownsSplit(owner: AbortController, generation: number, context: number, mergeID: number): boolean {
    return (
      !this.destroyed &&
      !owner.signal.aborted &&
      this.splitAbort === owner &&
      generation === this.splitGeneration &&
      context === this.contextGeneration &&
      this.selectedMergeID === mergeID
    );
  }
}
function cloneSnapshot(snapshot: ConfirmedSplitSnapshot): ConfirmedSplitSnapshot {
  return { ...snapshot, participantIDs: [...snapshot.participantIDs] };
}
function sameSnapshot(left: ConfirmedSplitSnapshot | null, right: ConfirmedSplitSnapshot | null): boolean {
  return (
    !!left &&
    !!right &&
    left.sourcePersonID === right.sourcePersonID &&
    left.mergeID === right.mergeID &&
    left.sourceETag === right.sourceETag &&
    left.sourceRevision === right.sourceRevision &&
    left.participantIDs.length === right.participantIDs.length &&
    left.participantIDs.every((id, index) => id === right.participantIDs[index])
  );
}
function isExactErrorCode(error: unknown, code: string): boolean {
  return typeof error === 'object' && error !== null && 'error' in error && error.error === code;
}
function failureMessage(error: unknown, status: number): string {
  if (typeof error === 'object' && error !== null && 'message' in error && typeof error.message === 'string')
    return error.message;
  if (error instanceof Error && error.message) return error.message;
  return status ? `The server answered ${status}.` : "Couldn't reach msgvault.";
}
