import { describe, expect, it } from 'vitest';

import corpus from './cron-corpus.json';
import { CRON_PRESETS, describeCron, joinCron, parseCron, scheduleSummary, splitCron } from './cron';

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
    ['0 0 * * 8', 'Weekday: 8 is above the maximum of 7.'],
    ['0 0 * * funday', 'Weekday: "funday" is not a weekday.'],
    ['*/0 * * * *', 'Minute: step in */0 must be at least 1.'],
    ['*/60 * * * *', 'Minute: step in */60 must be less than 60.'],
    ['55/10 * * * *', 'Minute: step in 55/10 must be less than 5.'],
    ['0 0 * * */8', 'Weekday: step in */8 must be less than 8.'],
    ['1-2-3 * * * *', 'Minute: too many hyphens in 1-2-3.'],
    ['1/2/3 * * * *', 'Minute: too many slashes in 1/2/3.'],
    [', * * * *', 'Minute: a value is missing.'],
    ['CRON_TZ=UTC', 'Enter five fields: minute, hour, day, month, and weekday.'],
    ['CRON_TZ= 0 3 * * *', 'Enter a time zone name after the equals sign.'],
    ['CRON_TZ=Bad*Zone 0 3 * * *', '"Bad*Zone" is not a time zone name.'],
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

  it('reads a time zone prefix and keeps token positions on the whole string', () => {
    const parsed = parseCron('TZ=Europe/Berlin  +5 3 * * *');
    expect(parsed.error).toBeUndefined();
    expect(parsed.zone).toBe('Europe/Berlin');
    expect(parsed.tokens[0]).toMatchObject({ field: 'minute', text: '+5', start: 18, end: 20 });
    expect(parsed.fields?.[0].terms).toEqual([{ start: 5, end: 5, step: 1, all: false }]);
    expect(parseCron('0 3 * * *').zone).toBeUndefined();
  });

  it('leaves zone names to the daemon instead of the browser database', () => {
    const parsed = parseCron('CRON_TZ=US/Eastern 0 3 * * *');
    expect(parsed.error).toBeUndefined();
    expect(parsed.zone).toBe('US/Eastern');
    expect(describeCron('CRON_TZ=Mars/Olympus 0 3 * * *')).toBe('At 03:00 every day, Mars/Olympus time');
  });

  it('rejects a schedule longer than the daemon reads', () => {
    expect(parseCron(`0${',0'.repeat(600)} 3 * * *`).error).toBe('A schedule can be at most 1024 bytes long.');
  });

  it('reads a quoted zone name without its quotes, like the daemon', () => {
    expect(parseCron('TZ="Europe/Berlin" 0 3 * * *').zone).toBe('Europe/Berlin');
    expect(splitCron("TZ='UTC' 0 3 * * *")).toEqual({ zone: 'UTC', expression: '0 3 * * *' });
  });

  it('reads 7 as Sunday and ranges that wrap past the end of a field', () => {
    expect(parseCron('0 9 * * 5-7').fields?.[4].terms).toEqual([{ start: 5, end: 7, step: 1, all: false }]);
    expect(parseCron('0 22-2 * * *').fields?.[1].terms).toEqual([{ start: 22, end: 2, step: 1, all: false }]);
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
    ['0 4 * * */7', 'At 04:00 on Sundays'],
    ['0 2 * * 7', 'At 02:00 on Sundays'],
    ['0 9 * * 5-7', 'At 09:00 Friday to Sunday'],
    ['0 9 * * 5-7/2', 'At 09:00 on Sunday and Friday'],
    ['0 9 * * fri-mon', 'At 09:00 Friday to Monday'],
    ['0 9 * * 0-7', 'At 09:00 every day'],
    ['0 9 1 * 0-7', 'At 09:00 every day'],
    ['0 22-2 * * *', 'At 00:00, 01:00, 02:00, 22:00, and 23:00 every day'],
    ['*/30 22-2 * * *', 'Every 30 minutes from 22:00 to 02:59'],
    ['50-5/10 * * * *', 'At :00 and :50 past every hour'],
    ['0 4 * nov-feb *', 'At 04:00 every day November to February'],
    // The daemon reads an endpoint past the end of the field as the start
    // of the next lap, so these match what it runs.
    ['*/30 24-2 * * *', 'Every 30 minutes from 00:00 to 02:59'],
    ['0 4 * 13-2 *', 'At 04:00 every day January to February'],
    ['0 4 * * 8-1', 'At 04:00 Sunday to Monday'],
    ['0 4 28-0 * *', 'At 04:00 on the 28th, 29th, 30th, and 31st of the month'],
    ['70-5 * * * *', 'Never runs'],
    // When both day fields are restricted the daemon runs on a day matching
    // either one, so an empty range in one leaves the other in charge.
    ['0 4 40-3 * mon', 'At 04:00 on Mondays'],
    ['0 4 1 * 20-1', 'At 04:00 on the 1st of the month'],
    ['0 4 40-3 * 0-7', 'At 04:00 every day'],
    ['0 4 40-3 * *', 'Never runs'],
    ['0 4 40-3 * */1', 'Never runs'],
    ['0 4 * * 20-1', 'Never runs'],
    ['0 4 40-3 * 20-1', 'Never runs'],
    ['CRON_TZ=America/New_York 0 3 * * *', 'At 03:00 every day, America/New York time'],
    ['TZ=UTC */15 * * * *', 'Every 15 minutes, UTC time'],
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

describe('splitCron and joinCron', () => {
  it('round-trips a zone and leaves an empty expression empty', () => {
    expect(splitCron('CRON_TZ=Europe/Berlin 0 3 * * *')).toEqual({ zone: 'Europe/Berlin', expression: '0 3 * * *' });
    expect(splitCron('TZ=UTC  0 3 * * *')).toEqual({ zone: 'UTC', expression: '0 3 * * *' });
    expect(splitCron('0 3 * * *')).toEqual({ zone: '', expression: '0 3 * * *' });
    expect(joinCron('Europe/Berlin', ' 0 3 * * * ')).toBe('CRON_TZ=Europe/Berlin 0 3 * * *');
    expect(joinCron('', ' 0 3 * * * ')).toBe('0 3 * * *');
    expect(joinCron('Europe/Berlin', '   ')).toBe('');
    expect(joinCron('', '   ')).toBe('');
  });
});

describe('scheduleSummary', () => {
  it('describes a stored schedule and falls back to the raw expression', () => {
    expect(scheduleSummary('0 2 * * *')).toBe('At 02:00 every day');
    expect(scheduleSummary('CRON_TZ=Asia/Tokyo 0 2 * * *')).toBe('At 02:00 every day, Asia/Tokyo time');
    expect(scheduleSummary('0 2 * * L')).toBe('0 2 * * L');
    expect(scheduleSummary('')).toBe('');
    expect(scheduleSummary(undefined)).toBe('');
  });
});
