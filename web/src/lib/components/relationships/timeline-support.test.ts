import { describe, expect, it, vi } from 'vitest';

import { localDayBoundsUTC } from './timeline-support';

describe('localDayBoundsUTC', () => {
  it.each(['2018-11-04T00:00:00', '2018-11-04T15:00:00Z'])(
    'ends at the next midnight when the selected day skips midnight: %s', (instant) => {
      vi.stubEnv('TZ', 'America/Sao_Paulo');
      try {
        expect(localDayBoundsUTC(instant)).toEqual({
          start: '2018-11-04T03:00:00.000Z',
          end: '2018-11-05T02:00:00.000Z'
        });
      } finally {
        vi.unstubAllEnvs();
      }
    }
  );

  it('returns the local midnight-to-midnight UTC bounds containing the instant', () => {
    const bounds = localDayBoundsUTC('2026-07-18T08:00:00Z');
    const start = new Date(bounds.start);
    const end = new Date(bounds.end);
    expect(start.getHours()).toBe(0);
    expect(end.getTime() - start.getTime()).toBe(24 * 60 * 60 * 1000);
    expect(new Date('2026-07-18T08:00:00Z').getTime()).toBeGreaterThanOrEqual(start.getTime());
    expect(new Date('2026-07-18T08:00:00Z').getTime()).toBeLessThan(end.getTime());
  });
});
