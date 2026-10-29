import { listSourceStatus } from '../api/generated/api/api';
import type { SourceStatus } from '../api/generated/models';
import type { APIClient } from '../api/client';
import type { OperationsController } from '../operations/controller.svelte';
import { collectJobs, jobsHealth, type JobsHealth } from './jobs';

export const SOURCE_STATUS_TIMEOUT_MS = 20_000;
export type SourceRead = Awaited<ReturnType<typeof listSourceStatus>>;
type PendingSourceRead = { controller: AbortController; promise: Promise<SourceRead>; waiters: number };

/**
 * What is running and which latest jobs still have errors. The status dot reads
 * `health`; `JobsPopover` lists `items`. Call `refresh()` on a timer and when
 * the popover opens.
 */
export class JobsStore {
  items = $derived.by(() => collectJobs(this.#sources, this.#operations.snapshot.statusLanes.flatMap(lane => lane.kinds)));
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
  #sourceError = $state<boolean>();
  #operations: OperationsController;
  #client: APIClient;
  #sourceRead?: PendingSourceRead;
  #disposal = new AbortController();

  constructor(client: APIClient, operations: OperationsController) {
    this.#client = client;
    this.#operations = operations;
  }

  async readSources(signal: AbortSignal, supersede = false): Promise<SourceRead> {
    this.#disposal.signal.throwIfAborted();
    signal.throwIfAborted();
    if (supersede || !this.#sourceRead) {
      this.#sourceRead?.controller.abort();
      const controller = new AbortController();
      const requestSignal = AbortSignal.any([controller.signal, AbortSignal.timeout(SOURCE_STATUS_TIMEOUT_MS)]);
      const read: PendingSourceRead = {
        controller, waiters: 0,
        promise: listSourceStatus(undefined, { ...this.#client, signal: requestSignal }).then(result => {
          requestSignal.throwIfAborted();
          if (this.#sourceRead === read) {
            if (result.data) this.#sources = result.data.sources;
            this.#sourceError = !result.data;
          }
          return result;
        }).catch(error => {
          if (this.#sourceRead === read && !controller.signal.aborted) this.#sourceError = true;
          throw error;
        }).finally(() => { if (this.#sourceRead === read) this.#sourceRead = undefined; }),
      };
      this.#sourceRead = read;
    }
    const read = this.#sourceRead;
    read.waiters += 1;
    let released = false;
    const release = () => {
      if (released) return;
      released = true;
      if (--read.waiters === 0 && this.#sourceRead === read) {
        this.#sourceRead = undefined;
        read.controller.abort();
      }
    };
    let leave!: () => void;
    try {
      return await new Promise<SourceRead>((resolve, reject) => {
        leave = () => { release(); reject(signal.reason); };
        signal.addEventListener('abort', leave, { once: true });
        read.promise.then(resolve, reject);
      });
    } finally {
      signal.removeEventListener('abort', leave);
      release();
    }
  }

  async refresh(supersede = false): Promise<void> {
    if (this.#disposal.signal.aborted) return;
    await Promise.all([
      this.readSources(this.#disposal.signal, supersede).catch(() => undefined),
      this.#operations.refreshStatus(supersede),
    ]);
  }

  dispose(): void {
    this.#disposal.abort();
    this.#sourceRead?.controller.abort();
    this.#sourceRead = undefined;
  }
}
