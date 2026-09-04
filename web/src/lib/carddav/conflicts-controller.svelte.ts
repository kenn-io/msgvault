import type { APIClient } from '../api/client';
import type { components } from '../api/generated/schema';

type GeneratedAddressBook = components['schemas']['CardDAVAddressBookIdentityResponse'];
type GeneratedConflict = components['schemas']['CardDAVConflictResponse'];
type GeneratedDetail = components['schemas']['CardDAVConflictDetailResponse'];
type GeneratedSummary = components['schemas']['CardDAVContactSummaryResponse'];

export type CardDAVConflictChoice = GeneratedDetail['allowed_resolutions'][number];
export type CardDAVConflictAddressBook = Pick<GeneratedAddressBook, 'id' | 'name'>;
export type CardDAVConflictSummary = Pick<GeneratedSummary, 'state' | 'emails' | 'phones'> &
  Partial<Pick<GeneratedSummary, 'display_name' | 'truncated'>>;
export type CardDAVConflictListItem = Pick<GeneratedConflict,
  'id' | 'status' | 'local_state' | 'remote_state' | 'allowed_resolutions' | 'updated_at'> & {
    address_book: CardDAVConflictAddressBook;
  };
export type CardDAVConflictDetail = Pick<GeneratedDetail,
  'id' | 'status' | 'allowed_resolutions' | 'created_at' | 'updated_at'> &
  Partial<Pick<GeneratedDetail, 'resolution' | 'resolved_at'>> & {
    address_book: CardDAVConflictAddressBook;
    base: CardDAVConflictSummary;
    local: CardDAVConflictSummary;
    remote: CardDAVConflictSummary;
  };

export interface CardDAVConflictFocusRequest {
  key: number;
  conflictID?: number;
  detail?: boolean;
}

export interface CardDAVRequestedConflict {
  conflictID: number;
  key: number;
}

export type CardDAVConflictResolutionOutcome =
  | { kind: 'resolved' }
  | { kind: 'reconciled' }
  | { kind: 'unknown' }
  | { kind: 'error' }
  | { kind: 'ignored' };

type Snapshot<T> =
  | { ok: true; value: T }
  | { ok: false; unavailable: boolean };

const STALE_RESOLUTION_CODES = new Set(['carddav_conflict_stale', 'carddav_conflict_pending']);

export class CardDAVConflictsController {
  conflicts = $state<CardDAVConflictListItem[]>([]);
  selectedID = $state<number>();
  selectedDetail = $state<CardDAVConflictDetail>();

  listLoading = $state(true);
  detailLoading = $state(false);
  pendingResolutionID = $state<number>();

  listError = $state<string | null>(null);
  detailError = $state<string | null>(null);
  unavailable = $state(false);
  resolutionError = $state<string | null>(null);
  resolutionUnknown = $state(false);
  announcement = $state<string | null>(null);
  focusRequest = $state<CardDAVConflictFocusRequest>();

  private readonly client: APIClient;
  private disposed = false;
  private generation = 1;
  private listRequestGeneration = 0;
  private detailRequestGeneration = 0;
  private mutationGeneration = 0;
  private reconciliationGeneration = 0;
  private focusGeneration = 0;
  private reconciliationPending = false;
  private consumedRequestKey?: number;
  private listAbort?: AbortController;
  private detailAbort?: AbortController;
  private mutationAbort?: AbortController;

  constructor(client: APIClient) {
    this.client = client;
  }

  async load(): Promise<void> {
    await this.readList();
  }

  async retryList(): Promise<void> {
    await this.readList();
  }

  async select(id: number): Promise<void> {
    if (this.disposed || this.unavailable || this.reconciliationPending || id <= 0) return;
    if (this.selectedID !== id) {
      this.selectedID = id;
      this.selectedDetail = undefined;
      this.detailError = null;
      this.resolutionError = null;
      this.resolutionUnknown = false;
    }
    this.announcement = null;
    await this.readDetail(id);
  }

  async retrySelectedState(): Promise<void> {
    if (this.selectedID === undefined || this.disposed || this.unavailable || this.reconciliationPending) return;
    if (this.resolutionUnknown) {
      await this.reconcile(this.selectedID);
      return;
    }
    await this.readDetail(this.selectedID);
  }

  async openRequestedConflict(request: CardDAVRequestedConflict): Promise<boolean> {
    if (this.disposed || this.reconciliationPending || request.key === this.consumedRequestKey || request.conflictID <= 0) return false;
    this.consumedRequestKey = request.key;
    await this.select(request.conflictID);
    if (!this.disposed && !this.unavailable) {
      this.focusRequest = { key: ++this.focusGeneration, conflictID: request.conflictID, detail: true };
    }
    return true;
  }

  isResolutionAllowed(choice: CardDAVConflictChoice): boolean {
    const detail = this.selectedDetail;
    if (!detail) return false;
    return (
      !this.disposed &&
      !this.unavailable &&
      !this.resolutionUnknown &&
      this.pendingResolutionID === undefined &&
      detail.id === this.selectedID &&
      detail.status === 'unresolved' &&
      detail.allowed_resolutions.includes(choice)
    );
  }

  async resolve(id: number, choice: CardDAVConflictChoice): Promise<CardDAVConflictResolutionOutcome> {
    if (id !== this.selectedID || !this.isResolutionAllowed(choice)) return { kind: 'ignored' };
    const context = this.generation;
    const mutation = ++this.mutationGeneration;
    const controller = new AbortController();
    this.mutationAbort = controller;
    this.pendingResolutionID = id;
    this.resolutionError = null;
    this.announcement = null;
    try {
      const result = await this.client.POST('/api/v1/carddav/conflicts/{id}/resolve', {
        params: { path: { id } },
        body: { choice },
        signal: controller.signal
      });
      if (!this.currentMutation(context, mutation, controller.signal)) return { kind: 'ignored' };
      if (result.data &&
        result.data.id === id &&
        result.data.status === 'resolved' &&
        result.data.resolution === choice) {
        this.applyResolution(id, choice);
        return { kind: 'resolved' };
      }
      if (result.data || isAmbiguousResolution(result.response.status, result.error?.error)) {
        return await this.reconcileResolution(id, context, mutation);
      }
      this.resolutionError = 'Unable to resolve this CardDAV conflict.';
      return { kind: 'error' };
    } catch {
      if (!this.currentMutation(context, mutation, controller.signal)) return { kind: 'ignored' };
      return await this.reconcileResolution(id, context, mutation);
    } finally {
      if (this.currentMutation(context, mutation)) {
        if (this.mutationAbort === controller) this.mutationAbort = undefined;
        this.pendingResolutionID = undefined;
      }
    }
  }

  destroy(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.generation += 1;
    this.listRequestGeneration += 1;
    this.detailRequestGeneration += 1;
    this.mutationGeneration += 1;
    this.listAbort?.abort();
    this.detailAbort?.abort();
    this.mutationAbort?.abort();
    this.listAbort = undefined;
    this.detailAbort = undefined;
    this.mutationAbort = undefined;
  }

  private async readList(): Promise<boolean> {
    if (this.disposed) return false;
    const context = this.generation;
    const request = ++this.listRequestGeneration;
    this.listAbort?.abort();
    const controller = new AbortController();
    this.listAbort = controller;
    this.listLoading = true;
    this.listError = null;
    const snapshot = await this.fetchList(controller.signal);
    if (!this.currentList(context, request, controller.signal)) return false;
    if (snapshot.ok) {
      this.unavailable = false;
      this.conflicts = snapshot.value;
    } else if (snapshot.unavailable) {
      this.applyUnavailable();
    } else {
      this.listError = 'Unable to load CardDAV conflicts.';
    }
    if (this.listAbort === controller) this.listAbort = undefined;
    this.listLoading = false;
    return snapshot.ok;
  }

  private async readDetail(id: number): Promise<boolean> {
    if (this.disposed) return false;
    const context = this.generation;
    const request = ++this.detailRequestGeneration;
    this.detailAbort?.abort();
    const controller = new AbortController();
    this.detailAbort = controller;
    this.detailLoading = true;
    this.detailError = null;
    const snapshot = await this.fetchDetail(id, controller.signal);
    if (!this.currentDetail(context, request, controller.signal) || this.selectedID !== id) return false;
    if (snapshot.ok && snapshot.value.id === id) {
      this.selectedDetail = snapshot.value;
    } else if (!snapshot.ok && snapshot.unavailable) {
      this.applyUnavailable();
    } else {
      this.selectedDetail = undefined;
      this.detailError = 'Unable to load CardDAV conflict details.';
    }
    if (this.detailAbort === controller) this.detailAbort = undefined;
    this.detailLoading = false;
    return snapshot.ok && snapshot.value.id === id;
  }

  private async reconcileResolution(
    id: number,
    context: number,
    mutation: number
  ): Promise<CardDAVConflictResolutionOutcome> {
    const reconciled = await this.reconcile(id);
    if (this.unavailable && !this.disposed) return { kind: 'unknown' };
    if (!this.currentMutation(context, mutation)) return { kind: 'ignored' };
    if (reconciled) {
      if (this.selectedDetail?.id === id && this.selectedDetail.status === 'resolved') {
        this.resolutionError = null;
        this.announcement = `CardDAV conflict ${id} state was refreshed and is already resolved.`;
        return { kind: 'reconciled' };
      }
      this.resolutionError = 'Current conflict state was refreshed after the resolution result was uncertain. Choose again to resolve it.';
      return { kind: 'reconciled' };
    }
    this.resolutionUnknown = true;
    this.resolutionError = 'Current CardDAV conflict state is unknown. Retry state before resolving it.';
    return { kind: 'unknown' };
  }

  private async reconcile(id: number): Promise<boolean> {
    if (this.disposed || this.reconciliationPending) return false;
    const context = this.generation;
    const reconciliation = ++this.reconciliationGeneration;
    const listRequest = ++this.listRequestGeneration;
    const detailRequest = ++this.detailRequestGeneration;
    this.listAbort?.abort();
    this.detailAbort?.abort();
    const listController = new AbortController();
    const detailController = new AbortController();
    this.listAbort = listController;
    this.detailAbort = detailController;
    this.reconciliationPending = true;
    this.listLoading = true;
    this.detailLoading = true;
    this.listError = null;
    this.detailError = null;
    try {
      const [list, detail] = await Promise.all([
        this.fetchList(listController.signal),
        this.fetchDetail(id, detailController.signal)
      ]);
      if (!this.currentList(context, listRequest, listController.signal) ||
        !this.currentDetail(context, detailRequest, detailController.signal)) return false;

      const validDetail = detail.ok && detail.value.id === id;
      const unavailable = (!list.ok && list.unavailable) || (!detail.ok && detail.unavailable);
      if (unavailable) {
        this.applyUnavailable();
      } else if (list.ok && validDetail) {
        this.unavailable = false;
        this.conflicts = list.value;
        if (this.selectedID === id) this.selectedDetail = detail.value;
        this.resolutionUnknown = false;
        this.resolutionError = null;
      } else {
        if (!list.ok) this.listError = 'Unable to load CardDAV conflicts.';
        if (!validDetail) this.detailError = 'Unable to load CardDAV conflict details.';
        this.resolutionUnknown = true;
      }
      return !unavailable && list.ok && validDetail;
    } finally {
      if (this.currentList(context, listRequest)) {
        if (this.listAbort === listController) this.listAbort = undefined;
        this.listLoading = false;
      }
      if (this.currentDetail(context, detailRequest)) {
        if (this.detailAbort === detailController) this.detailAbort = undefined;
        this.detailLoading = false;
      }
      if (this.current(context) && this.reconciliationGeneration === reconciliation) {
        this.reconciliationPending = false;
      }
    }
  }

  private async fetchList(signal: AbortSignal): Promise<Snapshot<CardDAVConflictListItem[]>> {
    try {
      const { data, error } = await this.client.GET('/api/v1/carddav/conflicts', { signal });
      if (!data) return { ok: false, unavailable: error?.error === 'carddav_unavailable' };
      return { ok: true, value: data.conflicts.map(safeListItem) };
    } catch {
      return { ok: false, unavailable: false };
    }
  }

  private async fetchDetail(id: number, signal: AbortSignal): Promise<Snapshot<CardDAVConflictDetail>> {
    try {
      const { data, error } = await this.client.GET('/api/v1/carddav/conflicts/{id}', {
        params: { path: { id } },
        signal
      });
      if (!data) return { ok: false, unavailable: error?.error === 'carddav_unavailable' };
      return { ok: true, value: safeDetail(data) };
    } catch {
      return { ok: false, unavailable: false };
    }
  }

  private applyUnavailable(): void {
    // Unavailable replaces the whole conflict context, so every in-flight lane
    // must lose ownership before any ignored-abort response can settle later.
    this.generation += 1;
    this.listRequestGeneration += 1;
    this.detailRequestGeneration += 1;
    this.mutationGeneration += 1;
    this.reconciliationGeneration += 1;
    this.listAbort?.abort();
    this.detailAbort?.abort();
    this.mutationAbort?.abort();
    this.listAbort = undefined;
    this.detailAbort = undefined;
    this.mutationAbort = undefined;
    this.unavailable = true;
    this.conflicts = [];
    this.selectedID = undefined;
    this.selectedDetail = undefined;
    this.listLoading = false;
    this.detailLoading = false;
    this.pendingResolutionID = undefined;
    this.listError = null;
    this.detailError = null;
    this.resolutionError = null;
    this.resolutionUnknown = false;
    this.reconciliationPending = false;
    this.announcement = null;
    this.focusRequest = undefined;
  }

  private applyResolution(id: number, choice: CardDAVConflictChoice): void {
    const index = this.conflicts.findIndex((conflict) => conflict.id === id);
    const remaining = this.conflicts.filter((conflict) => conflict.id !== id);
    this.conflicts = remaining;
    if (this.selectedDetail?.id === id) {
      this.selectedDetail = {
        ...this.selectedDetail,
        status: 'resolved',
        resolution: choice,
        allowed_resolutions: []
      };
    }
    this.resolutionUnknown = false;
    this.resolutionError = null;
    const side = choice === 'keep_local' ? 'local' : 'remote';
    this.announcement = `CardDAV conflict ${id} resolved by keeping the ${side} card.`;
    const fallbackIndex = index < 0 ? 0 : Math.min(index, remaining.length - 1);
    const next = fallbackIndex >= 0 ? remaining[fallbackIndex] : undefined;
    this.focusRequest = { key: ++this.focusGeneration, ...(next ? { conflictID: next.id } : {}) };
  }

  private current(generation: number, signal?: AbortSignal): boolean {
    return !this.disposed && this.generation === generation && !signal?.aborted;
  }

  private currentList(generation: number, request: number, signal?: AbortSignal): boolean {
    return this.current(generation, signal) && this.listRequestGeneration === request;
  }

  private currentDetail(generation: number, request: number, signal?: AbortSignal): boolean {
    return this.current(generation, signal) && this.detailRequestGeneration === request;
  }

  private currentMutation(generation: number, mutation: number, signal?: AbortSignal): boolean {
    return this.current(generation, signal) && this.mutationGeneration === mutation;
  }
}

function safeAddressBook(addressBook: GeneratedAddressBook): CardDAVConflictAddressBook {
  return { id: addressBook.id, name: addressBook.name };
}

function safeChoices(choices: GeneratedDetail['allowed_resolutions']): CardDAVConflictChoice[] {
  return choices.filter((choice): choice is CardDAVConflictChoice =>
    choice === 'keep_local' || choice === 'keep_remote');
}

function safeListItem(conflict: GeneratedConflict): CardDAVConflictListItem {
  return {
    id: conflict.id,
    address_book: safeAddressBook(conflict.address_book),
    status: conflict.status,
    local_state: conflict.local_state,
    remote_state: conflict.remote_state,
    allowed_resolutions: safeChoices(conflict.allowed_resolutions),
    updated_at: conflict.updated_at
  };
}

function safeSummary(summary: GeneratedSummary): CardDAVConflictSummary {
  return {
    state: summary.state,
    emails: [...summary.emails],
    phones: [...summary.phones],
    ...(summary.display_name !== undefined ? { display_name: summary.display_name } : {}),
    ...(summary.truncated !== undefined ? { truncated: summary.truncated } : {})
  };
}

function safeDetail(detail: GeneratedDetail): CardDAVConflictDetail {
  return {
    id: detail.id,
    address_book: safeAddressBook(detail.address_book),
    status: detail.status,
    base: safeSummary(detail.base),
    local: safeSummary(detail.local),
    remote: safeSummary(detail.remote),
    allowed_resolutions: safeChoices(detail.allowed_resolutions),
    created_at: detail.created_at,
    updated_at: detail.updated_at,
    ...(detail.resolution !== undefined ? { resolution: detail.resolution } : {}),
    ...(detail.resolved_at !== undefined ? { resolved_at: detail.resolved_at } : {})
  };
}

function isAmbiguousResolution(status: number, errorCode: string | undefined): boolean {
  return status >= 500 || (status === 409 && errorCode !== undefined && STALE_RESOLUTION_CODES.has(errorCode));
}
