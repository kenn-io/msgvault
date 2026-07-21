import { describe, expect, it } from 'vitest';

import { filtersForGroup, parseGroupSelection } from './group-context';

describe('group context', () => {
  it.each([
    ['source', '7'],
    ['participant', '42'],
    ['domain', 'example.com'],
    ['message_type', 'email']
  ] as const)('constrains %s details with the canonical scalar filter', (dimension, key) => {
    expect(filtersForGroup(
      [{ dimension: 'deletion', values: ['active'] }, { dimension, values: ['old'] }],
      dimension,
      key
    )).toEqual([
      { dimension: 'deletion', values: ['active'] },
      { dimension, values: [key] }
    ]);
  });

  it('constrains year and month details with exact half-open UTC intervals', () => {
    expect(filtersForGroup(
      [{ dimension: 'domain', values: ['example.com'] }, { dimension: 'after', values: ['old'] }],
      'year',
      '2026'
    )).toEqual([
      { dimension: 'domain', values: ['example.com'] },
      { dimension: 'after', values: ['2026-01-01T00:00:00Z'] },
      { dimension: 'before', values: ['2027-01-01T00:00:00Z'] }
    ]);
    expect(filtersForGroup([], 'month', '2026-12')).toEqual([
      { dimension: 'after', values: ['2026-12-01T00:00:00Z'] },
      { dimension: 'before', values: ['2027-01-01T00:00:00Z'] }
    ]);
  });

  it('intersects temporal groups with an existing partial interval', () => {
    expect(filtersForGroup([
      { dimension: 'after', values: ['2026-06-15T00:00:00Z'] },
      { dimension: 'before', values: ['2026-06-20T00:00:00Z'] }
    ], 'month', '2026-06')).toEqual([
      { dimension: 'after', values: ['2026-06-15T00:00:00Z'] },
      { dimension: 'before', values: ['2026-06-20T00:00:00Z'] }
    ]);
    expect(filtersForGroup([
      { dimension: 'domain', values: ['example.com'] },
      { dimension: 'after', values: ['2026-07-01T00:00:00Z'] }
    ], 'month', '2026-06')).toBeUndefined();
  });

  it('rejects malformed temporal keys and parses only requestable group selections', () => {
    expect(filtersForGroup([], 'year', '20x6')).toBeUndefined();
    expect(filtersForGroup([], 'month', '2026-13')).toBeUndefined();
    expect(parseGroupSelection('group:domain:example.com')).toEqual({ dimension: 'domain', key: 'example.com' });
    expect(parseGroupSelection('group:kind:email')).toBeUndefined();
    expect(parseGroupSelection('message:1')).toBeUndefined();
  });
});
