import { listSourceStatus } from '../api/generated/api/api';
import type { SourceStatus } from '../api/generated/models';
import type { APIClient } from '../api/client';
import type { OperationsController } from '../operations/controller.svelte';
import { collectJobs, jobsHealth, type JobsHealth } from './jobs';

/**
 * What is running and which latest jobs still have errors. The status dot reads
 * `health`; `JobsPopover` lists `items`. Call `refresh()` on a timer and when
 * the popover opens.
 */
export class JobsStore {
  items = $derived.by(() => collectJobs(this.#sources, this.#operations.snapshot.statusLanes.flatMap(lane => lane.kinds)));
  /** False until the first refresh answers. */
  loaded = $state(false);
  error = $derived.by(() => this.loaded && (this.#sourceError || !this.#operations.snapshot.statusReadable || this.#operations.snapshot.statusError ||
    this.#operations.snapshot.statusLanes.some(lane => lane.kinds.some(kind => kind.configured && kind.history_availability === 'unavailable')))
    ? 'Unable to refresh all job status.' : undefined);
  health = $derived.by<JobsHealth>(() => {
    const health = jobsHealth(this.items);
    return health !== 'ok' ? health : this.error || !this.loaded ? 'unavailable' : health;
  });
  #sources = $state<SourceStatus[]>([]);
  #sourceError = $state(false);
  #operations: OperationsController;
  #client: APIClient;
  #controller?: AbortController;

  constructor(client: APIClient, operations: OperationsController) {
    this.#client = client;
    this.#operations = operations;
  }

  setSources(sources: SourceStatus[]): void {
    this.#controller?.abort();
    this.#sources = sources;
    this.#sourceError = false;
  }

  async refresh(supersede = false): Promise<void> {
    this.#controller?.abort();
    const controller = (this.#controller = new AbortController());
    const options = { ...this.#client, signal: controller.signal };
    const [sources] = await Promise.all([
      listSourceStatus(undefined, options).catch(() => undefined),
      this.#operations.refreshStatus(supersede),
    ]);
    if (this.#controller !== controller) return;
    if (!controller.signal.aborted) {
      if (sources?.data) this.#sources = sources.data.sources;
      this.#sourceError = !sources?.data;
    }
    this.loaded = true;
  }

  dispose(): void {
    this.#controller?.abort();
  }
}
