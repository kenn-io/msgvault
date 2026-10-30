import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { meetingAction, meetingActions, meetingMetrics } from '../../meetings/fixtures.test-support';
import MessageMeetingSummary from './MessageMeetingSummary.svelte';

describe('Messages meeting summary', () => {
  it.each([false, true])('resets action filters on close after same-scope refresh=%s', async (refresh) => {
    const actions: Record<string, unknown>[] = [];
    const client = createAPIClient(async (input) => {
      const request = input as Request;
      if (request.url.endsWith('/metrics')) return Response.json(meetingMetrics({ totals: { ...meetingMetrics().totals, meeting_count: 1 } }));
      const body = await request.json();
      actions.push(body);
      return Response.json(meetingActions({ rows: [meetingAction()], total_count: body.assignee_email ? 1 : 2 }));
    });
    const scope = { kind: 'direct' as const, scope: { message_ids: [42] } };
    const view = render(MessageMeetingSummary, { client, scope, refreshKey: 'first' });
    await screen.findByRole('button', { name: 'Action items · 2' });
    await fireEvent.click(screen.getByRole('button', { name: 'Action items · 2' }));
    if (refresh) await view.rerender({ refreshKey: 'second' });
    await waitFor(() => expect(actions).toHaveLength(refresh ? 2 : 1));
    const dialog = screen.getByRole('dialog', { name: 'Action items' });
    await fireEvent.input(within(dialog).getByRole('textbox', { name: 'Assignee' }), { target: { value: 'person@example.com' } });
    await waitFor(() => expect(actions).toHaveLength(refresh ? 3 : 2));
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Close' }));
    await screen.findByRole('button', { name: 'Action items · 2' });
    expect(actions.at(-1)).toEqual({ scope: { message_ids: [42] }, limit: 50 });
    expect(screen.getByText(/^1 meeting ·/)).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Action items · 2' }));
    expect(screen.getByRole('textbox', { name: 'Assignee' })).toHaveProperty('value', '');
  });

  it('opens scoped actions, debounces assignee filters, and navigates to source evidence', async () => {
    const requests: Request[] = [];
    const onOpenMeeting = vi.fn();
    const client = createAPIClient(async (input) => {
      const request = input as Request;
      requests.push(request.clone());
      if (request.url.endsWith('/metrics')) return Response.json(meetingMetrics());
      return Response.json(meetingActions({ rows: [meetingAction()] }));
    });
    render(MessageMeetingSummary, { client, scope: { kind: 'direct', scope: { message_ids: [42] } }, onOpenMeeting });
    expect(await screen.findByText(/4 meetings/)).toBeDefined();
    expect(screen.queryByRole('region', { name: 'Meeting metrics' })).toBeNull();
    await fireEvent.click(await screen.findByRole('button', { name: /Action items/ }));
    const dialog = screen.getByRole('dialog', { name: 'Action items' });
    expect(within(dialog).getByText('Prepare review')).toBeDefined();
    await fireEvent.input(within(dialog).getByRole('textbox', { name: 'Assignee' }), { target: { value: 'person' } });
    await fireEvent.input(within(dialog).getByRole('textbox', { name: 'Assignee' }), { target: { value: 'person@example.test' } });
    await waitFor(() => expect(requests.filter((request) => request.url.endsWith('/actions'))).toHaveLength(2));
    await fireEvent.click(await within(dialog).findByRole('link', { name: 'Archived review' }));
    expect(onOpenMeeting).toHaveBeenCalledWith(expect.objectContaining({ message_id: 42 }));
  });

  it.each([1, 4])('labels a count of %s meetings', async (count) => {
    render(MessageMeetingSummary, { client: createAPIClient(async (input) => (input as Request).url.endsWith('/metrics')
      ? Response.json(meetingMetrics({ totals: { ...meetingMetrics().totals, meeting_count: count } }))
      : Response.json(meetingActions())), scope: { kind: 'direct', scope: { message_ids: [42] } } });
    expect(await screen.findByText(new RegExp(`^${count} ${count === 1 ? 'meeting' : 'meetings'} ·`))).toBeDefined();
  });

  it('asks the workspace for fresh authority after a changed scope', async () => {
    const onReloadScope = vi.fn();
    render(MessageMeetingSummary, {
      client: createAPIClient(async (input) => (input as Request).url.endsWith('/metrics')
        ? Response.json({ error: 'meeting_scope_changed', message: 'Meetings changed.' }, { status: 409 })
        : Response.json(meetingActions({ rows: [meetingAction()], total_count: 1 }))),
      scope: { kind: 'explore', explore: { predicate: {}, cache_revision: 'old', search_provenance: {} } }, onReloadScope,
    });
    await screen.findByText(/Meetings changed/);
    await fireEvent.click(screen.getByRole('button', { name: 'Reload meeting activity' }));
    expect(onReloadScope).toHaveBeenCalledOnce();
  });
});
