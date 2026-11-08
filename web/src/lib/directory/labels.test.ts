import { describe, expect, it, vi } from 'vitest';
import { conflictSaved, dateSpan, channelLabel, contactStateLabel, formatContactDate, formatDay } from './labels';

describe('directory labels', () => {
  it('names known codes and sentence-cases unknown ones', () => {
    expect(contactStateLabel('active')).toBe('Active');
    expect(contactStateLabel('needs_follow_up')).toBe('Needs follow up');
    expect(channelLabel('email')).toBe('Email');
    expect(channelLabel('carrier_pigeon')).toBe('Carrier pigeon');
  });
  it('formats dates for display', () => {
    expect(formatDay('2024-01-05')).toBe('Jan 5, 2024');
    expect(formatContactDate('not a date')).toBe('not a date');
    expect(formatContactDate('2002-01-02T12:00:00Z')).toMatch(/2002/);
  });
});

describe('work and connection labels', () => {
  it('reads current and ended periods, including an open start', () => {
    expect(dateSpan({ year: 2024, month: 3 }, undefined, true)).toBe('Mar 2024 – present');
    expect(dateSpan({ year: 2020 }, { year: 2023 }, false)).toBe('2020 – 2023');
    expect(dateSpan(undefined, { year: 2023 }, false, { later: true })).toBe('until 2023');
    expect(dateSpan({ year: 2019 }, undefined, false)).toBe('From 2019');
    expect(dateSpan(undefined, undefined, true)).toBe('');
  });

  it('dates saved conflicts and names imported edits', () => {
    vi.useFakeTimers();
    try {
      vi.setSystemTime(new Date(2026, 10, 6, 12));
      const today = conflictSaved(new Date(2026, 10, 6, 8, 34).toISOString());
      expect(today.text).toMatch(/^Saved at 8:34/);
      const earlier = conflictSaved(new Date(2026, 10, 5, 8, 34).toISOString(), 'carddav_import');
      expect(earlier.text).toMatch(/^Saved from CardDAV contacts on Nov 5 at 8:34/);
      expect(earlier.title).toContain('2026');
      expect(conflictSaved('invalid').text).toBe('Saved');
      expect(conflictSaved('2024-01-05T12:00:00Z', 'from_custom').text).toMatch(/^Saved by From custom on /);
    } finally { vi.useRealTimers(); }
  });
});
