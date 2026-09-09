import { describe, expect, it } from 'vitest';

import corpus from './cron-corpus.json';
import { CRON_PRESETS, describeCron, parseCron, scheduleSummary } from './cron';

describe('parseCron', () => {
  it('labels each token with its field and position', () => {
    const parsed = parseCron('  */15 0-6 1,15 JAN mon-fri');
    expect(parsed.error).toBeUndefined();
    expect(parsed.tokens.map((token) => [token.field, token.text, token.start])).toEqual([
      ['minute', '*/15', 2],
      ['hour', '0-6', 7],
      ['day', '1,15', 11],
      ['month', 'JAN', 16],
      ['weekday', 'mon-fri', 20],
    ]);
    expect(parsed.fields?.[3].terms).toEqual([{ start: 1, end: 1, step: 1, all: false }]);
    expect(parsed.fields?.[4].terms).toEqual([{ start: 1, end: 5, step: 1, all: false }]);
  });

  it('treats N/step as N to the end of the field, like the daemon', () => {
    const parsed = parseCron('5/20 * * * *');
    expect(parsed.fields?.[0].terms).toEqual([{ start: 5, end: 59, step: 20, all: false }]);
  });

  it.each([
    ['', 'Enter five fields: minute, hour, day, month, and weekday.'],
    ['0 3', 'Missing day, month, and weekday.'],
    ['0 3 * * * *', 'Only five fields are allowed.'],
    ['60 * * * *', 'Minute: 60 is above the maximum of 59.'],
    ['0 25 * * *', 'Hour: 25 is above the maximum of 23.'],
    ['0 0 0 * *', 'Day: 0 is below the minimum of 1.'],
    ['0 0 * 13 *', 'Month: 13 is above the maximum of 12.'],
    ['0 0 * * 7', 'Weekday: 7 is above the maximum of 6.'],
    ['0 0 * * funday', 'Weekday: "funday" is not a weekday.'],
    ['10-5 * * * *', 'Minute: 10 comes after 5.'],
    ['*/0 * * * *', 'Minute: step in */0 must be at least 1.'],
    ['1-2-3 * * * *', 'Minute: too many hyphens in 1-2-3.'],
    ['1/2/3 * * * *', 'Minute: too many slashes in 1/2/3.'],
    [', * * * *', 'Minute: a value is missing.'],
  ])('rejects %j with a plain message', (expression, message) => {
    const parsed = parseCron(expression);
    expect(parsed.fields).toBeUndefined();
    expect(parsed.error).toBe(message);
  });

  it('reads lists the way the daemon does', () => {
    expect(parseCron('0,,30, * * * *').fields?.[0].terms.map((term) => term.start)).toEqual([0, 30]);
    expect(parseCron('*-5 * * * *').fields?.[0]).toEqual({
      name: 'minute',
      terms: [{ start: 0, end: 59, step: 1, all: true }],
      any: true,
    });
  });

  it('agrees with the daemon parser on the shared corpus', () => {
    for (const expression of corpus.valid) {
      expect(parseCron(expression).error, expression).toBeUndefined();
    }
    for (const expression of corpus.invalid) {
      expect(parseCron(expression).fields, expression).toBeUndefined();
    }
  });

  it('marks only the broken token', () => {
    const parsed = parseCron('0 3 * * 9');
    expect(parsed.tokens.map((token) => token.error !== undefined)).toEqual([false, false, false, false, true]);
  });
});

describe('describeCron', () => {
  it.each([
    ['* * * * *', 'Every minute'],
    ['*/15 * * * *', 'Every 15 minutes'],
    ['*/5 9-17 * * *', 'Every 5 minutes from 09:00 to 17:59'],
    ['* 3 * * *', 'Every minute during the 03:00 hour'],
    ['0 * * * *', 'At :00 past every hour'],
    ['30 */2 * * *', 'At :30 past every other hour'],
    ['30 */6 * * *', 'At :30 past every 6th hour'],
    ['0 3 * * *', 'At 03:00 every day'],
    ['0 3,15 * * *', 'At 03:00 and 15:00 every day'],
    ['0,30 9 * * *', 'At 09:00 and 09:30 every day'],
    ['0 9 * * 1-5', 'At 09:00 on weekdays'],
    ['0 9 * * mon-thu', 'At 09:00 Monday to Thursday'],
    ['0 2 * * 0', 'At 02:00 on Sundays'],
    ['0 2 * * 0,6', 'At 02:00 on weekends'],
    ['0 2 * * 1,3,5', 'At 02:00 on Monday, Wednesday, and Friday'],
    ['0 4 1 * *', 'At 04:00 on the 1st of the month'],
    ['0 4 1,15 * *', 'At 04:00 on the 1st and 15th of the month'],
    ['0 4 */2 * *', 'At 04:00 on odd days of the month'],
    ['0 4 1 jan *', 'At 04:00 on the 1st of the month in January'],
    ['0 4 * 6-8 *', 'At 04:00 every day June to August'],
    ['0 4 1 * 1', 'At 04:00 on the 1st of the month or on Mondays'],
    ['0 4 * */3 *', 'At 04:00 every day every 3rd month'],
    ['0 0-23/6 * * *', 'At 00:00, 06:00, 12:00, and 18:00 every day'],
    ['*/40 * * * *', 'At :00 and :40 past every hour'],
    ['30 */5 * * *', 'At 00:30, 05:30, 10:30, 15:30, and 20:30 every day'],
    ['*/20 * * * *', 'Every 20 minutes'],
    ['0 4 * * */3', 'At 04:00 on Sunday, Wednesday, and Saturday'],
  ])('describes %s as %s', (expression, description) => {
    expect(describeCron(expression)).toBe(description);
  });

  it('describes every preset without falling back to raw syntax', () => {
    for (const preset of CRON_PRESETS) {
      const description = describeCron(preset.expression);
      expect(description).not.toContain('*');
      expect(description).not.toContain('/');
    }
  });

  it('returns the parse error for invalid input', () => {
    expect(describeCron('0 3 * *')).toBe('Missing weekday.');
  });
});

describe('scheduleSummary', () => {
  it('describes a stored schedule and falls back to the raw expression', () => {
    expect(scheduleSummary('0 2 * * *')).toBe('At 02:00 every day');
    expect(scheduleSummary('0 2 * * L')).toBe('0 2 * * L');
    expect(scheduleSummary('')).toBe('');
    expect(scheduleSummary(undefined)).toBe('');
  });
});
