import { act, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import { createAPIClient } from '../../api/client';
import { meetingActions, meetingAction, meetingMetrics } from '../../meetings/fixtures.test-support';
import PersonMeetings from './PersonMeetings.svelte';

it('scopes Meetings to the saved person, waits for both empty reads, and refreshes actions with profile revisions', async () => {
  let releaseActions!: (response: Response) => void;
  let releaseMetrics!: (response: Response) => void;
  const actions = new Promise<Response>(resolve => { releaseActions = resolve; });
  const metrics = new Promise<Response>(resolve => { releaseMetrics = resolve; });
  const meetingRequests: Request[] = [];
  const options = { meetingRequests, meetingResponses: { actions, metrics } };
  const client = createAPIClient(async input => { const request = input instanceof Request ? input : new Request(input); meetingRequests.push(request.clone()); return new URL(request.url).pathname.endsWith('/actions') ? options.meetingResponses.actions : options.meetingResponses.metrics; });
  const view = render(PersonMeetings, { client, personID: 7, refreshKey: '1' });
  expect(await screen.findByText('Loading meetings…')).toBeDefined();
  await waitFor(() => expect(meetingRequests.find(request => new URL(request.url).pathname.endsWith('/actions'))).toBeDefined());
  await expect(meetingRequests.find(request => new URL(request.url).pathname.endsWith('/actions'))!.json()).resolves.toMatchObject({ scope: { person_id: 7 } });
  expect(screen.queryByRole('region', { name: 'Meeting activity' })).toBeNull();
  await act(async () => { releaseActions(Response.json(meetingActions({ coverage: { meeting_count: 0, available: 0, partial: 0, unsupported: 0, unavailable: 0 } }))); });
  expect(screen.getByText('Loading meetings…')).toBeDefined();
  expect(screen.queryByText('No meetings yet')).toBeNull();
  await act(async () => { releaseMetrics(Response.json(meetingMetrics({ totals: { meeting_count: 0, known_duration_count: 0, unknown_duration_count: 0, total_known_seconds: 0, average_known_seconds: null }, first_meeting_at: null, last_meeting_at: null, duration_by_basis: [], months: [] }))); });
  expect(await screen.findByText('No meetings yet')).toBeDefined();
  expect(screen.queryByText('Loading meetings…')).toBeNull();
  expect(screen.queryByRole('region', { name: 'Meeting activity' })).toBeNull();
  options.meetingResponses = {
    actions: new Promise<Response>(resolve => { releaseActions = resolve; }),
    metrics: new Promise<Response>(resolve => { releaseMetrics = resolve; })
  };
  await view.rerender({ client, personID: 7, refreshKey: '2' });
  expect(await screen.findByText('Loading meetings…')).toBeDefined();
  expect(screen.queryByText('No meetings yet')).toBeNull();
  await act(async () => {
    releaseActions(Response.json(meetingActions({ rows: [meetingAction({ action: { ordinal: 0, locator: 'action:0', origin: 'source', title: 'Revision 2 follow-up', status: 'pending' } })], total_count: 1 })));
    releaseMetrics(Response.json(meetingMetrics()));
  });
  expect(await screen.findByText('Revision 2 follow-up')).toBeDefined();
  const meetings = within(screen.getByRole('region', { name: 'Meetings' }));
  expect(meetings.getByRole('heading', { name: 'Meetings', level: 3 })).toBeDefined();
  expect(meetings.getByRole('region', { name: 'Meeting activity' })).toBeDefined();
  expect(meetings.queryByRole('heading', { name: 'Meeting activity and follow-ups' })).toBeNull();
});
