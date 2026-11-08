import { readablePartialDate, spokenDate, dateNote, DATE_HINT } from './dates';
import { describe, expect, it } from 'vitest';
import { intervalDateError, isCalendarDate, isTextProfileDate, profileDateError } from './dates';

describe('isCalendarDate', () => {
  it.each([
    ['2024-02-29', true], ['2023-02-29', false], ['2026-02-31', false],
    ['2026-13-01', false], ['2026-1-01', false], ['', false], ['last week', false],
  ])('%s → %s', (value, want) => expect(isCalendarDate(value)).toBe(want));
});

describe('intervalDateError', () => {
  it.each(['', '  ', '2019', '2019-04', '2019-04-12', '20190412', '2024-02-29'])('accepts %j', (value) => {
    expect(intervalDateError(value)).toBeNull();
  });
  it.each(['--04-12', '--04', '---12', 'spring 2019', '2019-13', '2019-02-30', '0000', '201904', '2019-4'])(
    'rejects %j', (value) => {
      expect(intervalDateError(value)).toBe(
        'Use 2024, 2024-03, or 2024-03-15.');
    });
});

describe('profileDateError', () => {
  it.each(['', '2019', '2019-04', '2019-04-12', '--04-12', '--02-29', '--04', '---12', 'spring 2019'])(
    'accepts %j', (value) => expect(profileDateError(value)).toBeNull());
  it.each(['2024-13', '2023-02-29', '--02-30', '--13', '---32', '0000-01'])('rejects %j', (value) => {
    expect(profileDateError(value)).toBe('This date does not exist. Check the month and day.');
  });
  it('marks free text as a text date only', () => {
    expect(isTextProfileDate('spring 2019')).toBe(true);
    expect(isTextProfileDate('--04-12')).toBe(false);
    expect(isTextProfileDate('2024-13')).toBe(false);
    expect(isTextProfileDate('  ')).toBe(false);
  });
});

describe('date readings', () => {
  it('reads partial dates, full dates, and compact input', () => {
    expect(readablePartialDate('2024')).toBe('2024');
    expect(readablePartialDate({ year: 2024, month: 3 })).toBe('Mar 2024');
    expect(readablePartialDate('2024-03-05')).toBe('Mar 5, 2024');
    expect(readablePartialDate('20240305')).toBe('Mar 5, 2024');
    const today = new Date(2026, 10, 6);
    expect(spokenDate('2026-11-06', today)).toBe('Today, Nov 6');
    expect(spokenDate('20261105', today)).toBe('Nov 5');
    expect(spokenDate('2025-11-06', today)).toBe('Nov 6, 2025');
    expect(spokenDate('2026-02-30', today)).toBe('');
    expect(dateNote('2024-03', true, today)).toBe(DATE_HINT);
    expect(dateNote('2024-03', false, today)).toBe('Mar 2024');
    expect(dateNote('invalid', false, today)).toBe('');
  });
});
