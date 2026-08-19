const MAX_CONCURRENT_THUMBNAILS = 4;

interface Waiter {
  signal: AbortSignal;
  resolve: () => void;
  reject: (cause: unknown) => void;
  abort: () => void;
}

let active = 0;
const waiting: Waiter[] = [];

async function acquire(signal: AbortSignal): Promise<void> {
  if (signal.aborted) throw new DOMException('Thumbnail cancelled', 'AbortError');
  if (active < MAX_CONCURRENT_THUMBNAILS) {
    active += 1;
    return;
  }
  await new Promise<void>((resolve, reject) => {
    const waiter: Waiter = {
      signal,
      resolve,
      reject,
      abort: () => {
        const index = waiting.indexOf(waiter);
        if (index >= 0) waiting.splice(index, 1);
        reject(new DOMException('Thumbnail cancelled', 'AbortError'));
      }
    };
    waiting.push(waiter);
    signal.addEventListener('abort', waiter.abort, { once: true });
  });
}

function release(): void {
  active -= 1;
  while (waiting.length > 0) {
    const waiter = waiting.shift()!;
    waiter.signal.removeEventListener('abort', waiter.abort);
    if (waiter.signal.aborted) continue;
    active += 1;
    waiter.resolve();
    return;
  }
}

export async function withThumbnailSlot<T>(signal: AbortSignal, task: () => Promise<T>): Promise<T> {
  await acquire(signal);
  try {
    if (signal.aborted) throw new DOMException('Thumbnail cancelled', 'AbortError');
    return await task();
  } finally {
    release();
  }
}
