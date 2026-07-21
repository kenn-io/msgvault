import { cleanup } from '@testing-library/svelte';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach } from 'vitest';

const densityCSS = readFileSync(join(process.cwd(), 'src/styles/density.css'), 'utf8');

const compactRowHeight = /--row-height:\s*([^;]+);/.exec(densityCSS)?.[1]?.trim();
if (!compactRowHeight) throw new Error('density.css must define --row-height');

function createMemoryStorage(): Storage {
  const data = new Map<string, string>();
  return {
    get length() {
      return data.size;
    },
    clear() {
      data.clear();
    },
    getItem(key) {
      return data.get(key) ?? null;
    },
    key(index) {
      return Array.from(data.keys())[index] ?? null;
    },
    removeItem(key) {
      data.delete(key);
    },
    setItem(key, value) {
      data.set(key, value);
    }
  };
}

// Node may expose a warning-producing localStorage getter when no backing file
// is configured. Unit tests need deterministic, isolated browser storage.
Object.defineProperty(globalThis, 'localStorage', {
  configurable: true,
  writable: true,
  value: createMemoryStorage()
});

Object.defineProperty(globalThis, 'sessionStorage', {
  configurable: true,
  writable: true,
  value: createMemoryStorage()
});

class TestResizeObserver implements ResizeObserver {
  readonly root = null;
  readonly rootMargin = '0px';
  readonly thresholds = [];
  disconnect(): void {}
  observe(): void {}
  unobserve(): void {}
  takeRecords(): ResizeObserverEntry[] {
    return [];
  }
}

Object.defineProperty(globalThis, 'ResizeObserver', {
  configurable: true,
  writable: true,
  value: TestResizeObserver
});

Object.defineProperty(Element.prototype, 'scrollIntoView', {
  configurable: true,
  writable: true,
  value() {}
});

beforeEach(() => {
  document.documentElement.dataset.density = 'compact';
  document.documentElement.style.setProperty('--row-height', compactRowHeight);
});

afterEach(() => {
  cleanup();
  document.documentElement.style.removeProperty('--row-height');
  localStorage.clear();
  sessionStorage.clear();
});
