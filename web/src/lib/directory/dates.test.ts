import { describe, expect, it } from 'vitest';
import { DATE_HINT, dateNote, intervalDateError, isCalendarDate, isTextProfileDate, profileDateError, readablePartialDate } from './dates';

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
  it.each([
    ...['--04-12', '--04', '---12', 'spring 2019', '201904', '2019-4'].map(value => [value, '', 'Use 2024, 2024-03, or 2024-03-15.']),
    ...['2019-13', '2019-02-30'].map(value => [value, '', 'This date does not exist. Check the month and day.']),
    ['0000', '', 'Year must be between 0001 and 9999.'],
    ['0000-01', '', 'Year must be between 0001 and 9999.'],
    ['2024-02', '2024-03', 'End date must be on or after start date.'],
  ])('rejects %j with start %j', (value, start, error) => {
    expect(intervalDateError(value, start)).toBe(error);
  });
});

describe('profileDateError', () => {
  it.each(['', '2019', '2019-04', '2019-04-12', '--04-12', '--02-29', '--04', '---12', 'spring 2019'])(
    'accepts %j', (value) => expect(profileDateError(value)).toBeNull());
  it.each(['2024-13', '2023-02-29', '--02-30', '--13', '---32'])('rejects %j', (value) => {
    expect(profileDateError(value)).toBe('This date does not exist. Check the month and day.');
  });
  it.each(['0000', '0000-01'])('identifies the invalid year in %j', (value) => {
    expect(profileDateError(value)).toBe('Year must be between 0001 and 9999.');
  });
  it('marks free text as a text date only', () => {
    expect(isTextProfileDate('spring 2019')).toBe(true);
    expect(isTextProfileDate('--04-12')).toBe(false);
    expect(isTextProfileDate('2024-13')).toBe(false);
    expect(isTextProfileDate('  ')).toBe(false);
  });
});

describe('date readings', () => {
  it('reads dates', () => {
    expect(readablePartialDate('2024')).toBe('2024');
    expect(readablePartialDate({ year: 2024, month: 3 })).toBe('Mar 2024');
    expect(readablePartialDate('2024-03-05')).toBe('Mar 5, 2024');
    expect(readablePartialDate('20240305')).toBe('Mar 5, 2024');
    const today = new Date(2026, 10, 6);
    expect(readablePartialDate('2026-03-05', today)).toBe('Mar 5');
    expect(readablePartialDate('2026-03', today)).toBe('Mar 2026');
    expect(dateNote('2026-11-06', false, today)).toBe('Today, Nov 6');
    expect(dateNote('20261105', false, today)).toBe('Nov 5');
    expect(dateNote('2025-11-06', false, today)).toBe('Nov 6, 2025');
    expect(dateNote('2026-02-30', false, today)).toBe('');
    expect(dateNote('2024-03', true, today)).toBe(DATE_HINT);
    expect(dateNote('2024-03', false, today)).toBe('Mar 2024');
    expect(dateNote('2024', false, today)).toBe('');
    expect(dateNote('2024', true, today)).toBe(DATE_HINT);
    expect(dateNote('invalid', false, today)).toBe('');
  });
});
