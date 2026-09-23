import { describe, expect, it } from 'vitest';

import { dayTooltipText } from './calendar-tooltip';

const base = {
  sent: 0, received: 0, email: 0, chat: 0, meetings: 0, total: 0,
  modality_mask: 0, level: 'NONE'
} as const;

describe('dayTooltipText', () => {
  it('counts email plus chat as messages with a readable date', () => {
    expect(dayTooltipText({ ...base, date: '2026-03-12', email: 1, chat: 3, total: 4 }))
      .toBe('4 messages on Mar 12, 2026');
  });

  it('uses the singular and appends meetings', () => {
    expect(dayTooltipText({ ...base, date: '2026-01-02', chat: 1, meetings: 2, total: 3 }))
      .toBe('1 message on Jan 2, 2026, 2 meetings');
  });

  it('names empty days', () => {
    expect(dayTooltipText({ ...base, date: '2026-12-31' }))
      .toBe('No messages on Dec 31, 2026');
  });
});
