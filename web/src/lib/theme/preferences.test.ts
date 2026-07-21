import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  createAppearancePreferences,
  rebaseVirtualScroll,
  tableViewportHeight
} from './preferences.svelte';

describe('appearance preferences', () => {
  afterEach(() => {
    document.documentElement.className = '';
    document.documentElement.removeAttribute('data-theme');
    document.documentElement.removeAttribute('data-density');
    sessionStorage.clear();
    vi.unstubAllGlobals();
  });

  it('applies daemon settings as authoritative defaults', () => {
    const preferences = createAppearancePreferences({ theme: 'dark', density: 'comfortable' });

    expect(preferences.current).toEqual({ theme: 'dark', density: 'comfortable', overridden: false });
    expect(document.documentElement.dataset.theme).toBe('dark');
    expect(document.documentElement.dataset.density).toBe('comfortable');
    expect(document.documentElement.classList.contains('dark')).toBe(true);
  });

  it('keeps temporary overrides session-scoped and leaves daemon defaults unchanged', () => {
    const preferences = createAppearancePreferences({ theme: 'light', density: 'compact' });
    preferences.setTemporary({ theme: 'dark', density: 'comfortable' });

    expect(preferences.defaults).toEqual({ theme: 'light', density: 'compact' });
    expect(preferences.current).toEqual({ theme: 'dark', density: 'comfortable', overridden: true });
    expect(sessionStorage.getItem('msgvault.appearance.override')).toBe(JSON.stringify({
      theme: 'dark', density: 'comfortable'
    }));
    expect(localStorage.getItem('msgvault.appearance.override')).toBeNull();

    const restored = createAppearancePreferences({ theme: 'system', density: 'compact' });
    expect(restored.current).toEqual({ theme: 'dark', density: 'comfortable', overridden: true });
    restored.clearTemporary();
    expect(restored.current).toEqual({ theme: 'system', density: 'compact', overridden: false });
  });

  it('tracks system dark mode while system is effective and stops after disposal', () => {
    let listener: ((event: MediaQueryListEvent) => void) | undefined;
    const media = {
      matches: true,
      addEventListener: vi.fn((_name: string, handler: (event: MediaQueryListEvent) => void) => { listener = handler; }),
      removeEventListener: vi.fn()
    } as unknown as MediaQueryList;
    vi.stubGlobal('matchMedia', vi.fn(() => media));
    const preferences = createAppearancePreferences({ theme: 'system', density: 'compact' });
    expect(document.documentElement.classList.contains('dark')).toBe(true);

    Object.defineProperty(media, 'matches', { value: false });
    listener?.({ matches: false } as MediaQueryListEvent);
    expect(document.documentElement.classList.contains('dark')).toBe(false);

    preferences.destroy();
    expect(media.removeEventListener).toHaveBeenCalledWith('change', expect.any(Function));
  });

  it.each([
    [{ theme: 'sepia', density: 'compact' }, { density: 'compact' }],
    [{ theme: 'dark', density: 'spacious' }, { theme: 'dark' }],
    [{ theme: 'sepia', density: 'spacious' }, {}]
  ])('removes invalid parsed session values %#', (stored, retained) => {
    sessionStorage.setItem('msgvault.appearance.override', JSON.stringify(stored));

    const preferences = createAppearancePreferences({ theme: 'system', density: 'comfortable' });

    expect(preferences.temporary).toEqual(retained);
    if (Object.keys(retained).length === 0) {
      expect(sessionStorage.getItem('msgvault.appearance.override')).toBeNull();
    } else {
      expect(sessionStorage.getItem('msgvault.appearance.override')).toBe(JSON.stringify(retained));
    }
  });

  it('derives row geometry from the computed CSS token', async () => {
    document.documentElement.style.removeProperty('--row-height');
    const style = document.createElement('style');
    style.textContent = `
      :root[data-density="compact"] { --row-height: 36px; }
      :root[data-density="comfortable"] { --row-height: 46px; }
    `;
    document.head.append(style);
    document.documentElement.dataset.density = 'compact';
    const { RowGeometry } = await import('./preferences.svelte');
    const geometry = new RowGeometry();
    expect(geometry.height).toBe(36);

    document.documentElement.dataset.density = 'comfortable';
    await new Promise((resolve) => setTimeout(resolve));
    expect(geometry.height).toBe(46);
    geometry.destroy();
    style.remove();
  });

  it('has no geometry before CSS exists and becomes ready from a later computed token', async () => {
    document.documentElement.style.removeProperty('--row-height');
    document.documentElement.removeAttribute('data-density');
    const frames = new Map<number, FrameRequestCallback>();
    let nextFrame = 1;
    vi.stubGlobal('requestAnimationFrame', vi.fn((callback: FrameRequestCallback) => {
      const frame = nextFrame++;
      frames.set(frame, callback);
      return frame;
    }));
    vi.stubGlobal('cancelAnimationFrame', vi.fn((frame: number) => frames.delete(frame)));
    const { RowGeometry } = await import('./preferences.svelte');
    const geometry = new RowGeometry();
    expect(geometry.height).toBeUndefined();
    expect(frames.size).toBe(1);

    const style = document.createElement('style');
    style.textContent = ':root { --row-height: 37px; }';
    document.head.append(style);
    const [frame, callback] = [...frames.entries()][0]!;
    frames.delete(frame);
    callback(performance.now());

    expect(geometry.height).toBe(37);
    expect(frames.size).toBe(0);
    geometry.destroy();
    style.remove();
  });

  it('stops probing when CSS readiness remains unavailable', async () => {
    document.documentElement.style.removeProperty('--row-height');
    document.documentElement.removeAttribute('data-density');
    const frames = new Map<number, FrameRequestCallback>();
    let nextFrame = 1;
    vi.stubGlobal('requestAnimationFrame', vi.fn((callback: FrameRequestCallback) => {
      const frame = nextFrame++;
      frames.set(frame, callback);
      return frame;
    }));
    vi.stubGlobal('cancelAnimationFrame', vi.fn((frame: number) => frames.delete(frame)));
    const { RowGeometry } = await import('./preferences.svelte');
    const geometry = new RowGeometry();

    for (let attempt = 0; attempt < 300; attempt += 1) {
      expect(frames.size).toBe(1);
      const [frame, callback] = [...frames.entries()][0]!;
      frames.delete(frame);
      callback(performance.now());
    }

    expect(geometry.height).toBeUndefined();
    expect(frames.size).toBe(0);
    geometry.destroy();
  });

  it('cancels an unfinished CSS readiness probe on disposal', async () => {
    document.documentElement.style.removeProperty('--row-height');
    document.documentElement.removeAttribute('data-density');
    vi.stubGlobal('requestAnimationFrame', vi.fn(() => 42));
    const cancelAnimationFrame = vi.fn();
    vi.stubGlobal('cancelAnimationFrame', cancelAnimationFrame);
    const { RowGeometry } = await import('./preferences.svelte');
    const geometry = new RowGeometry();

    geometry.destroy();

    expect(cancelAnimationFrame).toHaveBeenCalledWith(42);
  });

  it('rebases a virtual scroll offset around the same visible row', () => {
    expect(rebaseVirtualScroll(249 * 36 + 5, 36, 46)).toBe(249 * 46 + 5);
  });

  it('excludes the sticky table header from the virtualized row viewport', () => {
    expect(tableViewportHeight(360, 30, 800)).toBe(330);
    expect(tableViewportHeight(900, 34, 640)).toBe(606);
    expect(tableViewportHeight(20, 30, 800)).toBe(0);
  });
});
