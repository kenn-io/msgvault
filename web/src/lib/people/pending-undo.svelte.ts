/**
 * Holds one change that waits out its Undo window. The caller hides the thing
 * it removed while this is pending, and `commit` runs once the window closes.
 * A second change commits the first at once, and leaving the page does too, so
 * no change is lost.
 */
export class PendingUndo {
  current = $state<{ id: number; key: string; message: string } | null>(null);
  error = $state('');
  private committing = $state<string[]>([]);
  private commit: (() => Promise<void> | void) | undefined;
  private onUndo: (() => void) | undefined;
  private counter = 0;

  /** Whether the thing with this key is hidden while its Undo toast shows. */
  hides(key: string): boolean {
    return this.current?.key === key || this.committing.includes(key);
  }

  start(key: string, message: string, commit: () => Promise<void> | void, onUndo?: () => void): void {
    void this.flush();
    this.error = '';
    this.counter += 1;
    this.current = { id: this.counter, key, message };
    this.commit = commit;
    this.onUndo = onUndo;
  }

  undo(): void {
    const onUndo = this.onUndo;
    this.current = null;
    this.commit = undefined;
    this.onUndo = undefined;
    onUndo?.();
  }

  /** Makes the pending change final. Runs when the Undo window closes. */
  async flush(): Promise<void> {
    const commit = this.commit;
    const key = this.current?.key;
    if (key && commit) this.committing = [...this.committing, key];
    this.current = null;
    this.commit = undefined;
    this.onUndo = undefined;
    try { await commit?.(); } catch (cause) { this.error = cause instanceof Error ? cause.message : 'Could not remove this value.'; }
    finally { if (key) this.committing = this.committing.filter((value) => value !== key); }
  }
}
