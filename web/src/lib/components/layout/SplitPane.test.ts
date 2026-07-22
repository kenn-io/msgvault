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

function reportSize(target: Element, size: { width?: number; height?: number }): void {
  const callback = observers.get(target);
  if (!callback) throw new Error('split pane was not observed');
  callback(
    [{ target, contentRect: size } as unknown as ResizeObserverEntry],
    {} as ResizeObserver
  );
}

function reportWidth(target: Element, width: number): void {
  reportSize(target, { width });
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

  it('caps a horizontal primary pane at maxPrimary even when more width is available', async () => {
    localStorage.setItem('archive:test-split', '900');
    const { container } = render(SplitPane, {
      props: {
        storageKey: 'archive:test-split',
        ariaLabel: 'Resize relationship list',
        initialSize: 300,
        minPrimary: 240,
        maxPrimary: 440
      }
    });
    const host = container.querySelector('[data-split-pane]')!;
    const primary = container.querySelector('[data-pane="primary"]') as HTMLElement;

    reportWidth(host, 1600);
    await waitFor(() => expect(primary.style.flexBasis).toBe('440px'));

    const handle = screen.getByRole('button', { name: 'Resize relationship list' });
    await fireEvent.keyDown(handle, { key: 'ArrowRight' });
    expect(primary.style.flexBasis).toBe('440px');

    for (let step = 0; step < 12; step += 1) {
      await fireEvent.keyDown(handle, { key: 'ArrowLeft' });
    }
    expect(primary.style.flexBasis).toBe('240px');
  });

  it('double-clicking the handle resets to the default size and forgets the persisted value', async () => {
    const { container } = render(SplitPane, {
      props: {
        storageKey: 'archive:test-split',
        ariaLabel: 'Resize relationship list',
        initialSize: 300,
        minPrimary: 240,
        maxPrimary: 440
      }
    });
    const host = container.querySelector('[data-split-pane]')!;
    const primary = container.querySelector('[data-pane="primary"]') as HTMLElement;
    reportWidth(host, 1200);
    await waitFor(() => expect(primary.style.flexBasis).toBe('300px'));

    const handle = screen.getByRole('button', { name: 'Resize relationship list' });
    await fireEvent.keyDown(handle, { key: 'ArrowRight' });
    expect(primary.style.flexBasis).toBe('324px');
    expect(localStorage.getItem('archive:test-split')).toBe('324');

    await fireEvent.dblClick(handle);
    expect(primary.style.flexBasis).toBe('300px');
    expect(localStorage.getItem('archive:test-split')).toBeNull();
  });

  it('double-clicking a vertical handle resets to the proportional default', async () => {
    localStorage.setItem('archive:reading-pane', '600');
    const { container } = render(SplitPane, {
      props: {
        storageKey: 'archive:reading-pane',
        ariaLabel: 'Resize reading pane',
        orientation: 'vertical',
        initialFraction: 0.55,
        minPrimary: 120,
        minSecondary: 160
      }
    });
    const host = container.querySelector('[data-split-pane]')!;
    const secondary = container.querySelector('[data-pane="secondary"]') as HTMLElement;
    reportSize(host, { height: 800 });
    await waitFor(() => expect(secondary.style.flexBasis).toBe('600px'));

    await fireEvent.dblClick(screen.getByRole('button', { name: 'Resize reading pane' }));
    expect(secondary.style.flexBasis).toBe('440px');
    expect(localStorage.getItem('archive:reading-pane')).toBeNull();
  });

  it('sizes an untouched vertical secondary pane as a fraction of the container without persisting', async () => {
    const { container } = render(SplitPane, {
      props: {
        storageKey: 'archive:reading-pane',
        ariaLabel: 'Resize reading pane',
        orientation: 'vertical',
        initialFraction: 0.55,
        minPrimary: 120,
        minSecondary: 160
      }
    });
    const host = container.querySelector('[data-split-pane]')!;
    const secondary = container.querySelector('[data-pane="secondary"]') as HTMLElement;

    reportSize(host, { height: 800 });
    await waitFor(() => expect(secondary.style.flexBasis).toBe('440px'));
    expect(localStorage.getItem('archive:reading-pane')).toBeNull();

    // The proportional default keeps following the container until the user
    // resizes explicitly.
    reportSize(host, { height: 1000 });
    await waitFor(() => expect(secondary.style.flexBasis).toBe('550px'));
    expect(localStorage.getItem('archive:reading-pane')).toBeNull();
  });

  it('persists an explicit vertical resize and restores it on the next mount', async () => {
    const first = render(SplitPane, {
      props: {
        storageKey: 'archive:reading-pane',
        ariaLabel: 'Resize reading pane',
        orientation: 'vertical',
        initialFraction: 0.55,
        minPrimary: 120,
        minSecondary: 160
      }
    });
    const host = first.container.querySelector('[data-split-pane]')!;
    const secondary = first.container.querySelector('[data-pane="secondary"]') as HTMLElement;
    reportSize(host, { height: 800 });
    await waitFor(() => expect(secondary.style.flexBasis).toBe('440px'));

    await fireEvent.keyDown(screen.getByRole('button', { name: 'Resize reading pane' }), {
      key: 'ArrowUp'
    });
    expect(secondary.style.flexBasis).toBe('464px');
    expect(localStorage.getItem('archive:reading-pane')).toBe('464');

    await fireEvent.keyDown(screen.getByRole('button', { name: 'Resize reading pane' }), {
      key: 'ArrowDown'
    });
    expect(secondary.style.flexBasis).toBe('440px');
    expect(localStorage.getItem('archive:reading-pane')).toBe('440');

    first.unmount();
    const second = render(SplitPane, {
      props: {
        storageKey: 'archive:reading-pane',
        ariaLabel: 'Resize reading pane',
        orientation: 'vertical',
        initialFraction: 0.55
      }
    });
    const restored = second.container.querySelector('[data-pane="secondary"]') as HTMLElement;
    expect(restored.style.flexBasis).toBe('440px');
  });

  it('drags a vertical handle with the pointer, growing the pane as the handle moves up', async () => {
    const { container } = render(SplitPane, {
      props: {
        storageKey: 'archive:reading-pane',
        ariaLabel: 'Resize reading pane',
        orientation: 'vertical',
        initialSize: 300,
        minPrimary: 120,
        minSecondary: 160
      }
    });
    const host = container.querySelector('[data-split-pane]')!;
    const secondary = container.querySelector('[data-pane="secondary"]') as HTMLElement;
    reportSize(host, { height: 800 });

    const handle = screen.getByRole('button', { name: 'Resize reading pane' });
    await fireEvent.mouseDown(handle, { clientY: 500 });
    await fireEvent.mouseMove(window, { clientY: 420 });
    expect(secondary.style.flexBasis).toBe('380px');
    await fireEvent.mouseUp(window, { clientY: 420 });
    expect(localStorage.getItem('archive:reading-pane')).toBe('380');
  });

  it('collapses to the primary pane only, without a handle', () => {
    const { container } = render(SplitPane, {
      props: {
        storageKey: 'archive:reading-pane',
        ariaLabel: 'Resize reading pane',
        orientation: 'vertical',
        collapsed: true
      }
    });

    expect(container.querySelector('[data-pane="secondary"]')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Resize reading pane' })).toBeNull();
  });

  it('caps the vertical pane only at the opposite pane minimum, not an arbitrary maximum', async () => {
    const { container } = render(SplitPane, {
      props: {
        storageKey: 'archive:reading-pane',
        ariaLabel: 'Resize reading pane',
        orientation: 'vertical',
        initialSize: 300,
        minPrimary: 120,
        minSecondary: 160
      }
    });
    const host = container.querySelector('[data-split-pane]')!;
    const secondary = container.querySelector('[data-pane="secondary"]') as HTMLElement;
    reportSize(host, { height: 800 });

    const handle = screen.getByRole('button', { name: 'Resize reading pane' });
    for (let step = 0; step < 30; step += 1) {
      await fireEvent.keyDown(handle, { key: 'ArrowUp' });
    }
    // 800 - 120 (list minimum) - 5 (handle) = 675.
    expect(secondary.style.flexBasis).toBe('675px');

    for (let step = 0; step < 30; step += 1) {
      await fireEvent.keyDown(handle, { key: 'ArrowDown' });
    }
    expect(secondary.style.flexBasis).toBe('160px');
  });
});
