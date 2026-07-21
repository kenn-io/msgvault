import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import SplitPane from './SplitPane.svelte';

const observers = new Map<Element, ResizeObserverCallback>();

class ResizeObserverMock {
  constructor(private callback: ResizeObserverCallback) {}

  observe(target: Element): void {
    observers.set(target, this.callback);
  }

  unobserve(target: Element): void {
    observers.delete(target);
  }

  disconnect(): void {
    observers.clear();
  }
}

vi.stubGlobal('ResizeObserver', ResizeObserverMock);

afterEach(() => {
  localStorage.clear();
  observers.clear();
});

function reportWidth(target: Element, width: number): void {
  const callback = observers.get(target);
  if (!callback) throw new Error('split pane was not observed');
  callback(
    [{ target, contentRect: { width } } as unknown as ResizeObserverEntry],
    {} as ResizeObserver
  );
}

describe('SplitPane', () => {
  it('remains usable when the storage accessor throws during reads and writes', async () => {
    const storageDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
    let container: HTMLElement | undefined;

    Object.defineProperty(globalThis, 'localStorage', {
      configurable: true,
      get() {
        throw new Error('storage is unavailable');
      }
    });

    try {
      expect(() => {
        ({ container } = render(SplitPane, {
          props: { storageKey: 'archive:test-split', ariaLabel: 'Resize result list' }
        }));
      }).not.toThrow();

      const primary = container!.querySelector('[data-pane="primary"]') as HTMLElement;
      expect(primary.style.flexBasis).toBe('360px');

      await fireEvent.keyDown(screen.getByRole('button', { name: 'Resize result list' }), {
        key: 'ArrowRight'
      });
      expect(primary.style.flexBasis).toBe('384px');
    } finally {
      if (storageDescriptor) {
        Object.defineProperty(globalThis, 'localStorage', storageDescriptor);
      }
    }
  });

  it('restores, resizes, and persists the primary pane size', async () => {
    localStorage.setItem('archive:test-split', '480');
    const { container } = render(SplitPane, {
      props: { storageKey: 'archive:test-split', ariaLabel: 'Resize result list' }
    });
    const primary = container.querySelector('[data-pane="primary"]') as HTMLElement;

    expect(primary.style.flexBasis).toBe('480px');
    await fireEvent.keyDown(screen.getByRole('button', { name: 'Resize result list' }), {
      key: 'ArrowRight'
    });

    expect(primary.style.flexBasis).toBe('504px');
    expect(localStorage.getItem('archive:test-split')).toBe('504');
  });

  it('clamps restored and resized sizes as available width changes', async () => {
    localStorage.setItem('archive:test-split', '900');
    const { container } = render(SplitPane, {
      props: { storageKey: 'archive:test-split', ariaLabel: 'Resize result list' }
    });
    const host = container.querySelector('[data-split-pane]')!;
    const primary = container.querySelector('[data-pane="primary"]') as HTMLElement;

    reportWidth(host, 700);
    await waitFor(() => expect(primary.style.flexBasis).toBe('376px'));

    await fireEvent.keyDown(screen.getByRole('button', { name: 'Resize result list' }), {
      key: 'ArrowRight'
    });
    expect(primary.style.flexBasis).toBe('376px');

    reportWidth(host, 500);
    await waitFor(() => expect(primary.style.flexBasis).toBe('176px'));
    expect(localStorage.getItem('archive:test-split')).toBe('176');
  });
});
