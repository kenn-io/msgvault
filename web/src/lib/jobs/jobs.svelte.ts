import { untrack } from 'svelte';
import { listSourceStatus } from '../api/generated/api/api';
import type { OperationLaneStatus, SourceStatus } from '../api/generated/models';
import type { APIClient } from '../api/client';
import type { OperationsController } from '../operations/controller.svelte';
import { collectJobs, jobsHealth, type JobsHealth } from './jobs';

/**
 * What is running and which latest jobs still have errors. The status dot reads
 * `health`; `JobsPopover` lists `items`. Call `refresh()` on a timer and when
 * the popover opens.
 */
export class JobsStore {
  items = $derived.by(() => collectJobs(this.#sources, this.#kinds));
  /** Both source and operation status have answered. */
  loaded = $derived.by(() => this.#sourceError !== undefined && (this.#operations.snapshot.statusReadable || Boolean(this.#operations.snapshot.statusError)));
  error = $derived.by(() => this.loaded && (this.#sourceError || !this.#operations.snapshot.statusReadable || this.#operations.snapshot.statusError ||
    this.#operations.snapshot.statusLanes.some(lane => lane.kinds.some(kind => kind.configured && kind.history_availability === 'unavailable')))
    ? 'Unable to refresh all job status.' : undefined);
  health = $derived.by<JobsHealth>(() => {
    const health = jobsHealth(this.items);
    return health !== 'ok' ? health : this.error || !this.loaded ? 'unavailable' : health;
  });
  #sources = $state<SourceStatus[]>([]);
  #kinds = $state<OperationLaneStatus[]>([]);
  #stopObserving: () => void;
  #sourceError = $state<boolean>();
  #operations: OperationsController;
  #client: APIClient;
  #controller?: AbortController;

  constructor(client: APIClient, operations: OperationsController) {
    this.#client = client;
    this.#operations = operations;
    this.#stopObserving = $effect.root(() => {
      $effect(() => {
        const kinds = this.#operations.snapshot.statusLanes.flatMap(lane => lane.kinds);
        this.#kinds = untrack(() => kinds.map(kind => {
          const previous = this.#kinds.find(candidate => candidate.kind === kind.kind);
          return kind.history_availability === 'unavailable'
            ? { ...kind, active: previous?.active, latest: previous?.latest }
            : kind;
        }));
      });
    });
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
  }

  dispose(): void {
    this.#stopObserving();
    this.#controller?.abort();
  }
}
