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
    expect(dateSpan({ year: 2024, month: 1, day: 5 }, { year: 2024, month: 1, day: 6 }, false, { precision: 'day' })).toBe('Jan 5, 2024 – Jan 6, 2024');
    expect(dateSpan({ year: 2024, month: 1, day: 5 }, { year: 2024, month: 1, day: 6 }, false)).toBe('Jan 2024 – Jan 2024');
  });

  it('dates saved conflicts and names imported edits', () => {
    vi.useFakeTimers();
    const DateTimeFormat = Intl.DateTimeFormat;
    const formatter = vi.spyOn(Intl, 'DateTimeFormat').mockImplementation(function (locale, options) {
      return new DateTimeFormat(locale ?? 'en-US', options);
    });
    const toLocaleTimeString = Date.prototype.toLocaleTimeString;
    const time = vi.spyOn(Date.prototype, 'toLocaleTimeString').mockImplementation(function (this: Date, locale, options) {
      return toLocaleTimeString.call(this, locale ?? 'en-US', options);
    });
    try {
      vi.setSystemTime(new Date(2026, 10, 6, 12));
      const today = conflictSaved(new Date(2026, 10, 6, 8, 34).toISOString());
      expect(today.text).toMatch(/^Saved at 8:34/);
      const earlier = conflictSaved(new Date(2026, 10, 5, 8, 34).toISOString(), 'carddav_import');
      expect(earlier.text).toMatch(/^Saved from CardDAV contacts on Nov 5 at 8:34/);
      expect(earlier.title).toContain('2026');
    } finally { time.mockRestore(); formatter.mockRestore(); vi.useRealTimers(); }
  });
});
