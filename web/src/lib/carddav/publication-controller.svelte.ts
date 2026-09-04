import type { APIClient } from '../api/client';
import type { components } from '../api/generated/schema';

type GeneratedPublication = components['schemas']['CardDAVPublicationResponse'];
type GeneratedBook = components['schemas']['CardDAVAddressBookIdentityResponse'];

export type CardDAVPublicationAction = 'publish' | 'unpublish';
export type CardDAVPublicationBook = Pick<GeneratedBook, 'id' | 'name'>;
export type CardDAVPublication = Pick<GeneratedPublication, 'person_id' | 'state' | 'desired'> &
  Partial<Pick<GeneratedPublication, 'pending_operation' | 'conflict_id'>> & {
    address_book?: CardDAVPublicationBook;
  };

export type CardDAVPublicationOutcome =
  | { kind: 'confirmed'; action: CardDAVPublicationAction }
  | { kind: 'reconciled'; action: CardDAVPublicationAction }
  | { kind: 'unknown'; action: CardDAVPublicationAction }
  | { kind: 'error'; action: CardDAVPublicationAction }
  | { kind: 'ignored' };

type Snapshot =
  | { ok: true; value: CardDAVPublication }
  | { ok: false; unavailable: boolean };

export class CardDAVPublicationController {
  personID = $state<number>();
  publication = $state<CardDAVPublication>();
  loading = $state(false);
  pendingAction = $state<CardDAVPublicationAction>();
  error = $state<string | null>(null);
  unavailable = $state(false);
  stateUnknown = $state(false);
  announcement = $state<string | null>(null);

  private readonly client: APIClient;
  private disposed = false;
  private generation = 0;
  private readGeneration = 0;
  private mutationGeneration = 0;
  private readAbort?: AbortController;
  private mutationAbort?: AbortController;

  constructor(client: APIClient) {
    this.client = client;
  }

  async setPerson(personID: number): Promise<void> {
    if (this.disposed || !Number.isSafeInteger(personID) || personID <= 0) return;
    this.generation += 1;
    this.readGeneration += 1;
    this.mutationGeneration += 1;
    this.readAbort?.abort();
    this.mutationAbort?.abort();
    this.readAbort = undefined;
    this.mutationAbort = undefined;
    this.personID = personID;
    this.publication = undefined;
    this.loading = true;
    this.pendingAction = undefined;
    this.error = null;
    this.unavailable = false;
    this.stateUnknown = false;
    this.announcement = null;
    await this.load();
  }

  async load(): Promise<void> {
    if (this.personID === undefined || this.disposed) return;
    await this.readState(this.personID);
  }

  async retryState(): Promise<void> {
    if (this.personID === undefined || this.disposed || this.loading) return;
    await this.readState(this.personID);
  }

  async publish(): Promise<CardDAVPublicationOutcome> {
    return await this.mutate('publish');
  }

  async unpublish(): Promise<CardDAVPublicationOutcome> {
    return await this.mutate('unpublish');
  }

  canPublish(): boolean {
    return this.isActionAllowed('publish');
  }

  canUnpublish(): boolean {
    return this.isActionAllowed('unpublish');
  }

  destroy(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.generation += 1;
    this.readGeneration += 1;
    this.mutationGeneration += 1;
    this.readAbort?.abort();
    this.mutationAbort?.abort();
    this.readAbort = undefined;
    this.mutationAbort = undefined;
  }

  private async mutate(action: CardDAVPublicationAction): Promise<CardDAVPublicationOutcome> {
    const personID = this.personID;
    if (personID === undefined || !this.isActionAllowed(action)) return { kind: 'ignored' };
    const context = this.generation;
    const mutation = ++this.mutationGeneration;
    const controller = new AbortController();
    this.mutationAbort?.abort();
    this.mutationAbort = controller;
    this.pendingAction = action;
    this.error = null;
    this.announcement = null;
    try {
      const result = action === 'publish'
        ? await this.client.POST('/api/v1/carddav/publications/{person_id}', {
            params: { path: { person_id: personID } }, signal: controller.signal
          })
        : await this.client.DELETE('/api/v1/carddav/publications/{person_id}', {
            params: { path: { person_id: personID } }, signal: controller.signal
          });
      if (!this.currentMutation(context, mutation, personID, controller.signal)) return { kind: 'ignored' };
      const confirmed = result.data ? safePublication(result.data) : undefined;
      if (confirmed?.person_id === personID) {
        this.applyConfirmed(confirmed, action);
        return { kind: 'confirmed', action };
      }
      if (result.response.status === 400 || result.response.status === 404) {
        this.error = action === 'publish'
          ? 'Unable to publish this person to CardDAV.'
          : 'Unable to remove this person from CardDAV.';
        return { kind: 'error', action };
      }
      return await this.reconcileMutation(personID, action, context, mutation);
    } catch {
      if (!this.currentMutation(context, mutation, personID, controller.signal)) return { kind: 'ignored' };
      return await this.reconcileMutation(personID, action, context, mutation);
    } finally {
      if (this.currentMutation(context, mutation, personID)) {
        if (this.mutationAbort === controller) this.mutationAbort = undefined;
        this.pendingAction = undefined;
      }
    }
  }

  private async reconcileMutation(
    personID: number,
    action: CardDAVPublicationAction,
    context: number,
    mutation: number
  ): Promise<CardDAVPublicationOutcome> {
    const reconciled = await this.readState(personID);
    if (!this.currentMutation(context, mutation, personID)) return { kind: 'ignored' };
    if (reconciled) {
      this.error = null;
      this.announcement = 'CardDAV publication state refreshed.';
      return { kind: 'reconciled', action };
    }
    this.stateUnknown = true;
    this.error = 'Current CardDAV publication state is unknown. Retry state before changing publication.';
    return { kind: 'unknown', action };
  }

  private async readState(personID: number): Promise<boolean> {
    if (this.disposed || this.personID !== personID) return false;
    const context = this.generation;
    const request = ++this.readGeneration;
    this.readAbort?.abort();
    const controller = new AbortController();
    this.readAbort = controller;
    this.loading = true;
    this.error = null;
    const snapshot = await this.fetchState(personID, controller.signal);
    if (!this.currentRead(context, request, personID, controller.signal)) return false;
    if (snapshot.ok && snapshot.value.person_id === personID) {
      this.publication = snapshot.value;
      this.unavailable = false;
      this.stateUnknown = false;
    } else if (!snapshot.ok && snapshot.unavailable) {
      this.publication = undefined;
      this.unavailable = true;
      this.stateUnknown = false;
    } else {
      this.publication = undefined;
      this.unavailable = false;
      this.stateUnknown = true;
      this.error = 'Unable to load CardDAV publication state.';
    }
    if (this.readAbort === controller) this.readAbort = undefined;
    this.loading = false;
    return snapshot.ok && snapshot.value.person_id === personID;
  }

  private async fetchState(personID: number, signal: AbortSignal): Promise<Snapshot> {
    try {
      const { data, error } = await this.client.GET('/api/v1/carddav/publications/{person_id}', {
        params: { path: { person_id: personID } }, signal
      });
      const value = data ? safePublication(data) : undefined;
      if (value) return { ok: true, value };
      return { ok: false, unavailable: error?.error === 'carddav_unavailable' };
    } catch {
      return { ok: false, unavailable: false };
    }
  }

  private isActionAllowed(action: CardDAVPublicationAction): boolean {
    if (this.disposed || this.loading || this.pendingAction !== undefined || this.stateUnknown) return false;
    const publication = this.publication;
    if (!publication || publication.person_id !== this.personID) return false;
    if (action === 'publish') return publication.state === 'unpublished' && publication.address_book !== undefined;
    return publication.state === 'published';
  }

  private applyConfirmed(publication: CardDAVPublication, action: CardDAVPublicationAction): void {
    this.publication = publication;
    this.unavailable = false;
    this.stateUnknown = false;
    this.error = null;
    const book = publication.address_book?.name;
    if (publication.state === 'pending') {
      this.announcement = 'CardDAV publication change is pending.';
    } else if (publication.state === 'conflict') {
      this.announcement = 'CardDAV publication needs conflict review.';
    } else if (publication.state === 'published') {
      this.announcement = `Published this person to CardDAV${book ? ` in ${book}` : ''}.`;
    } else {
      this.announcement = `Removed this person from CardDAV${book ? ` in ${book}` : ''}.`;
    }
  }

  private current(generation: number, personID: number, signal?: AbortSignal): boolean {
    return !this.disposed && this.generation === generation && this.personID === personID && !signal?.aborted;
  }

  private currentRead(generation: number, request: number, personID: number, signal?: AbortSignal): boolean {
    return this.current(generation, personID, signal) && this.readGeneration === request;
  }

  private currentMutation(generation: number, mutation: number, personID: number, signal?: AbortSignal): boolean {
    return this.current(generation, personID, signal) && this.mutationGeneration === mutation;
  }
}

function safePublication(value: GeneratedPublication): CardDAVPublication | undefined {
  if (!Number.isSafeInteger(value.person_id) || value.person_id <= 0) return undefined;
  const addressBook = value.address_book && Number.isSafeInteger(value.address_book.id) && value.address_book.id > 0
    ? { id: value.address_book.id, name: value.address_book.name }
    : undefined;
  const conflictID = value.conflict_id !== undefined && Number.isSafeInteger(value.conflict_id) && value.conflict_id > 0
    ? value.conflict_id
    : undefined;
  return {
    person_id: value.person_id,
    state: value.state,
    desired: value.desired,
    ...(addressBook ? { address_book: addressBook } : {}),
    ...(value.pending_operation !== undefined ? { pending_operation: value.pending_operation } : {}),
    ...(conflictID !== undefined ? { conflict_id: conflictID } : {})
  };
}
