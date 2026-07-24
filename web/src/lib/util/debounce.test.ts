import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { debounce } from './debounce';

describe('debounce', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('fires once with the latest arguments after the delay', () => {
    const fn = vi.fn();
    const debounced = debounce(fn, 250);
    debounced('a');
    debounced('ab');
    debounced('abc');
    expect(fn).not.toHaveBeenCalled();
    vi.advanceTimersByTime(250);
    expect(fn).toHaveBeenCalledTimes(1);
    expect(fn).toHaveBeenCalledWith('abc');
  });

  it('restarts the delay on each call', () => {
    const fn = vi.fn();
    const debounced = debounce(fn, 250);
    debounced('a');
    vi.advanceTimersByTime(200);
    debounced('ab');
    vi.advanceTimersByTime(200);
    expect(fn).not.toHaveBeenCalled();
    vi.advanceTimersByTime(50);
    expect(fn).toHaveBeenCalledTimes(1);
    expect(fn).toHaveBeenCalledWith('ab');
  });

  it('cancel discards the pending call', () => {
    const fn = vi.fn();
    const debounced = debounce(fn, 250);
    debounced('a');
    debounced.cancel();
    vi.advanceTimersByTime(1000);
    expect(fn).not.toHaveBeenCalled();
  });

  it('flush runs the pending call immediately with its captured arguments', () => {
    const fn = vi.fn();
    const debounced = debounce(fn, 250);
    debounced('a');
    debounced.flush();
    expect(fn).toHaveBeenCalledTimes(1);
    expect(fn).toHaveBeenCalledWith('a');
    vi.advanceTimersByTime(1000);
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it('flush is a no-op when nothing is pending', () => {
    const fn = vi.fn();
    const debounced = debounce(fn, 250);
    debounced.flush();
    expect(fn).not.toHaveBeenCalled();
  });
});
