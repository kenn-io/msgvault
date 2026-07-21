import { describe, expect, it } from 'vitest';

import { identityHue, initialsFor } from './identity';

describe('identityHue', () => {
  it('is deterministic for the same seed', () => {
    expect(identityHue('cluster:42')).toBe(identityHue('cluster:42'));
  });

  it('stays within [0, 360)', () => {
    for (const seed of ['', 'a', 'cluster:1', 'domain:example.com', '🙂 unicode']) {
      const hue = identityHue(seed);
      expect(hue).toBeGreaterThanOrEqual(0);
      expect(hue).toBeLessThan(360);
      expect(Number.isInteger(hue)).toBe(true);
    }
  });

  it('spreads nearby seeds to different hues', () => {
    const hues = new Set([1, 2, 3, 4, 5, 6, 7, 8].map((n) => identityHue(`cluster:${n}`)));
    expect(hues.size).toBeGreaterThan(4);
  });
});

describe('initialsFor', () => {
  it('takes the first letters of the first two words', () => {
    expect(initialsFor('Alice Example')).toBe('AE');
    expect(initialsFor('  thing   document ')).toBe('TD');
  });

  it('uses a single initial for one-word names', () => {
    expect(initialsFor('Alice')).toBe('A');
  });

  it('derives initials from the local part of an email address', () => {
    expect(initialsFor('jane.doe@example.com')).toBe('JD');
    expect(initialsFor('person19@mail.test')).toBe('P');
  });

  it('skips leading symbols when deriving initials', () => {
    expect(initialsFor('@example.com')).toBe('EC');
  });

  it('falls back for empty and symbol-only labels', () => {
    expect(initialsFor('')).toBe('?');
    expect(initialsFor('   ')).toBe('?');
    expect(initialsFor('++')).toBe('+');
  });
});
