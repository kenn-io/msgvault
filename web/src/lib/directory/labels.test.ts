import { describe, expect, it } from 'vitest';
import { channelLabel, contactStateLabel, formatContactDate, formatDay, reviewStateChip } from './labels';

describe('directory labels', () => {
  it('names known codes and sentence-cases unknown ones', () => {
    expect(contactStateLabel('active')).toBe('Active');
    expect(contactStateLabel('needs_follow_up')).toBe('Needs follow up');
    expect(channelLabel('email')).toBe('Email');
    expect(channelLabel('carrier_pigeon')).toBe('Carrier pigeon');
  });
  it.each([
    ['candidate', 'Candidate', 'warning'], ['conflict', 'Conflict', 'warning'],
    ['pending', 'Pending', 'warning'], ['accepted', 'Accepted', 'success'],
    ['rejected', 'Rejected', 'muted'], ['superseded', 'Superseded', 'neutral'],
  ])('review state %s', (code, label, tone) => {
    expect(reviewStateChip(code)).toEqual({ label, tone });
  });
  it('formats dates for display', () => {
    expect(formatDay('2024-01-05')).toBe('Jan 5, 2024');
    expect(formatContactDate('not a date')).toBe('not a date');
    expect(formatContactDate('2002-01-02T12:00:00Z')).toMatch(/2002/);
  });
});
