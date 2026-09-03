import type { APIClient } from '../api/client';
import type { components } from '../api/generated/schema';

type GeneratedTracking = components['schemas']['PersonTracking'];
type GeneratedTarget = components['schemas']['TargetDescriptor'];

export type PersonTrackingState = Pick<GeneratedTracking, 'person_id' | 'tracked' | 'tracked_at'>;
export type PersonTrackingTarget = Pick<
  GeneratedTarget,
  'kind' | 'description' | 'value_type' | 'cardinality' | 'sensitive'
>;
export type PersonTrackingOutcome =
  | { kind: 'confirmed'; desired: boolean }
  | { kind: 'reconciled'; desired: boolean }
  | { kind: 'unknown'; desired: boolean }
  | { kind: 'error'; desired: boolean }
  | { kind: 'ignored' };

export class PersonTrackingController {
  personID = $state<number>();
  tracking = $state<PersonTrackingState>();
  targets = $state<PersonTrackingTarget[]>([]);
  trackingLoading = $state(false);
  catalogLoading = $state(false);
  pending = $state(false);
  trackingError = $state<string | null>(null);
  catalogError = $state<string | null>(null);
  stateUnknown = $state(false);
  catalogIncludesSensitive = $state(false);
  announcement = $state<string | null>(null);

  private readonly client: APIClient;
  private disposed = false;
  private contextGeneration = 0;
  private trackingGeneration = 0;
  private catalogGeneration = 0;
  private mutationGeneration = 0;
  private trackingAbort?: AbortController;
  private catalogAbort?: AbortController;
  private mutationAbort?: AbortController;

  constructor(client: APIClient) {
    this.client = client;
  }

  get contextToken(): number {
    return this.contextGeneration;
  }

  isContextCurrent(contextToken: number): boolean {
    return !this.disposed && this.contextGeneration === contextToken;
  }

  async setPerson(personID: number): Promise<void> {
    if (this.disposed || !Number.isSafeInteger(personID) || personID <= 0) return;
    this.contextGeneration += 1;
    this.trackingGeneration += 1;
    this.catalogGeneration += 1;
    this.mutationGeneration += 1;
    this.trackingAbort?.abort();
    this.catalogAbort?.abort();
    this.mutationAbort?.abort();
    this.personID = personID;
    this.tracking = undefined;
    this.targets = [];
    this.trackingError = null;
    this.catalogError = null;
    this.pending = false;
    this.stateUnknown = false;
    this.catalogIncludesSensitive = false;
    this.announcement = null;
    await Promise.all([this.loadTracking(personID), this.loadCatalog(false)]);
  }

  async setTracked(desired: boolean): Promise<PersonTrackingOutcome> {
    const personID = this.personID;
    if (personID === undefined || this.disposed || this.pending || this.trackingLoading ||
      this.stateUnknown || !this.tracking || this.tracking.person_id !== personID ||
      this.tracking.tracked === desired) return { kind: 'ignored' };
    const context = this.contextGeneration;
    const mutation = ++this.mutationGeneration;
    this.mutationAbort?.abort();
    const abort = new AbortController();
    this.mutationAbort = abort;
    this.pending = true;
    this.trackingError = null;
    this.announcement = null;
    try {
      const result = await this.client.PUT('/api/v1/people/{id}/tracking', {
        params: { path: { id: personID } },
        body: { tracked: desired },
        signal: abort.signal
      });
      if (!this.currentMutation(context, mutation, personID, abort.signal)) return { kind: 'ignored' };
      const confirmed = result.data ? safeTracking(result.data) : undefined;
      if (confirmed?.person_id === personID && confirmed.tracked === desired) {
        this.applyConfirmed(confirmed);
        return { kind: 'confirmed', desired };
      }
      if (result.response.status === 400 || result.response.status === 404) {
        this.trackingError = 'Unable to update profile maintenance tracking.';
        return { kind: 'error', desired };
      }
      return await this.reconcileMutation(personID, desired, context, mutation);
    } catch {
      if (!this.currentMutation(context, mutation, personID, abort.signal)) return { kind: 'ignored' };
      return await this.reconcileMutation(personID, desired, context, mutation);
    } finally {
      if (this.currentMutation(context, mutation, personID)) {
        if (this.mutationAbort === abort) this.mutationAbort = undefined;
        this.pending = false;
      }
    }
  }

  async retryTracking(): Promise<void> {
    if (this.personID === undefined || this.disposed || this.trackingLoading || this.pending) return;
    await this.loadTracking(this.personID);
  }

  async retryCatalog(includeSensitive: boolean): Promise<void> {
    if (this.personID === undefined || this.disposed || this.catalogLoading) return;
    await this.loadCatalog(includeSensitive);
  }

  destroy(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.contextGeneration += 1;
    this.trackingGeneration += 1;
    this.catalogGeneration += 1;
    this.mutationGeneration += 1;
    this.trackingAbort?.abort();
    this.catalogAbort?.abort();
    this.mutationAbort?.abort();
  }

  private async reconcileMutation(
    personID: number,
    desired: boolean,
    context: number,
    mutation: number
  ): Promise<PersonTrackingOutcome> {
    const reconciled = await this.loadTracking(personID);
    if (!this.currentMutation(context, mutation, personID)) return { kind: 'ignored' };
    if (reconciled) {
      this.trackingError = null;
      this.stateUnknown = false;
      this.announcement = 'Profile maintenance state refreshed.';
      return { kind: 'reconciled', desired };
    }
    this.stateUnknown = true;
    this.trackingError = 'Current profile maintenance state is unknown. Retry state before changing tracking.';
    return { kind: 'unknown', desired };
  }

  private applyConfirmed(tracking: PersonTrackingState): void {
    this.tracking = tracking;
    this.trackingError = null;
    this.stateUnknown = false;
    this.announcement = tracking.tracked
      ? 'Profile maintenance tracking enabled.'
      : 'Profile maintenance tracking disabled.';
  }

  private async loadTracking(personID: number): Promise<boolean> {
    const context = this.contextGeneration;
    const request = ++this.trackingGeneration;
    this.trackingAbort?.abort();
    const abort = new AbortController();
    this.trackingAbort = abort;
    this.trackingLoading = true;
    this.trackingError = null;
    try {
      const { data } = await this.client.GET('/api/v1/people/{id}/tracking', {
        params: { path: { id: personID } },
        signal: abort.signal
      });
      if (!this.currentTracking(context, request, personID, abort.signal)) return false;
      const tracking = data ? safeTracking(data) : undefined;
      if (!tracking || tracking.person_id !== personID) {
        this.tracking = undefined;
        this.stateUnknown = true;
        this.trackingError = 'Unable to load profile maintenance state.';
        return false;
      }
      this.tracking = tracking;
      this.stateUnknown = false;
      return true;
    } catch {
      if (!this.currentTracking(context, request, personID, abort.signal)) return false;
      this.tracking = undefined;
      this.stateUnknown = true;
      this.trackingError = 'Unable to load profile maintenance state.';
      return false;
    } finally {
      if (this.currentTracking(context, request, personID)) {
        if (this.trackingAbort === abort) this.trackingAbort = undefined;
        this.trackingLoading = false;
      }
    }
  }

  private async loadCatalog(includeSensitive: boolean): Promise<boolean> {
    const personID = this.personID;
    if (personID === undefined) return false;
    const context = this.contextGeneration;
    const request = ++this.catalogGeneration;
    this.catalogAbort?.abort();
    const abort = new AbortController();
    this.catalogAbort = abort;
    this.catalogLoading = true;
    this.catalogError = null;
    try {
      const { data } = await this.client.GET('/api/v1/person-fact-targets', {
        params: { query: { include_sensitive: includeSensitive } },
        signal: abort.signal
      });
      if (!this.currentCatalog(context, request, personID, abort.signal)) return false;
      const targets = data ? safeTargets(data.targets) : undefined;
      if (!targets) {
        this.catalogError = 'Unable to load eligible profile fields.';
        return false;
      }
      this.targets = targets;
      this.catalogIncludesSensitive = includeSensitive;
      return true;
    } catch {
      if (!this.currentCatalog(context, request, personID, abort.signal)) return false;
      this.catalogError = 'Unable to load eligible profile fields.';
      return false;
    } finally {
      if (this.currentCatalog(context, request, personID)) {
        if (this.catalogAbort === abort) this.catalogAbort = undefined;
        this.catalogLoading = false;
      }
    }
  }

  private current(context: number, personID: number, signal?: AbortSignal): boolean {
    return this.isContextCurrent(context) && this.personID === personID && !signal?.aborted;
  }

  private currentTracking(context: number, request: number, personID: number, signal?: AbortSignal): boolean {
    return this.current(context, personID, signal) && this.trackingGeneration === request;
  }

  private currentCatalog(context: number, request: number, personID: number, signal?: AbortSignal): boolean {
    return this.current(context, personID, signal) && this.catalogGeneration === request;
  }

  private currentMutation(context: number, request: number, personID: number, signal?: AbortSignal): boolean {
    return this.current(context, personID, signal) && this.mutationGeneration === request;
  }
}

function safeTracking(value: GeneratedTracking): PersonTrackingState | undefined {
  if (!Number.isSafeInteger(value.person_id) || value.person_id <= 0 || typeof value.tracked !== 'boolean') return undefined;
  if (value.tracked_at !== null && typeof value.tracked_at !== 'string') return undefined;
  return { person_id: value.person_id, tracked: value.tracked, tracked_at: value.tracked_at };
}

function safeTargets(values: GeneratedTarget[] | null): PersonTrackingTarget[] | undefined {
  if (values === null) return [];
  const targets: PersonTrackingTarget[] = [];
  for (const value of values) {
    if (typeof value.kind !== 'string' || typeof value.description !== 'string' ||
      typeof value.value_type !== 'string' || typeof value.cardinality !== 'string' ||
      typeof value.sensitive !== 'boolean') return undefined;
    targets.push({
      kind: value.kind,
      description: value.description,
      value_type: value.value_type,
      cardinality: value.cardinality,
      sensitive: value.sensitive
    });
  }
  return targets;
}
