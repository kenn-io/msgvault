import { cancelDeletion, listDeletions } from '../api/generated/api/api';
import type { APIClient } from '../api/client';
import type { DeletionManifestSummary } from '../api/generated/models';

/** Shared staged deletion list and cancellation. */
export class DeletionsController {
  batches = $state<DeletionManifestSummary[]>([]);
  visible = $derived(this.batches.some(batch => ['pending', 'in_progress', 'failed'].includes(batch.status)));
  loading = $state(false);
  error = $state('');
  #client: APIClient;
  #disposed = false;
  #loaded = false;
  #list?: Promise<void>;
  #trailing?: Promise<void>;
  #listController?: AbortController;

  constructor(client: APIClient) { this.#client = client; }

  refresh(): Promise<void> {
    if (this.#disposed) return Promise.resolve();
    if (this.#list) {
      return this.#trailing ??= this.#list.then(() => {
        this.#trailing = undefined;
        return this.refresh();
      });
    }
    this.loading = !this.#loaded;
    const controller = this.#listController = new AbortController();
    this.#list = (async () => {
      try {
        const { data } = await listDeletions(undefined, { ...this.#client, signal: controller.signal });
        if (controller.signal.aborted || this.#disposed) return;
        if (!data) { this.error = "Can't load staged deletions."; return; }
        const next = data.manifests ?? [];
        this.batches = next;
        this.#loaded = true;
        this.error = '';
        this.loading = false;
      } catch { if (!controller.signal.aborted && !this.#disposed) this.error = "Can't load staged deletions."; }
      finally { this.loading = false; this.#list = undefined; }
    })();
    return this.#list;
  }

  async cancel(id: string): Promise<void> {
    const { data, error } = await cancelDeletion({ id }, this.#client);
    if (!data) throw new Error(typeof error?.message === 'string' ? error.message : "Can't cancel the batch.");
    this.#listController?.abort();
    this.batches = this.batches.map(batch => batch.id === id ? { ...batch, status: data.status } : batch);
  }

  destroy(): void {
    this.#disposed = true;
    this.#listController?.abort();
  }
}
