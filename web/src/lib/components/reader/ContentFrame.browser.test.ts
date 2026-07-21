import { describe, expect, it, vi } from 'vitest';

import { createArchivedContentMessageHandler } from './ContentFrame.browser.svelte';

function event(source: Window, data: unknown, origin = 'null'): MessageEvent {
  return { source, data, origin } as MessageEvent;
}

describe('archived content frame message boundary', () => {
  it('accepts only the expected opaque frame window, nonce, and exact schema', () => {
    const frameWindow = {} as Window;
    const otherWindow = {} as Window;
    const onKey = vi.fn();
    const onScroll = vi.fn();
    const handle = createArchivedContentMessageHandler({
      frameWindow: () => frameWindow,
      nonce: () => 'secret-nonce',
      onKey,
      onScroll
    });
    const valid = {
      channel: 'msgvault-archived-content',
      nonce: 'secret-nonce',
      type: 'key',
      key: 'Escape'
    };

    handle(event(frameWindow, valid));
    handle(event(otherWindow, valid));
    handle(event(frameWindow, valid, 'https://archive.example'));
    handle(event(frameWindow, { ...valid, nonce: 'wrong' }));
    handle(event(frameWindow, { ...valid, type: 'close' }));
    handle(event(frameWindow, { ...valid, extra: true }));
    handle(event(frameWindow, { ...valid, key: 'x' }));
    handle(event(frameWindow, ['msgvault-archived-content', 'secret-nonce']));

    expect(onKey).toHaveBeenCalledTimes(1);
    expect(onKey).toHaveBeenCalledWith('Escape');
    expect(onScroll).not.toHaveBeenCalled();
  });

  it('accepts finite scroll deltas only through the exact scroll schema', () => {
    const frameWindow = {} as Window;
    const onScroll = vi.fn();
    const handle = createArchivedContentMessageHandler({
      frameWindow: () => frameWindow,
      nonce: () => 'nonce',
      onKey: vi.fn(),
      onScroll
    });
    const base = { channel: 'msgvault-archived-content', nonce: 'nonce', type: 'scroll' };

    handle(event(frameWindow, { ...base, deltaY: 12.5 }));
    handle(event(frameWindow, { ...base, deltaY: Number.POSITIVE_INFINITY }));
    handle(event(frameWindow, { ...base, deltaY: 10_001 }));
    handle(event(frameWindow, { ...base, deltaY: -10_001 }));
    handle(event(frameWindow, { ...base, deltaY: '12' }));
    handle(event(frameWindow, { ...base, deltaY: 1, key: 'Escape' }));

    expect(onScroll).toHaveBeenCalledTimes(1);
    expect(onScroll).toHaveBeenCalledWith(12.5);
  });
});
