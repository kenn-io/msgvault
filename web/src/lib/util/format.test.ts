import { formatDate } from './format';
import { describe, expect, it, vi } from 'vitest';
import { formatBytes, formatDateTime, formatRelativeTime, formatShortDate, formatShortDateTime } from './format';

describe('format', () => {
  it.each([[120, '120 B'], [3 * 1024, '3 KB'], [1.5 * 1024 * 1024, '1.5 MB']])('formats %d bytes', (value, want) => {
    expect(formatBytes(value)).toBe(want);
  });
  it('formats short dates and date times with the year only for earlier years', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-11-17T12:00:00Z'));
    try {
      const current = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric' }).format(new Date('2026-07-18T12:00:00'));
      const earlier = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', year: 'numeric' }).format(new Date('2025-07-18T12:00:00'));
      expect(formatShortDate('2026-07-18T12:00:00')).toBe(current);
      expect(formatShortDate('2025-07-18T12:00:00')).toBe(earlier);
      const time = new Intl.DateTimeFormat(undefined, { timeStyle: 'short' }).format(new Date('2026-07-18T12:00:00'));
      expect(formatShortDateTime('2026-07-18T12:00:00')).toBe(`${current}, ${time}`);
    } finally { vi.useRealTimers(); }
  });
  it('names missing and unparseable times', () => {
    expect(formatDateTime(undefined)).toBe('Not available');
    expect(formatDateTime('not a time')).toBe('Not available');
    expect(formatDateTime('2026-07-19T10:00:00Z')).toMatch(/2026/);
  });
  it('states time relative to now', () => {
    const now = new Date('2026-07-19T10:00:00Z');
    expect(formatRelativeTime('2026-07-19T12:00:00Z', now)).toBe('in 2 hours');
    expect(formatRelativeTime('2026-07-19T10:05:00Z', now)).toBe('in 5 minutes');
    expect(formatRelativeTime('2026-07-19T09:57:00Z', now)).toBe('3 minutes ago');
    expect(formatRelativeTime('2026-07-19T10:00:00Z', now)).toBe('now');
    expect(formatRelativeTime('soon', now)).toBe('soon');
  });

  it('preserves invalid date input', () => {
    expect(formatDate('invalid')).toBe('invalid');
  });
});
