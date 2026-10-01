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
        'Use a year, year and month, or full date, like 2019, 2019-04, or 2019-04-12.');
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
