import type { DirectoryEntityMutationResult } from './models';

export class WriteAttempt<C = never> {
  busy = $state(false);
  failed = $state(false);
  conflict = $state<C>();
  details = $state('');
  prepared = $state<C>();

  /** Loads the version a later write sends; undefined when the load failed. */
  async prepare(load: () => Promise<C>): Promise<C | undefined> {
    this.details = '';
    try {
      const current = await load();
      this.prepared = current;
      return current;
    } catch (cause) {
      this.details = cause instanceof Error ? cause.message : 'No response from the server.';
    }
  }

  async run<T>(send: (current: C | undefined) => Promise<DirectoryEntityMutationResult<T, C>>): Promise<boolean> {
    if (this.busy) return false;
    const current = this.conflict ?? this.prepared;
    this.prepared = current;
    this.failed = false;
    this.conflict = undefined;
    this.details = '';
    this.busy = true;
    try {
      const result = await send(current);
      if (result.ok) { this.prepared = undefined; return true; }
      this.failed = true;
      if (result.kind === 'conflict') this.conflict = this.prepared = result.current;
      this.details = result.kind === 'unknown' || result.kind === 'blocked' ? '' : result.message;
      return false;
    } finally {
      this.busy = false;
    }
  }

  reset(): void {
    this.failed = false;
    this.conflict = undefined;
    this.prepared = undefined;
    this.details = '';
  }
}
