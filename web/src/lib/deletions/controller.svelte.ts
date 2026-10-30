import { cancelDeletion, getDeletion, listDeletions } from '../api/generated/api/api';
import type { APIClient } from '../api/client';
import type { DeletionManifestDetail, DeletionManifestSummary } from '../api/generated/models';

const WEEK = 7 * 24 * 60 * 60 * 1000;
const terminal = (status: string) => status === 'completed' || status === 'failed';
export function batchIsRecent(batch: Pick<DeletionManifestSummary, 'status' | 'created_at'>, now: number, completedAt?: string): boolean {
  return batch.status === 'pending' || batch.status === 'in_progress' || (terminal(batch.status) && now - Date.parse(completedAt || batch.created_at) <= WEEK);
}

/** One bounded detail cache for sidebar retention and staged deletion history. */
export class DeletionsController {
  batches = $state<DeletionManifestSummary[]>([]);
  details = $state<Record<string, DeletionManifestDetail>>({});
  visible = $state(false);
  loading = $state(false);
  error = $state('');
  #client: APIClient;
  #timeout: number;
  #disposed = false;
  #loaded = false;
  #revision = 0;
  #list?: Promise<void>;
  #trailing?: Promise<void>;
  #retryRequested = false;
  #listController?: AbortController;
  #failed = new Set<string>();
  #pending = new Map<string, Promise<DeletionManifestDetail | undefined>>();
  #controllers = new Map<string, AbortController>();
  #queue: Array<() => void> = [];
  #running = 0;

  constructor(client: APIClient, timeoutMs = 20_000) { this.#client = client; this.#timeout = timeoutMs; }

  refresh(retry = false): Promise<void> {
    if (this.#disposed) return Promise.resolve();
    if (this.#list) {
      this.#retryRequested ||= retry;
      return this.#trailing ??= this.#list.then(() => {
        const retry = this.#retryRequested;
        this.#retryRequested = false;
        this.#trailing = undefined;
        return this.refresh(retry);
      });
    }
    if (retry) this.#failed.clear();
    this.loading = !this.#loaded;
    const controller = this.#listController = new AbortController();
    this.#list = (async () => {
      try {
        const { data } = await listDeletions(undefined, { ...this.#client, signal: AbortSignal.any([controller.signal, AbortSignal.timeout(this.#timeout)]) });
        if (controller.signal.aborted || this.#disposed) return;
        if (!data) { this.error = "Can't load staged deletions."; return; }
        const next = data.manifests ?? [];
        const previous = new Map(this.batches.map(batch => [batch.id, batch]));
        const activeDetails: string[] = [];
        for (const batch of next) {
          const old = previous.get(batch.id);
          if (old && (old.status !== batch.status || (batch.status === 'in_progress' && this.details[batch.id]))) {
            if (batch.status === 'in_progress' && this.details[batch.id]) activeDetails.push(batch.id);
            this.#controllers.get(batch.id)?.abort();
            this.#pending.delete(batch.id);
            this.#failed.delete(batch.id);
            const details = { ...this.details };
            delete details[batch.id];
            this.details = details;
          }
        }
        const revision = ++this.#revision;
        this.batches = next;
        this.#loaded = true;
        this.error = '';
        this.loading = false;
        void this.#retention(revision);
        activeDetails.forEach(id => void this.getDetail(id));
      } catch { if (!controller.signal.aborted && !this.#disposed) this.error = "Can't load staged deletions."; }
      finally { this.loading = false; this.#list = undefined; }
    })();
    return this.#list;
  }

  getDetail(id: string, retry = false): Promise<DeletionManifestDetail | undefined> {
    if (this.#disposed) return Promise.resolve(undefined);
    if (this.details[id]) return Promise.resolve(this.details[id]);
    const pending = this.#pending.get(id);
    if (pending) return pending;
    if (this.#failed.has(id) && !retry) return Promise.resolve(undefined);
    this.#failed.delete(id);
    const status = this.batches.find(batch => batch.id === id)?.status;
    const controller = new AbortController();
    this.#controllers.set(id, controller);
    const request = new Promise<DeletionManifestDetail | undefined>(resolve => {
      this.#queue.push(() => {
        if (this.#disposed || controller.signal.aborted) { resolve(undefined); return; }
        this.#running++;
        let timeout: ReturnType<typeof setTimeout>;
        const stopped = new Promise<undefined>(done => {
          controller.signal.addEventListener('abort', () => done(undefined), { once: true });
          timeout = setTimeout(() => controller.abort(), this.#timeout);
        });
        void Promise.race([
          getDeletion({ id }, { ...this.#client, signal: controller.signal }).then(({ data }) => data),
          stopped,
        ]).catch(() => undefined).then(data => {
          const current = !this.#disposed && this.#controllers.get(id) === controller && this.batches.find(batch => batch.id === id)?.status === status;
          if (current) {
            if (data && !controller.signal.aborted) {
              this.details = { ...this.details, [id]: data };
              const batch = this.batches.find(batch => batch.id === id);
              if (batch && batchIsRecent(batch, Date.now(), data.execution?.completed_at)) this.visible = true;
            } else this.#failed.add(id);
          }
          return current && !controller.signal.aborted ? data : undefined;
        }).finally(() => {
          clearTimeout(timeout);
          if (this.#controllers.get(id) === controller) { this.#controllers.delete(id); this.#pending.delete(id); }
          this.#running--;
          this.#next();
        }).then(resolve);
      });
    });
    this.#pending.set(id, request);
    this.#next();
    return request;
  }

  #next(): void { while (this.#running < 4 && this.#queue.length) this.#queue.shift()!(); }

  async #retention(revision = this.#revision): Promise<void> {
    const now = Date.now();
    if (this.batches.some(batch => batchIsRecent(batch, now, this.details[batch.id]?.execution?.completed_at))) { this.visible = true; return; }
    const older = this.batches.filter(batch => terminal(batch.status));
    if (!older.length) { this.visible = false; return; }
    let index = 0, found = false, failed = false;
    await Promise.all(Array.from({ length: Math.min(4, older.length) }, async () => {
      while (index < older.length && !found && !this.#disposed && revision === this.#revision) {
        const batch = older[index++];
        const detail = await this.getDetail(batch.id, true);
        if (revision !== this.#revision || this.#disposed) return;
        if (!detail) failed = true;
        else if (batchIsRecent(batch, now, detail.execution?.completed_at)) { found = true; this.visible = true; }
      }
    }));
    if (!this.#disposed && revision === this.#revision && (found || !failed)) this.visible = found;
  }

  async cancel(id: string): Promise<void> {
    const { data, error } = await cancelDeletion({ id }, this.#client);
    if (!data) throw new Error(typeof error === 'object' && error && 'message' in error && typeof error.message === 'string' ? error.message : "Can't cancel the batch.");
    this.#controllers.get(id)?.abort();
    this.#controllers.delete(id);
    this.#pending.delete(id);
    this.#revision++;
    this.batches = this.batches.map(batch => batch.id === id ? { ...batch, status: data.status } : batch);
    if (this.details[id]) this.details = { ...this.details, [id]: { ...this.details[id], status: data.status } };
    void this.#retention();
  }

  destroy(): void {
    this.#disposed = true;
    this.#listController?.abort();
    this.#controllers.forEach(controller => controller.abort());
    this.#next();
  }
}
