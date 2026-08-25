export interface BufferedCallback<T extends unknown[]> {
  (...args: T): void;
  cancel(): void;
  flush(): void;
}

/**
 * Buffers a callback until input settles while still allowing navigation to
 * commit the pending value synchronously.
 */
export function bufferedCallback<T extends unknown[]>(
  fn: (...args: T) => void,
  delayMs: number
): BufferedCallback<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  let pendingArgs: T | undefined;
  const buffered = (...args: T): void => {
    if (timer !== undefined) clearTimeout(timer);
    pendingArgs = args;
    timer = setTimeout(() => {
      timer = undefined;
      const args = pendingArgs;
      pendingArgs = undefined;
      if (args) fn(...args);
    }, delayMs);
  };
  buffered.cancel = (): void => {
    if (timer !== undefined) clearTimeout(timer);
    timer = undefined;
    pendingArgs = undefined;
  };
  buffered.flush = (): void => {
    if (timer === undefined) return;
    clearTimeout(timer);
    timer = undefined;
    const args = pendingArgs;
    pendingArgs = undefined;
    if (args) fn(...args);
  };
  return buffered;
}
