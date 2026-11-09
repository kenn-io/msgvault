import type { DirectoryEntityMutationResult } from './models';

export class WriteAttempt<C = never> {
  #sending = $state(false);
  #checking = $state(false);
  failed = $state(false);
  conflict = $state<C>();
  hadConflict = $state(false);
  details = $state('');
  prepared = $state<C>();
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

  get busy(): boolean {
    return this.#sending || this.#checking;
  }

  async run<T>(send: (current: C | undefined) => Promise<DirectoryEntityMutationResult<T, C>>): Promise<boolean> {
    if (this.busy) return false;
    const current = this.conflict ?? this.prepared;
    this.prepared = current;
    this.failed = false;
    this.conflict = undefined;
    this.details = '';
    this.#sending = true;
    try {
      const result = await send(current);
      if (result.ok) { this.prepared = undefined; return true; }
      this.failed = true;
      // A conflict whose refresh failed keeps the version sent, so Retry collides again and shows what changed.
      if (result.kind === 'conflict') {
        this.hadConflict = true;
        if (result.current) this.conflict = this.prepared = result.current;
      }
      this.details = result.kind === 'unknown' || result.kind === 'blocked' ? '' : result.message;
      return false;
    } finally {
      this.#sending = false;
    }
  }

  /** Runs a check that settles an uncertain write under busy, so the form stays locked and the dialog open until it ends. */
  async hold(check: () => Promise<void>): Promise<void> {
    if (this.#checking) return;
    this.#checking = true;
    try {
      await check();
    } finally {
      this.#checking = false;
    }
  }

  reset(): void {
    this.#generation++;
    this.failed = false;
    this.conflict = undefined;
    this.hadConflict = false;
    this.prepared = undefined;
    this.details = '';
  }
}
