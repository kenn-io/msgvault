import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { bufferedCallback } from './buffered-callback';

describe('buffered callback', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('flushes the latest pending value before navigation', () => {
    const fn = vi.fn();
    const buffered = bufferedCallback(fn, 250);
    buffered('a');
    buffered('ab');

    buffered.flush();

    expect(fn).toHaveBeenCalledOnce();
    expect(fn).toHaveBeenCalledWith('ab');
    vi.advanceTimersByTime(1000);
    expect(fn).toHaveBeenCalledOnce();
  });

  it('cancels a pending value when history restores older state', () => {
    const fn = vi.fn();
    const buffered = bufferedCallback(fn, 250);
    buffered('new draft');

    buffered.cancel();
    vi.advanceTimersByTime(1000);

    expect(fn).not.toHaveBeenCalled();
  });
});
