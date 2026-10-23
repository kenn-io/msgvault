import { cleanup } from '@testing-library/svelte';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach } from 'vitest';

const densityCSS = readFileSync(join(process.cwd(), 'src/styles/density.css'), 'utf8');

const compactRowHeight = /--row-height:\s*([^;]+);/.exec(densityCSS)?.[1]?.trim();
if (!compactRowHeight) throw new Error('density.css must define --row-height');

// Browser Storage exposes each key as an enumerable own property. kit-ui's
// telemetry reads claims back with Object.keys, so a Map that only implements
// getItem never reports a claim and the reporter posts again until the heap dies.
function createMemoryStorage(): Storage {
  const data = new Map<string, string>();
  const storage = Object.create(null) as Storage;
  const forget = (key: string) => {
    data.delete(key);
    delete storage[key as keyof Storage];
  };
  Object.defineProperties(storage, {
    length: {
      enumerable: false,
      get() {
        return data.size;
      }
    },
    clear: {
      configurable: true,
      enumerable: false,
      writable: true,
      value() {
        for (const key of [...data.keys()]) forget(key);
      }
    },
    getItem: {
      configurable: true,
      enumerable: false,
      writable: true,
      value(key: string) {
        return data.get(key) ?? null;
      }
    },
    key: {
      configurable: true,
      enumerable: false,
      writable: true,
      value(index: number) {
        return Array.from(data.keys())[index] ?? null;
      }
    },
    removeItem: {
      configurable: true,
      enumerable: false,
      writable: true,
      value(key: string) {
        forget(key);
      }
    },
    setItem: {
      configurable: true,
      enumerable: false,
      writable: true,
      value(key: string, value: string) {
        data.set(key, value);
        if (Object.prototype.hasOwnProperty.call(storage, key)) return;
        Object.defineProperty(storage, key, {
          configurable: true,
          enumerable: true,
          get() {
            return data.get(key) ?? null;
          },
          set(next: string) {
            data.set(key, next);
          }
        });
      }
    }
  });
  return storage;
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

// kit-ui's AppSidebar imports Svelte's window.devicePixelRatio binding, which
// calls matchMedia while the module loads. jsdom does not provide it.
if (typeof window.matchMedia !== 'function') {
  Object.defineProperty(window, 'matchMedia', {
    configurable: true,
    writable: true,
    value(query: string): MediaQueryList {
      return {
        matches: false,
        media: query,
        onchange: null,
        addListener() {},
        removeListener() {},
        addEventListener() {},
        removeEventListener() {},
        dispatchEvent() {
          return false;
        }
      };
    }
  });
}

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
