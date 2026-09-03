import { SvelteMap } from 'svelte/reactivity';

import type { APIClient } from '../api/client';
import type { components } from '../api/generated/schema';

type CardDAVStatus = components['schemas']['CardDAVStatusResponse'];
type CardDAVRun = components['schemas']['CardDAVRunResponse'];
type GeneratedBook = components['schemas']['CardDAVBookResponse'];
export type CardDAVBookRoles = components['schemas']['CardDAVBookRolesRequest'];
export type CardDAVBook = Pick<GeneratedBook,
  'id' | 'name' | 'subscribed' | 'lookup_source' | 'write_target' | 'needs_full_reconcile'>;

interface VisibilityDocument {
  readonly hidden: boolean;
  addEventListener(type: 'visibilitychange', listener: () => void): void;
  removeEventListener(type: 'visibilitychange', listener: () => void): void;
}

const FIRST_PAGE_LIMIT = 25;
const MIN_POLL_MS = 500;
const MAX_POLL_MS = 8_000;

export class CardDAVController {
  status = $state<CardDAVStatus>();
  books = $state<CardDAVBook[]>([]);
  runs = $state<CardDAVRun[]>([]);
  nextBeforeID = $state<number>();

  statusLoading = $state(true);
  booksLoading = $state(true);
  runsLoading = $state(true);
  runsPageLoading = $state(false);
  syncPending = $state(false);
  bookPendingID = $state<number>();

  statusError = $state<string | null>(null);
  booksError = $state<string | null>(null);
  runsError = $state<string | null>(null);
  runsPageError = $state<string | null>(null);
  syncError = $state<string | null>(null);
  syncStatus = $state<string | null>(null);
  syncUnknown = $state(false);
  bookError = $state<string | null>(null);
  bookStatus = $state<string | null>(null);
  booksUnknown = $state(false);

  private readonly drafts = new SvelteMap<number, CardDAVBookRoles>();
  private readonly client: APIClient;
  private readonly visibilityDocument?: VisibilityDocument;
  private disposed = false;
  private generation = 1;
  private statusReadAbort?: AbortController;
  private booksReadAbort?: AbortController;
  private runsReadAbort?: AbortController;
  private pollAbort?: AbortController;
  private syncAbort?: AbortController;
  private bookMutationAbort?: AbortController;
  private pollTimer?: ReturnType<typeof setTimeout>;
  private pollGeneration = 1;
  private pollDelay = MIN_POLL_MS;
  private pollFingerprint = '';
  private statusCommitGeneration = 0;
  private successfulStatusCommitGeneration = 0;
  private syncKnownAfterStatusCommit?: number;
  private failedRunsCursor?: number;

  private readonly visibilityChanged = (): void => {
    if (this.visibilityDocument?.hidden) {
      this.stopPolling();
      return;
    }
    if (this.shouldPoll()) {
      this.pollDelay = MIN_POLL_MS;
      this.schedulePoll(MIN_POLL_MS);
    }
  };

  constructor(client: APIClient, visibilityDocument: VisibilityDocument | undefined =
    typeof document === 'undefined' ? undefined : document) {
    this.client = client;
    this.visibilityDocument = visibilityDocument;
    visibilityDocument?.addEventListener('visibilitychange', this.visibilityChanged);
  }

  async load(): Promise<void> {
    await Promise.all([this.readStatus(true), this.loadBooks(false), this.refreshRuns()]);
  }

  async retryStatus(): Promise<void> {
    await this.readStatus(true);
  }

  async retryBooks(): Promise<void> {
    await this.loadBooks(this.books.length > 0 || this.booksUnknown);
  }

  async retryRuns(): Promise<void> {
    if (this.runsPageError && this.failedRunsCursor !== undefined) {
      await this.loadRunsPage(this.failedRunsCursor);
      return;
    }
    await this.refreshRuns();
  }

  async refreshRuns(): Promise<boolean> {
    const context = this.generation;
    this.runsReadAbort?.abort();
    const requestController = new AbortController();
    this.runsReadAbort = requestController;
    this.runsLoading = this.runs.length === 0;
    this.runsError = null;
    this.runsPageError = null;
    this.failedRunsCursor = undefined;
    try {
      const { data } = await this.client.GET('/api/v1/carddav/runs', {
        params: { query: { limit: FIRST_PAGE_LIMIT } },
        signal: requestController.signal
      });
      if (!this.current(context, requestController.signal)) return false;
      if (!data) {
        this.runsError = 'Unable to load CardDAV history.';
        return false;
      }
      this.runs = uniqueRuns(data.runs);
      this.nextBeforeID = data.next_before_id;
      return true;
    } catch {
      if (this.current(context, requestController.signal)) {
        this.runsError = 'Unable to load CardDAV history.';
      }
      return false;
    } finally {
      if (this.current(context)) {
        if (this.runsReadAbort === requestController) this.runsReadAbort = undefined;
        this.runsLoading = false;
      }
    }
  }

  async loadMoreRuns(): Promise<void> {
    if (this.runsPageLoading || this.nextBeforeID === undefined) return;
    await this.loadRunsPage(this.nextBeforeID);
  }

  setBookDraft(id: number, roles: CardDAVBookRoles): void {
    this.drafts.set(id, normalizedRoles(roles));
    this.bookError = null;
    this.bookStatus = null;
  }

  bookDraft(id: number): CardDAVBookRoles | undefined {
    const draft = this.drafts.get(id);
    return draft ? { ...draft } : undefined;
  }

  rolesFor(book: CardDAVBook): CardDAVBookRoles {
    return this.bookDraft(book.id) ?? rolesFromBook(book);
  }

  async setBookRoles(id: number, roles: CardDAVBookRoles): Promise<void> {
    const intended = normalizedRoles(roles);
    this.drafts.set(id, intended);
    if (!this.canSetBookRoles) return;
    const context = this.generation;
    this.bookMutationAbort?.abort();
    const requestController = new AbortController();
    this.bookMutationAbort = requestController;
    this.bookPendingID = id;
    this.bookError = null;
    this.bookStatus = null;
    let reconcile = false;
    let committed = false;
    try {
      const { data, response } = await this.client.PATCH('/api/v1/carddav/books/{id}', {
        params: { path: { id } },
        body: intended,
        signal: requestController.signal
      });
      if (!this.current(context, requestController.signal)) return;
      if (data) {
        committed = true;
        reconcile = true;
        this.drafts.delete(id);
        this.bookStatus = 'Address-book roles saved. All books were refreshed.';
      } else if (response.status === 409 || response.status >= 500) {
        reconcile = true;
        this.bookError = 'Address-book roles may have changed. Current books were refreshed; review and apply again.';
      } else {
        this.bookError = 'Unable to save address-book roles.';
      }
    } catch {
      if (!this.current(context, requestController.signal)) return;
      reconcile = true;
      this.bookError = 'Address-book role result is uncertain. Current books were refreshed; review and apply again.';
    } finally {
      if (this.current(context)) {
        if (this.bookMutationAbort === requestController) this.bookMutationAbort = undefined;
      }
    }
    if (!this.current(context) || !reconcile) {
      if (this.current(context)) this.bookPendingID = undefined;
      return;
    }
    const loaded = await this.loadBooks(true);
    if (!loaded && this.current(context)) {
      this.booksUnknown = true;
      this.bookError = committed
        ? 'Address-book roles were saved, but current book state is unknown. Retry book state before editing.'
        : 'Address-book role result is uncertain and current book state is unknown. Retry book state before editing.';
    }
    if (this.current(context)) this.bookPendingID = undefined;
  }

  async sync(full: boolean): Promise<void> {
    if (!this.canSync || this.syncPending) return;
    const context = this.generation;
    this.syncAbort?.abort();
    const requestController = new AbortController();
    this.syncAbort = requestController;
    this.syncPending = true;
    this.syncError = null;
    this.syncStatus = null;
    this.pollDelay = MIN_POLL_MS;
    this.schedulePoll(MIN_POLL_MS);
    let succeeded = false;
    let reconcile = false;
    try {
      const { data, response } = await this.client.POST('/api/v1/carddav/sync', {
        body: { full },
        signal: requestController.signal
      });
      if (!this.current(context, requestController.signal)) return;
      if (data) {
        succeeded = true;
        reconcile = true;
        this.syncStatus = full ? 'Full CardDAV sync completed.' : 'CardDAV sync completed.';
      } else if (response.status === 409 || response.status >= 500) {
        reconcile = true;
        this.syncError = 'Unable to complete CardDAV sync. Current state was refreshed.';
      } else {
        this.syncError = 'Unable to start CardDAV sync.';
      }
    } catch {
      if (!this.current(context, requestController.signal)) return;
      reconcile = true;
      this.syncError = 'Unable to complete CardDAV sync. Current state was refreshed.';
    } finally {
      if (this.current(context)) {
        if (this.syncAbort === requestController) this.syncAbort = undefined;
      }
    }
    if (!this.current(context)) return;
    this.stopPolling();
    if (reconcile) {
      const [statusLoaded, runsLoaded] = await Promise.all([
        this.readStatus(false),
        this.refreshRuns(),
        ...(succeeded ? [this.loadBooks(true)] : [])
      ]);
      this.syncUnknown = !statusLoaded || !runsLoaded;
    }
    this.syncPending = false;
    if (this.shouldPoll()) this.schedulePoll(MIN_POLL_MS);
  }

  async retrySyncState(): Promise<void> {
    if (this.disposed || this.syncPending) return;
    const context = this.generation;
    const statusCommitFloor = this.statusCommitGeneration;
    this.syncKnownAfterStatusCommit = undefined;
    this.syncPending = true;
    const statusRead = this.readStatus(false);
    const reconciliationStatusCommit = this.statusCommitGeneration;
    const [statusLoaded, runsLoaded] = await Promise.all([statusRead, this.refreshRuns()]);
    if (!this.current(context)) return;
    const statusKnown = statusLoaded || this.successfulStatusCommitGeneration > statusCommitFloor;
    this.syncUnknown = !statusKnown || !runsLoaded;
    this.syncKnownAfterStatusCommit = runsLoaded && !statusKnown ? statusCommitFloor : undefined;
    this.syncPending = false;
    if (!this.syncUnknown) this.syncError = null;
    if (this.shouldPoll() && this.statusCommitGeneration === reconciliationStatusCommit) {
      this.schedulePoll(MIN_POLL_MS);
    }
  }

  get canSync(): boolean {
    return Boolean(
      !this.disposed &&
      !this.statusLoading &&
      !this.statusError &&
      this.status?.configured &&
      this.status.available &&
      this.status.credential_configured &&
      !this.syncUnknown &&
      !this.status.active &&
      !this.syncPending
    );
  }

  get canSetBookRoles(): boolean {
    return Boolean(
      !this.disposed &&
      !this.statusLoading &&
      !this.statusError &&
      !this.booksLoading &&
      !this.booksError &&
      this.status?.configured &&
      this.status.available &&
      this.status.credential_configured &&
      !this.booksUnknown &&
      this.bookPendingID === undefined
    );
  }

  destroy(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.generation += 1;
    this.visibilityDocument?.removeEventListener('visibilitychange', this.visibilityChanged);
    this.stopPolling();
    this.statusReadAbort?.abort();
    this.booksReadAbort?.abort();
    this.runsReadAbort?.abort();
    this.syncAbort?.abort();
    this.bookMutationAbort?.abort();
  }

  private async readStatus(schedule: boolean): Promise<boolean> {
    const context = this.generation;
    const statusCommit = ++this.statusCommitGeneration;
    this.statusReadAbort?.abort();
    const requestController = new AbortController();
    this.statusReadAbort = requestController;
    this.statusLoading = true;
    this.statusError = null;
    try {
      const { data } = await this.client.GET('/api/v1/carddav/status', { signal: requestController.signal });
      if (!this.currentStatusCommit(context, statusCommit, requestController.signal)) return false;
      if (!data) {
        this.statusError = 'Unable to load CardDAV status.';
        return false;
      }
      this.status = data;
      this.pollFingerprint = statusFingerprint(data);
      this.recordSuccessfulStatusCommit(statusCommit);
      return true;
    } catch {
      if (this.currentStatusCommit(context, statusCommit, requestController.signal)) {
        this.statusError = 'Unable to load CardDAV status.';
      }
      return false;
    } finally {
      if (schedule && this.currentStatusCommit(context, statusCommit, requestController.signal) && this.shouldPoll()) {
        this.pollDelay = MIN_POLL_MS;
        this.schedulePoll(MIN_POLL_MS);
      }
      if (this.current(context) && this.statusReadAbort === requestController) {
        this.statusReadAbort = undefined;
        this.statusLoading = false;
      }
    }
  }

  private async loadBooks(reconciliation: boolean): Promise<boolean> {
    const context = this.generation;
    this.booksReadAbort?.abort();
    const requestController = new AbortController();
    this.booksReadAbort = requestController;
    this.booksLoading = this.books.length === 0;
    this.booksError = null;
    try {
      const { data } = await this.client.GET('/api/v1/carddav/books', { signal: requestController.signal });
      if (!this.current(context, requestController.signal)) return false;
      if (!data) {
        this.booksError = 'Unable to load CardDAV address books.';
        this.booksUnknown = true;
        return false;
      }
      this.books = (data.books ?? []).map(safeBook);
      const liveIDs = new Set(this.books.map(({ id }) => id));
      for (const id of this.drafts.keys()) if (!liveIDs.has(id)) this.drafts.delete(id);
      this.booksUnknown = false;
      return true;
    } catch {
      if (this.current(context, requestController.signal)) {
        this.booksError = 'Unable to load CardDAV address books.';
        this.booksUnknown = true;
      }
      return false;
    } finally {
      if (this.current(context)) {
        if (this.booksReadAbort === requestController) this.booksReadAbort = undefined;
        this.booksLoading = false;
      }
    }
  }

  private async loadRunsPage(cursor: number): Promise<void> {
    const context = this.generation;
    this.runsReadAbort?.abort();
    const requestController = new AbortController();
    this.runsReadAbort = requestController;
    this.runsPageLoading = true;
    this.runsPageError = null;
    this.failedRunsCursor = cursor;
    try {
      const { data } = await this.client.GET('/api/v1/carddav/runs', {
        params: { query: { limit: FIRST_PAGE_LIMIT, before_id: cursor } },
        signal: requestController.signal
      });
      if (!this.current(context, requestController.signal)) return;
      if (!data) {
        this.runsPageError = 'Unable to load more CardDAV history.';
        return;
      }
      this.runs = uniqueRuns([...this.runs, ...data.runs]);
      this.nextBeforeID = data.next_before_id;
      this.failedRunsCursor = undefined;
    } catch {
      if (this.current(context, requestController.signal)) {
        this.runsPageError = 'Unable to load more CardDAV history.';
      }
    } finally {
      if (this.current(context)) {
        if (this.runsReadAbort === requestController) this.runsReadAbort = undefined;
        this.runsPageLoading = false;
      }
    }
  }

  private shouldPoll(): boolean {
    return !this.disposed && !this.visibilityDocument?.hidden && Boolean(this.status?.active || this.syncPending);
  }

  private schedulePoll(delay: number): void {
    if (!this.shouldPoll()) return;
    if (this.pollTimer !== undefined) clearTimeout(this.pollTimer);
    const generation = ++this.pollGeneration;
    this.pollTimer = setTimeout(() => {
      this.pollTimer = undefined;
      void this.poll(generation);
    }, delay);
  }

  private async poll(generation: number): Promise<void> {
    if (this.disposed || generation !== this.pollGeneration || this.visibilityDocument?.hidden) return;
    const context = this.generation;
    const statusCommit = ++this.statusCommitGeneration;
    this.pollAbort?.abort();
    const requestController = new AbortController();
    this.pollAbort = requestController;
    const priorActive = Boolean(this.status?.active);
    try {
      const { data } = await this.client.GET('/api/v1/carddav/status', { signal: requestController.signal });
      if (!this.currentStatusCommit(context, statusCommit, requestController.signal) || generation !== this.pollGeneration) return;
      this.statusLoading = false;
      if (!data) {
        this.statusError = 'Unable to load CardDAV status.';
        this.pollDelay = Math.min(MAX_POLL_MS, this.pollDelay * 2);
        this.schedulePoll(this.pollDelay);
        return;
      }
      const fingerprint = statusFingerprint(data);
      const advanced = fingerprint !== this.pollFingerprint;
      this.status = data;
      this.statusError = null;
      this.pollFingerprint = fingerprint;
      this.recordSuccessfulStatusCommit(statusCommit);
      if (priorActive && !data.active && !this.syncPending) {
        this.stopPolling(false);
        await Promise.all([this.loadBooks(true), this.refreshRuns()]);
        return;
      }
      if (this.shouldPoll()) {
        this.pollDelay = advanced ? MIN_POLL_MS : Math.min(MAX_POLL_MS, this.pollDelay * 2);
        this.schedulePoll(this.pollDelay);
      }
    } catch {
      if (!this.currentStatusCommit(context, statusCommit, requestController.signal) || generation !== this.pollGeneration) return;
      this.statusLoading = false;
      this.statusError = 'Unable to load CardDAV status.';
      this.pollDelay = Math.min(MAX_POLL_MS, this.pollDelay * 2);
      this.schedulePoll(this.pollDelay);
    } finally {
      if (this.pollAbort === requestController) this.pollAbort = undefined;
    }
  }

  private stopPolling(invalidate = true): void {
    if (invalidate) this.pollGeneration += 1;
    if (this.pollTimer !== undefined) clearTimeout(this.pollTimer);
    this.pollTimer = undefined;
    this.pollAbort?.abort();
    this.pollAbort = undefined;
  }

  private current(generation: number, signal?: AbortSignal): boolean {
    return !this.disposed && this.generation === generation && !signal?.aborted;
  }

  private currentStatusCommit(generation: number, statusCommit: number, signal?: AbortSignal): boolean {
    return this.current(generation, signal) && this.statusCommitGeneration === statusCommit;
  }

  private recordSuccessfulStatusCommit(statusCommit: number): void {
    this.successfulStatusCommitGeneration = statusCommit;
    if (this.syncKnownAfterStatusCommit === undefined || statusCommit <= this.syncKnownAfterStatusCommit) return;
    this.syncKnownAfterStatusCommit = undefined;
    this.syncUnknown = false;
    this.syncError = null;
  }
}

function safeBook(book: GeneratedBook): CardDAVBook {
  return {
    id: book.id,
    name: book.name,
    subscribed: book.subscribed,
    lookup_source: book.lookup_source,
    write_target: book.write_target,
    needs_full_reconcile: book.needs_full_reconcile
  };
}

function rolesFromBook(book: CardDAVBook): CardDAVBookRoles {
  return {
    subscribed: book.subscribed,
    lookup_source: book.lookup_source,
    write_target: book.write_target
  };
}

function normalizedRoles(roles: CardDAVBookRoles): CardDAVBookRoles {
  return {
    subscribed: roles.subscribed || roles.write_target,
    lookup_source: roles.lookup_source,
    write_target: roles.write_target
  };
}

function uniqueRuns(runs: CardDAVRun[]): CardDAVRun[] {
  const seen = new Set<number>();
  return runs.filter((run) => !seen.has(run.id) && Boolean(seen.add(run.id)));
}

function statusFingerprint(status: CardDAVStatus): string {
  const run = status.active;
  if (!run) return `idle:${status.latest?.id ?? 0}:${status.latest?.state ?? ''}`;
  return [run.id, run.state, run.books, run.created, run.updated, run.removed].join(':');
}
