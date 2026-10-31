import { render, screen, waitFor } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import DomainCalendarHost from './DomainCalendar.test-support.svelte';

it.each([
  { name: 'last activity', year: 2025, lastAt: '2025-07-18T12:00:00Z' },
  { name: 'future activity', year: 2026, lastAt: '2027-07-18T12:00:00Z' },
])('requests initial local year $year once for $name', async ({ year, lastAt }) => {
  vi.stubEnv('TZ', 'America/New_York');
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(new Date('2026-10-31T12:00:00Z'));
  const bodies: unknown[] = [];
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    bodies.push(await request.json());
    return Response.json({ rows: [{ key: `${year}-07-18`, label: `${year}-07-18`, count: 1, estimated_bytes: 0 }], total_count: 1, cache_revision: 'cache-domain', search_provenance: {} });
  });
  const view = render(DomainCalendarHost, { props: { client, domain: 'example.com', firstAt: '2022-01-01T12:00:00Z', lastAt, selectedDate: null, onDateChange: vi.fn() } });
  try {
    await screen.findAllByRole('button', { name: `1 message on Jul 18, ${year}` });
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toMatchObject({ grouping: ['day'], timezone: 'America/New_York', sent_only: true, limit: 366,
      filters: [{ dimension: 'domain', values: ['example.com'] }, { dimension: 'after', values: [`${year}-01-01T05:00:00.000Z`] }, { dimension: 'before', values: [`${year + 1}-01-01T05:00:00.000Z`] }] });
  } finally { view.unmount(); vi.useRealTimers(); vi.unstubAllEnvs(); }
});
