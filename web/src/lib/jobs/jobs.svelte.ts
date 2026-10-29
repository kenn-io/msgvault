import { getOperationStatus, listSourceStatus } from '../api/generated/api/api';
import type { OperationLaneStatus, SourceStatus } from '../api/generated/models';
import type { APIClient } from '../api/client';
import { collectJobs, jobsHealth, type JobItem, type JobsHealth } from './jobs';

/**
 * What is running and which latest jobs still have errors. The status dot reads
 * `health`; `JobsPopover` lists `items`. Call `refresh()` on a timer and when
 * the popover opens.
 */
export class JobsStore {
  items = $state<JobItem[]>([]);
  /** False until the first refresh answers. */
  loaded = $state(false);
  error = $state<string>();
  health = $derived.by<JobsHealth>(() => {
    const health = jobsHealth(this.items);
    return health === 'failed' ? health : this.error || !this.loaded ? 'unavailable' : health;
  });
  #sources: SourceStatus[] = [];
  #lanes: OperationLaneStatus[] = [];
  #client: APIClient;
  #controller?: AbortController;

  constructor(client: APIClient) {
    this.#client = client;
  }

  async refresh(): Promise<void> {
    this.#controller?.abort();
    const controller = (this.#controller = new AbortController());
    const options = { ...this.#client, signal: controller.signal };
    const [sources, operations] = await Promise.all([
      listSourceStatus(undefined, options).catch(() => undefined),
      getOperationStatus(options).catch(() => undefined),
    ]);
    if (controller.signal.aborted) return;
    if (sources?.data) this.#sources = sources.data.sources;
    if (operations?.data) this.#lanes = operations.data.lanes;
    this.error = !sources?.data || !operations?.data || this.#lanes.some(lane => lane.configured && lane.history_availability === 'unavailable') ? 'Unable to refresh all job status.' : undefined;
    this.items = collectJobs(this.#sources, this.#lanes);
    this.loaded = true;
  }

  dispose(): void {
    this.#controller?.abort();
  }
}
