import { describe, expect, it } from 'vitest';

import { createStaleRequestGuard } from './stale-request';

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

describe('stale request guard', () => {
  it('rejects an older response when a newer request resolves first', async () => {
    const guard = createStaleRequestGuard();
    const first = deferred<string>();
    const second = deferred<string>();
    const accepted: string[] = [];

    const firstGeneration = guard.begin();
    const firstCommit = first.promise.then((value) =>
      guard.commit(firstGeneration, () => accepted.push(value))
    );
    const secondGeneration = guard.begin();
    const secondCommit = second.promise.then((value) =>
      guard.commit(secondGeneration, () => accepted.push(value))
    );

    second.resolve('new result');
    await expect(secondCommit).resolves.toBe(true);
    first.resolve('old result');
    await expect(firstCommit).resolves.toBe(false);
    expect(accepted).toEqual(['new result']);
  });

  it('invalidates an in-flight response without starting another request', () => {
    const guard = createStaleRequestGuard();
    const generation = guard.begin();
    guard.invalidate();

    expect(guard.commit(generation, () => {})).toBe(false);
  });
});
