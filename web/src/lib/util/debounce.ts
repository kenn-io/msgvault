export interface Debounced<T extends unknown[]> {
  (...args: T): void;
  cancel(): void;
  flush(): void;
}

export function debounce<T extends unknown[]>(
  fn: (...args: T) => void,
  delayMs: number
): Debounced<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  let pendingArgs: T | undefined;
  const debounced = (...args: T): void => {
    if (timer !== undefined) clearTimeout(timer);
    pendingArgs = args;
    timer = setTimeout(() => {
      timer = undefined;
      const args = pendingArgs;
      pendingArgs = undefined;
      if (args) fn(...args);
    }, delayMs);
  };
  debounced.cancel = (): void => {
    if (timer !== undefined) clearTimeout(timer);
    timer = undefined;
    pendingArgs = undefined;
  };
  debounced.flush = (): void => {
    if (timer === undefined) return;
    clearTimeout(timer);
    timer = undefined;
    const args = pendingArgs;
    pendingArgs = undefined;
    if (args) fn(...args);
  };
  return debounced;
}
