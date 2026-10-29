import { getOperationStatus, listSourceStatus } from '../api/generated/api/api';
import type { APIClient } from '../api/client';
import { collectJobs, jobsHealth, type JobItem, type JobsHealth } from './jobs';

/**
 * What is running and what failed in the last seven days. The status dot reads
 * `health`; `JobsPopover` lists `items`. Call `refresh()` on a timer and when
 * the popover opens.
 */
export class JobsStore {
  items = $state<JobItem[]>([]);
  health = $derived<JobsHealth>(jobsHealth(this.items));
  /** False until the first refresh answers. */
  loaded = $state(false);
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
      if (controller.signal.aborted || !sources?.data && !operations?.data) return;
      this.items = collectJobs(sources?.data?.sources ?? [], operations?.data?.lanes ?? [], Date.now());
      this.loaded = true;
    } catch {
      // A failed refresh keeps the last answer; the next one corrects it.
    }
  }

  dispose(): void {
    this.#controller?.abort();
  }
}
