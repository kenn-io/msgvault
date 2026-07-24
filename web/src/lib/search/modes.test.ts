import { describe, expect, it, vi } from 'vitest';

import {
  SEARCH_MODE_PREFERENCE_KEY,
  VisibleLexicalCountCache,
  loadRememberedSearchMode,
  parseSearchMode,
  rememberSearchMode,
  resolveInitialSearchMode
} from './modes';
import { parseSearchCoverage } from './modes';

function memoryStorage(initial: Record<string, string> = {}) {
  const values = new Map(Object.entries(initial));
  return {
    getItem: vi.fn((key: string) => values.get(key) ?? null),
    setItem: vi.fn((key: string, value: string) => values.set(key, value))
  };
}

describe('search mode preference', () => {
  it('validates the generated search coverage contract at runtime', () => {
    expect(parseSearchCoverage({
      status: 'ready', eligible_count: 2, embedded_count: 2, percentage: 100,
      cache_revision: 'cache-1', actions: ['retry']
    })).toMatchObject({ status: 'ready', actions: ['retry'] });
    expect(parseSearchCoverage({
      status: 'future_status', eligible_count: 2, embedded_count: 2, percentage: 100,
      cache_revision: 'cache-1', actions: []
    })).toBeUndefined();
    expect(parseSearchCoverage({
      status: 'ready', eligible_count: 2, embedded_count: 2, percentage: 100,
      cache_revision: 'cache-1', actions: ['future_action']
    })).toBeUndefined();
    expect(parseSearchCoverage({
      status: 'ready', eligible_count: 1.5, embedded_count: 2, percentage: 100,
      cache_revision: 'cache-1', actions: []
    })).toBeUndefined();
    expect(parseSearchCoverage({
      status: 'ready', eligible_count: 2, embedded_count: 2, percentage: 100,
      cache_revision: 'cache-1', actions: [], vector_generation: 1.25
    })).toBeUndefined();
    expect(parseSearchCoverage({
      status: 'ready', eligible_count: 2, embedded_count: 2, percentage: 100,
      cache_revision: 'cache-1', actions: [], vector_fingerprint: null
    })).toBeUndefined();
    expect(parseSearchCoverage({
      status: 'ready', eligible_count: 2, embedded_count: 2, percentage: 100,
      cache_revision: 'cache-1', actions: [], detail: 42
    })).toBeUndefined();
    expect(parseSearchCoverage({
      status: 'ready', eligible_count: 2, embedded_count: 2, percentage: 100,
      cache_revision: 'cache-1', actions: [], future_server_field: true
    })).toMatchObject({ future_server_field: true });
  });
  it('uses an explicit URL mode before the remembered default', () => {
    const storage = memoryStorage({ [SEARCH_MODE_PREFERENCE_KEY]: 'semantic' });

    expect(resolveInitialSearchMode('hybrid', storage)).toBe('hybrid');
    expect(resolveInitialSearchMode(undefined, storage)).toBe('semantic');
  });

  it('uses the configured default only when URL and saved preference are absent', () => {
    const empty = memoryStorage();
    const saved = memoryStorage({ [SEARCH_MODE_PREFERENCE_KEY]: 'full_text' });

    expect(resolveInitialSearchMode(undefined, empty, 'semantic')).toBe('semantic');
    expect(resolveInitialSearchMode(undefined, empty)).toBe('full_text');
    expect(resolveInitialSearchMode(undefined, saved, 'semantic')).toBe('full_text');
    expect(resolveInitialSearchMode('hybrid', saved, 'semantic')).toBe('hybrid');
  });

  it('rejects unknown configured modes', () => {
    expect(parseSearchMode('semantic')).toBe('semantic');
    expect(parseSearchMode('magic')).toBeUndefined();
    expect(parseSearchMode(undefined)).toBeUndefined();
    expect(resolveInitialSearchMode(undefined, memoryStorage(), parseSearchMode('magic'))).toBe('full_text');
  });

  it('falls back safely and persists explicit user choices', () => {
    const storage = memoryStorage({ [SEARCH_MODE_PREFERENCE_KEY]: 'invalid' });

    expect(loadRememberedSearchMode(storage)).toBeUndefined();
    rememberSearchMode('hybrid', storage);
    expect(storage.setItem).toHaveBeenCalledWith(SEARCH_MODE_PREFERENCE_KEY, 'hybrid');
  });

  it('tolerates browser storage being unavailable', () => {
    const storage = {
      getItem: vi.fn(() => { throw new DOMException('denied', 'SecurityError'); }),
      setItem: vi.fn(() => { throw new DOMException('denied', 'SecurityError'); })
    };

    expect(loadRememberedSearchMode(storage)).toBeUndefined();
    expect(() => rememberSearchMode('semantic', storage)).not.toThrow();
  });
});

describe('VisibleLexicalCountCache', () => {
  it('keys counts by canonical query, archive revision, authority, and row keys', () => {
    const cache = new VisibleLexicalCountCache(4);
    const first = cache.key({
      query: '  alpha   beta ', cacheRevision: 'cache-1', lexicalRevision: 'fts-1', rowKeys: ['b', 'a']
    });
    const equivalent = cache.key({
      query: 'alpha beta', cacheRevision: 'cache-1', lexicalRevision: 'fts-1', rowKeys: ['a', 'b']
    });

    expect(first).toBe(equivalent);
    cache.set(first, { a: 1, b: 2 });
    expect(cache.get(equivalent)).toEqual({ a: 1, b: 2 });
    expect(cache.key({ query: 'alpha beta', cacheRevision: 'cache-2', lexicalRevision: 'fts-1', rowKeys: ['a', 'b'] })).not.toBe(first);
    expect(cache.key({ query: 'alpha beta', cacheRevision: 'cache-1', lexicalRevision: 'fts-2', rowKeys: ['a', 'b'] })).not.toBe(first);
    const firstPredicate = cache.key({
      query: 'alpha beta', cacheRevision: 'cache-1', lexicalRevision: 'fts-1', rowKeys: ['a', 'b'],
      predicateFingerprint: 'domain=example.com'
    } as Parameters<typeof cache.key>[0]);
    const secondPredicate = cache.key({
      query: 'alpha beta', cacheRevision: 'cache-1', lexicalRevision: 'fts-1', rowKeys: ['a', 'b'],
      predicateFingerprint: 'domain=example.org'
    } as Parameters<typeof cache.key>[0]);
    expect(firstPredicate).not.toBe(secondPredicate);
  });

  it('invalidates old revisions and remains bounded', () => {
    const cache = new VisibleLexicalCountCache(2);
    cache.set('one', { a: 1 }, 'cache-1');
    cache.set('two', { b: 2 }, 'cache-1');
    cache.set('three', { c: 3 }, 'cache-1');

    expect(cache.size).toBe(2);
    expect(cache.get('one')).toBeUndefined();
    cache.invalidateRevision('cache-2');
    expect(cache.size).toBe(0);
  });
});
