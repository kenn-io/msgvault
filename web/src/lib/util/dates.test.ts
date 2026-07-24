import { describe, expect, it } from 'vitest';

import { compactDate } from './dates';

const now = new Date('2026-07-19T12:00:00Z');

describe('compactDate', () => {
  it('formats sub-hour ages in minutes with a floor of one minute', () => {
    expect(compactDate('2026-07-19T11:59:59Z', now)).toBe('1m');
    expect(compactDate('2026-07-19T11:35:00Z', now)).toBe('25m');
  });

  it('formats same-day ages in hours', () => {
    expect(compactDate('2026-07-19T09:00:00Z', now)).toBe('3h');
    expect(compactDate('2026-07-18T13:00:00Z', now)).toBe('23h');
  });

  it('formats the past week in days', () => {
    expect(compactDate('2026-07-17T12:00:00Z', now)).toBe('2d');
    expect(compactDate('2026-07-12T12:00:01Z', now)).toBe('6d');
  });

  // Day labels are rendered in the local timezone, so these assert the
  // month + day-number shape (the day can shift ±1 across timezones)
  // rather than one exact day.
  it('elides the year for older dates within the current year', () => {
    expect(compactDate('2026-06-15T12:00:00Z', now)).toMatch(/^Jun 1[456]$/);
    expect(compactDate('2026-01-15T12:00:00Z', now)).toMatch(/^Jan 1[456]$/);
  });

  it('collapses prior years to just the year', () => {
    expect(compactDate('2024-11-05T12:00:00Z', now)).toBe('2024');
    expect(compactDate('1999-06-15T12:00:00Z', now)).toBe('1999');
  });

  it('passes unparseable input through unchanged', () => {
    expect(compactDate('not-a-date', now)).toBe('not-a-date');
    expect(compactDate('', now)).toBe('');
  });

  it('renders slightly-future timestamps (clock skew) as a short date, never a negative age', () => {
    expect(compactDate('2026-07-19T12:05:00Z', now)).toMatch(/^Jul (18|19|20)$/);
  });
});
