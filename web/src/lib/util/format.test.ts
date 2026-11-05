import { formatDate } from './format';
import { describe, expect, it } from 'vitest';
import { formatBytes, formatDateTime, formatRelativeTime } from './format';

describe('format', () => {
  it.each([[120, '120 B'], [3 * 1024, '3 KB'], [1.5 * 1024 * 1024, '1.5 MB']])('formats %d bytes', (value, want) => {
    expect(formatBytes(value)).toBe(want);
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
});

it('keeps the year in current-year dates and preserves invalid input', () => {
 const year = new Date().getFullYear();
 expect(formatDate(`${year}-05-03T12:00:00Z`)).toContain(String(year));
 expect(formatDate('invalid')).toBe('invalid');
});
