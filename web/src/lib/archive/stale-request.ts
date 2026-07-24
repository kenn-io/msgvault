export interface StaleRequestGuard {
  begin(): number;
  invalidate(): void;
  isCurrent(generation: number): boolean;
  commit(generation: number, effect: () => void): boolean;
}

/**
 * Tracks the newest asynchronous request without assuming cancellation support.
 * Callers may still abort network work separately; this guard prevents an old
 * completion from replacing state owned by a newer request.
 */
export function createStaleRequestGuard(): StaleRequestGuard {
  let currentGeneration = 0;

  return {
    begin() {
      currentGeneration += 1;
      return currentGeneration;
    },
    invalidate() {
      currentGeneration += 1;
    },
    isCurrent(generation) {
      return generation === currentGeneration;
    },
    commit(generation, effect) {
      if (generation !== currentGeneration) return false;
      effect();
      return true;
    }
  };
}
