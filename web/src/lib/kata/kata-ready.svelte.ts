import { getContext, onDestroy, setContext } from 'svelte';
import { getKataIntegrationStatus } from '../api/generated/api/api';
import type { APIClient } from '../api/client';

export const kataReadinessKey = Symbol('kata-readiness');
export const KATA_RETRY_MS = 30_000;

// Page-scoped integration settings apply only after a daemon restart.
export class IntegrationReadiness {
  #probe: (signal: AbortSignal) => Promise<boolean | undefined>;
  #ready = $state<boolean>();
  #started = false;
  #controller?: AbortController;
  #retry?: ReturnType<typeof setTimeout>;

  constructor(probe: (signal: AbortSignal) => Promise<boolean | undefined>) {
    this.#probe = probe;
  }

  get ready(): boolean {
    if (!this.#started) {
      this.#started = true;
      queueMicrotask(() => this.#check(true));
    }
    return this.#ready === true;
  }

  get absent(): boolean { this.ready; return this.#ready === false; }

  dispose(): void {
    this.#controller?.abort();
    clearTimeout(this.#retry);
  }

  #check(retryOnFailure: boolean): void {
    this.#started = true;
    this.dispose();
    const controller = this.#controller = new AbortController();
    // An inconclusive check gets one retry shortly, and the next read asks again.
    const failed = () => {
      if (controller.signal.aborted) return;
      this.#started = false;
      if (retryOnFailure) this.#retry = setTimeout(() => this.#check(false), KATA_RETRY_MS);
    };
    void this.#probe(controller.signal).then((ready) => {
      if (ready === undefined) return failed();
      if (!controller.signal.aborted) this.#ready = ready;
    }).catch(failed);
  }
}

export class KataReadiness extends IntegrationReadiness {
  constructor(client: APIClient) {
    super(async signal => {
      const { data } = await getKataIntegrationStatus({ ...client, signal });
      return !data || data.state === 'unreachable' ? undefined : data.state === 'ready';
    });
  }
}

// Call during the page's component setup.
export function provideKataReadiness(client: APIClient): KataReadiness {
  const readiness = new KataReadiness(client);
  onDestroy(() => readiness.dispose());
  return setContext(kataReadinessKey, readiness);
}

// Without a page that provides it, Kata actions stay hidden. Call during component setup.
export function kataReadiness(): KataReadiness | undefined {
  return getContext<KataReadiness | undefined>(kataReadinessKey);
}
