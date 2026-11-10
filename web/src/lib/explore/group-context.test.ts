import { describe, expect, it, vi } from 'vitest';

import { filtersForGroup, parseGroupSelection } from './group-context';

describe('group context', () => {
  it.each([
    ['source', '7'],
    ['kind', 'meeting'],
    ['message_type', 'email'],
    ['mailing_list', '<dev_1@example.test>']
  ] as const)('constrains %s details by replacing the existing filter', (dimension, key) => {
    expect(filtersForGroup(
      [{ dimension: 'deletion', values: ['active'] }, { dimension, values: ['old'] }],
      dimension,
      key
    )).toEqual([
      { dimension: 'deletion', values: ['active'] },
      { dimension, values: [key] }
    ]);
  });

  it.each([
    ['participant', '42'],
    ['domain', 'example.com']
  ] as const)('appends a %s drill-down instead of replacing the existing filter', (dimension, key) => {
    // Drilling into a co-participant/domain group (B) under an existing
    // filter (A) must intersect (A∩B), not widen the scope by discarding A.
    expect(filtersForGroup(
      [{ dimension: 'deletion', values: ['active'] }, { dimension, values: ['old'] }],
      dimension,
      key
    )).toEqual([
      { dimension: 'deletion', values: ['active'] },
      { dimension, values: ['old'] },
      { dimension, values: [key] }
    ]);
  });

  it.each([
    ['participant', '42'],
    ['domain', 'example.com']
  ] as const)('does not duplicate an identical %s group filter already present', (dimension, key) => {
    expect(filtersForGroup(
      [{ dimension: 'deletion', values: ['active'] }, { dimension, values: [key] }],
      dimension,
      key
    )).toEqual([
      { dimension: 'deletion', values: ['active'] },
      { dimension, values: [key] }
    ]);
  });

  it('constrains year and month details at local midnight', () => {
    vi.stubEnv('TZ', 'America/New_York');
    try {
      expect(filtersForGroup(
        [{ dimension: 'domain', values: ['example.com'] }, { dimension: 'after', values: ['old'] }],
        'year',
        '2026'
      )).toEqual([
        { dimension: 'domain', values: ['example.com'] },
        { dimension: 'after', values: ['2026-01-01T05:00:00.000Z'] },
        { dimension: 'before', values: ['2027-01-01T05:00:00.000Z'] }
      ]);
      expect(filtersForGroup([], 'month', '2026-12')).toEqual([
        { dimension: 'after', values: ['2026-12-01T05:00:00.000Z'] },
        { dimension: 'before', values: ['2027-01-01T05:00:00.000Z'] }
      ]);
    } finally { vi.unstubAllEnvs(); }
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
      { dimension: 'after', values: ['2026-07-03T00:00:00Z'] }
    ], 'month', '2026-06')).toBeUndefined();
  });

  it('rejects malformed temporal keys and parses only requestable group selections', () => {
    expect(filtersForGroup([], 'year', '20x6')).toBeUndefined();
    expect(filtersForGroup([], 'month', '2026-13')).toBeUndefined();
    expect(parseGroupSelection('group:domain:example.com')).toEqual({ dimension: 'domain', key: 'example.com' });
    expect(parseGroupSelection('group:kind:message')).toEqual({ dimension: 'kind', key: 'message' });
    expect(parseGroupSelection('message:1')).toBeUndefined();
  });
});
