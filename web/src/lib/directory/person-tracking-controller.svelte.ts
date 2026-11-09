import {
  getPersonTracking as generatedGetPersonTracking,
  setPersonTracking as generatedSetPersonTracking,
} from '../api/generated/api/api';
import type { APIClient } from '../api/client';
import type {
  PersonTracking as GeneratedPersonTracking,
} from '../api/generated/models';
type GeneratedTracking = GeneratedPersonTracking;
export type PersonTrackingState = Pick<GeneratedTracking, 'person_id' | 'tracked' | 'tracked_at'>;
export type PersonTrackingOutcome =
  | {
      kind: 'confirmed';
      desired: boolean;
    }
  | {
      kind: 'reconciled';
      desired: boolean;
    }
  | {
      kind: 'unknown';
      desired: boolean;
    }
  | {
      kind: 'error';
      desired: boolean;
    }
  | {
      kind: 'ignored';
    };
export class PersonTrackingController {
  personID = $state<number>();
  tracking = $state<PersonTrackingState>();
  trackingLoading = $state(false);
  pending = $state(false);
  trackingError = $state<string | null>(null);
  stateUnknown = $state(false);
  /** The switch position a save didn't reach, while the read-back shows it didn't land. Retry saves it again. */
  failedSave = $state<boolean | null>(null);
  announcement = $state<string | null>(null);
  private readonly client: APIClient;
  private disposed = false;
  private contextGeneration = 0;
  private trackingGeneration = 0;
  private mutationGeneration = 0;
  private trackingAbort?: AbortController;
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
    this.mutationGeneration += 1;
    this.trackingAbort?.abort();
    this.mutationAbort?.abort();
    this.personID = personID;
    this.tracking = undefined;
    this.trackingError = null;
    this.pending = false;
    this.stateUnknown = false;
    this.failedSave = null;
    this.announcement = null;
    await this.loadTracking(personID);
  }
  async setTracked(desired: boolean): Promise<PersonTrackingOutcome> {
    const personID = this.personID;
    if (
      personID === undefined ||
      this.disposed ||
      this.pending ||
      this.trackingLoading ||
      this.stateUnknown ||
      !this.tracking ||
      this.tracking.person_id !== personID ||
      this.tracking.tracked === desired
    )
      return { kind: 'ignored' };
    const context = this.contextGeneration;
    const mutation = ++this.mutationGeneration;
    this.mutationAbort?.abort();
    const abort = new AbortController();
    this.mutationAbort = abort;
    this.pending = true;
    this.trackingError = null;
    this.failedSave = null;
    this.announcement = null;
    try {
      const result = await generatedSetPersonTracking(
        { id: personID },
        { tracked: desired },
        {
          ...this.client,
          signal: abort.signal,
        },
      );
      if (!this.currentMutation(context, mutation, personID, abort.signal)) return { kind: 'ignored' };
      const confirmed = result.data ? safeTracking(result.data) : undefined;
      if (confirmed?.person_id === personID && confirmed.tracked === desired) {
        this.applyConfirmed(confirmed);
        return { kind: 'confirmed', desired };
      }
      if (result.response.status === 400 || result.response.status === 404) {
        this.failedSave = desired;
        this.trackingError = "Couldn't save.";
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
  /** Saves the position a failed save didn't reach. */
  async retrySave(): Promise<PersonTrackingOutcome> {
    const desired = this.failedSave;
    if (desired === null) return { kind: 'ignored' };
    return await this.setTracked(desired);
  }
  destroy(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.contextGeneration += 1;
    this.trackingGeneration += 1;
    this.mutationGeneration += 1;
    this.trackingAbort?.abort();
    this.mutationAbort?.abort();
  }
  private async reconcileMutation(
    personID: number,
    desired: boolean,
    context: number,
    mutation: number,
  ): Promise<PersonTrackingOutcome> {
    const reconciled = await this.loadTracking(personID);
    if (!this.currentMutation(context, mutation, personID)) return { kind: 'ignored' };
    if (reconciled && this.tracking?.tracked === desired) {
      this.trackingError = null;
      this.stateUnknown = false;
      this.announcement = announce(desired);
      return { kind: 'reconciled', desired };
    }
    // The read-back shows the switch where it was: the save didn't land, and Retry saves it again.
    if (reconciled) {
      this.failedSave = desired;
      this.trackingError = "Couldn't save.";
      return { kind: 'error', desired };
    }
    this.stateUnknown = true;
    this.trackingError = "Couldn't check whether the change was saved.";
    return { kind: 'unknown', desired };
  }
  private applyConfirmed(tracking: PersonTrackingState): void {
    this.tracking = tracking;
    this.trackingError = null;
    this.stateUnknown = false;
    this.failedSave = null;
    this.announcement = announce(tracking.tracked);
  }
  private async loadTracking(personID: number): Promise<boolean> {
    const context = this.contextGeneration;
    const request = ++this.trackingGeneration;
    this.trackingAbort?.abort();
    const abort = new AbortController();
    this.trackingAbort = abort;
    this.trackingLoading = true;
    this.trackingError = null;
    this.failedSave = null;
    try {
      const { data } = await generatedGetPersonTracking(
        { id: personID },
        {
          ...this.client,
          signal: abort.signal,
        },
      );
      if (!this.currentTracking(context, request, personID, abort.signal)) return false;
      const tracking = data ? safeTracking(data) : undefined;
      if (!tracking || tracking.person_id !== personID) {
        this.tracking = undefined;
        this.stateUnknown = true;
        this.trackingError = "Couldn't check this profile's updates.";
        return false;
      }
      this.tracking = tracking;
      this.stateUnknown = false;
      return true;
    } catch {
      if (!this.currentTracking(context, request, personID, abort.signal)) return false;
      this.tracking = undefined;
      this.stateUnknown = true;
      this.trackingError = "Couldn't check this profile's updates.";
      return false;
    } finally {
      if (this.currentTracking(context, request, personID)) {
        if (this.trackingAbort === abort) this.trackingAbort = undefined;
        this.trackingLoading = false;
      }
    }
  }
  private current(context: number, personID: number, signal?: AbortSignal): boolean {
    return this.isContextCurrent(context) && this.personID === personID && !signal?.aborted;
  }
  private currentTracking(context: number, request: number, personID: number, signal?: AbortSignal): boolean {
    return this.current(context, personID, signal) && this.trackingGeneration === request;
  }
  private currentMutation(context: number, request: number, personID: number, signal?: AbortSignal): boolean {
    return this.current(context, personID, signal) && this.mutationGeneration === request;
  }
}
function announce(tracked: boolean): string {
  return tracked ? 'Keeping this profile updated.' : 'No longer keeping this profile updated.';
}
function safeTracking(value: GeneratedTracking): PersonTrackingState | undefined {
  if (!Number.isSafeInteger(value.person_id) || value.person_id <= 0 || typeof value.tracked !== 'boolean')
    return undefined;
  if (value.tracked_at !== null && typeof value.tracked_at !== 'string') return undefined;
  return { person_id: value.person_id, tracked: value.tracked, tracked_at: value.tracked_at };
}
