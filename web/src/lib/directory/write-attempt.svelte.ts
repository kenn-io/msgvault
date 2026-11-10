import type { DirectoryEntityMutationResult } from './models';

export class WriteAttempt<C = never> {
  #sending = $state(false);
  failed = $state(false);
  conflict = $state<C>();
  details = $state('');
  prepared = $state<C>();
  /** Set when a conflict's own refresh failed, so the held version is stale until reload() succeeds. */
  reloadNeeded = $state(false);
  // Bumped by reset(), so a load started for a dialog that has since closed or changed target is dropped.
  #generation = 0;

  /** Loads the version a later write sends; undefined when the load failed or the attempt was reset meanwhile. */
  async prepare(load: () => Promise<C>): Promise<C | undefined> {
    const generation = this.#generation;
    this.details = '';
    try {
      const current = await load();
      if (generation !== this.#generation) return undefined;
      this.prepared = current;
      return current;
    } catch (cause) {
      if (generation === this.#generation) this.details = cause instanceof Error ? cause.message : 'No response from the server.';
    }
  }

  /** Shows the saved version after a conflict whose refresh failed, so the next run sends it. */
  async reload(load: () => Promise<C>): Promise<void> {
    const current = await this.prepare(load);
    if (current) { this.conflict = current; this.reloadNeeded = false; }
  }

  get busy(): boolean {
    return this.#sending;
  }

  async run<T>(send: (current: C | undefined) => Promise<DirectoryEntityMutationResult<T, C>>): Promise<boolean> {
    if (this.busy) return false;
    const current = this.conflict ?? this.prepared;
    this.prepared = current;
    this.failed = this.reloadNeeded = false;
    this.conflict = undefined;
    this.details = '';
    this.#sending = true;
    try {
      const result = await send(current);
      if (result.ok) { this.prepared = undefined; return true; }
      this.failed = true;
      if (result.kind === 'conflict' && result.current) this.conflict = this.prepared = result.current;
      this.reloadNeeded = result.kind === 'conflict' && !result.current;
      this.details = result.kind === 'unknown' || result.kind === 'blocked' ? '' : result.message;
      return false;
    } finally {
      this.#sending = false;
    }
  }

  reset(): void {
    this.#generation++;
    this.failed = this.reloadNeeded = false;
    this.conflict = undefined;
    this.prepared = undefined;
    this.details = '';
  }
}
