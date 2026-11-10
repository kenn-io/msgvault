import { describe, expect, it } from 'vitest';
import { channelLabel, contactStateLabel, formatContactDate, formatDay, cadenceLabel } from './labels';

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

it.each([
  [{ stale: false, status: 'unknown' }, 'No list'],
  [{ stale: false, status: 'unknown', frequency_days: 17 }, 'Every 17 days · Due date unknown'],
  [{ stale: false, status: 'overdue', list: 'A', frequency_days: 21, due_at: '2026-07-18T00:00:00Z' }, 'List A · Overdue · Jul 18, 2026'],
  [{ stale: false, status: 'ok', frequency_days: 1, due_at: '2026-07-20T00:00:00Z' }, 'Every 1 day · Due Jul 20, 2026'],
  [{ stale: true, status: 'unknown', list: 'B', frequency_days: 60 }, 'List B · Due date updating'],
  [{ stale: true, status: 'unknown' }, 'No list'],
] as const)('labels cadence truthfully: %j', (cadence, expected) => {
  expect(cadenceLabel(cadence)).toBe(expected);
});
