import type { DirectoryEntityMutationResult } from './models';

export class WriteAttempt<C = never> {
  busy = $state(false);
  failed = $state(false);
  conflict = $state<C>();
  details = $state('');

  async run<T>(send: (current: C | undefined) => Promise<DirectoryEntityMutationResult<T, C>>): Promise<boolean> {
    if (this.busy) return false;
    const current = this.conflict;
    this.reset();
    this.busy = true;
    try {
      const result = await send(current);
      if (result.ok) return true;
      this.failed = true;
      if (result.kind === 'conflict') this.conflict = result.current;
      this.details = result.kind === 'unknown' || result.kind === 'blocked' ? '' : result.message;
      return false;
    } finally {
      this.busy = false;
    }
  }

  reset(): void {
    this.failed = false;
    this.conflict = undefined;
    this.details = '';
  }
}
