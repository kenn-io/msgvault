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
  health = $derived<JobsHealth>(this.items.some(item => item.state === 'failed') ? 'failed' : this.error || !this.loaded ? 'unavailable' : jobsHealth(this.items));
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
    try {
      const [sources, operations] = await Promise.all([
        listSourceStatus(undefined, options).catch(() => undefined),
        getOperationStatus(options).catch(() => undefined),
      ]);
      if (controller.signal.aborted) return;
      if (sources?.data) this.#sources = sources.data.sources;
      if (operations?.data) this.#lanes = operations.data.lanes;
      this.error = !sources?.data || !operations?.data ? 'Unable to refresh all job status.' : undefined;
      this.items = collectJobs(this.#sources, this.#lanes);
      this.loaded = true;
    } catch {
      // A failed refresh keeps the last answer; the next one corrects it.
    }
  }

  dispose(): void {
    this.#controller?.abort();
  }
}
