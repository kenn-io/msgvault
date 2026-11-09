import { act, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import { createAPIClient } from '../../api/client';
import { meetingActions, meetingMetrics } from '../../meetings/fixtures.test-support';
import PersonMeetings from './PersonMeetings.svelte';

it.each(['empty', 'populated', 'failed'])('keeps the empty Meetings section during a refresh that becomes %s', async outcome => {
  const metrics = meetingMetrics({ totals: { meeting_count: 0, known_duration_count: 0, unknown_duration_count: 0, total_known_seconds: 0, average_known_seconds: null }, first_meeting_at: null, last_meeting_at: null, duration_by_basis: [], months: [] });
  const actions = meetingActions({ coverage: { meeting_count: 0, available: 0, partial: 0, unsupported: 0, unavailable: 0 } });
  let metricsReads = 0;
  let releaseInitial!: (response: Response) => void;
  const initial = new Promise<Response>(resolve => { releaseInitial = resolve; });
  let release!: (response: Response) => void;
  const refresh = new Promise<Response>(resolve => { release = resolve; });
  const client = createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    if (path === '/api/v1/meetings/metrics') return ++metricsReads === 1 ? initial : refresh;
    if (path === '/api/v1/meetings/actions') return Response.json(actions);
    throw new Error(`Unexpected ${path}`);
  });
  const view = render(PersonMeetings, { client, personID: 7, refreshKey: '1' });
  try {
    await waitFor(() => expect(metricsReads).toBe(1));
    expect(screen.queryByRole('region', { name: 'Meeting activity' })).toBeNull();
    await act(() => releaseInitial(Response.json(metrics)));
    await screen.findByText('No meetings yet');
    expect(screen.getByRole('region', { name: 'Meetings' })).toBeDefined();
    expect(screen.queryByRole('region', { name: 'Meeting activity' })).toBeNull();
    await view.rerender({ client, personID: 7, refreshKey: '2' });
    await waitFor(() => expect(metricsReads).toBe(2));
    expect(screen.getByText('No meetings yet')).toBeDefined();
    expect(screen.queryByRole('region', { name: 'Meeting activity' })).toBeNull();
    await act(() => release(outcome === 'failed' ? Response.json({ error: 'unavailable', message: 'Meetings unavailable' }, { status: 503 }) : Response.json(outcome === 'empty' ? metrics : meetingMetrics())));
    if (outcome === 'empty') expect(screen.getByText('No meetings yet')).toBeDefined();
    else {
      await screen.findByRole('region', { name: 'Meeting activity' });
      expect(screen.queryByText('No meetings yet')).toBeNull();
      if (outcome === 'failed') expect(await screen.findByRole('alert')).toHaveProperty('textContent', 'Meetings unavailable');
    }
  } finally { releaseInitial(Response.json(metrics)); release(Response.json(metrics)); view.unmount(); }
});
