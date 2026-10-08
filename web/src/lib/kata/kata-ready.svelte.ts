import { getContext, onDestroy, setContext } from 'svelte';
import { getKataIntegrationStatus } from '../api/generated/api/api';
import type { APIClient } from '../api/client';

export const kataReadinessKey = Symbol('kata-readiness');
export const KATA_RETRY_MS = 30_000;

// The page asks Kata once, the first time an action needs the answer, and
// every card shares it. Kata settings apply only after a daemon restart.
export class KataReadiness {
  #client: APIClient;
  #ready = $state(false);
  #started = false;
  #controller?: AbortController;
  #retry?: ReturnType<typeof setTimeout>;

  constructor(client: APIClient) {
    this.#client = client;
  }

  // Kata actions show only when the integration is ready; Settings explains any other state.
  get ready(): boolean {
    if (!this.#started) {
      this.#started = true;
      queueMicrotask(() => this.#check(true));
    }
    return this.#ready;
  }

  dispose(): void {
    this.#controller?.abort();
    clearTimeout(this.#retry);
  }

  #check(retryOnFailure: boolean): void {
    this.#started = true;
    this.dispose();
    const controller = this.#controller = new AbortController();
    // A failed check, or Kata out of reach, is no lasting answer: mounted cards
    // get one retry shortly, and the next read asks again.
    const failed = () => {
      if (controller.signal.aborted) return;
      this.#started = false;
      if (retryOnFailure) this.#retry = setTimeout(() => this.#check(false), KATA_RETRY_MS);
    };
    void getKataIntegrationStatus({ ...this.#client, signal: controller.signal }).then(({ data }) => {
      if (!data || data.state === 'unreachable') return failed();
      if (!controller.signal.aborted) this.#ready = data.state === 'ready';
    }).catch(failed);
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
