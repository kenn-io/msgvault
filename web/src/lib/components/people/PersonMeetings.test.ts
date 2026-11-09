import { act, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import { createAPIClient } from '../../api/client';
import { meetingActions, meetingMetrics } from '../../meetings/fixtures.test-support';
import PersonMeetings from './PersonMeetings.svelte';

it('keeps first-load Meetings in one section until both reads settle, then collapses an empty result', async () => {
  let releaseActions!: (response: Response) => void;
  let releaseMetrics!: (response: Response) => void;
  const actions = new Promise<Response>(resolve => { releaseActions = resolve; });
  const metrics = new Promise<Response>(resolve => { releaseMetrics = resolve; });
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/meetings/actions') return actions;
    if (path === '/api/v1/meetings/metrics') return metrics;
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  const view = render(PersonMeetings, { client, personID: 7 });
  try {
    expect(await screen.findByText('Loading meetings…')).toBeDefined();
    expect(screen.getAllByRole('heading', { name: 'Meetings' })).toHaveLength(1);
    expect(screen.queryByRole('region', { name: 'Meeting activity' })).toBeNull();
    await act(async () => { releaseActions(Response.json(meetingActions({ coverage: { meeting_count: 0, available: 0, partial: 0, unsupported: 0, unavailable: 0 } }))); });
    expect(screen.getByText('Loading meetings…')).toBeDefined();
    expect(screen.queryByText('No meetings yet')).toBeNull();
    await act(async () => { releaseMetrics(Response.json(meetingMetrics({ totals: { meeting_count: 0, known_duration_count: 0, unknown_duration_count: 0, total_known_seconds: 0, average_known_seconds: null }, first_meeting_at: null, last_meeting_at: null, duration_by_basis: [], months: [] }))); });
    await waitFor(() => expect(screen.getByText('No meetings yet')).toBeDefined());
    expect(screen.getAllByRole('heading', { name: 'Meetings' })).toHaveLength(1);
    expect(screen.queryByText('Loading meetings…')).toBeNull();
    expect(screen.queryByRole('region', { name: 'Meeting activity' })).toBeNull();
  } finally { view.unmount(); }
});
