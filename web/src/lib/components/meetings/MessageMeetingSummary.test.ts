import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { meetingAction, meetingActions, meetingMetrics } from '../../meetings/fixtures.test-support';
import MessageMeetingSummary from './MessageMeetingSummary.svelte';

describe('Messages meeting summary', () => {
  it('opens scoped actions, applies assignee filters, pages, and navigates to source evidence', async () => {
    const requests: Request[] = [];
    const onOpenMeeting = vi.fn();
    const client = createAPIClient(async (input) => {
      const request = input as Request;
      requests.push(request.clone());
      if (request.url.endsWith('/metrics')) return Response.json(meetingMetrics());
      const body = await request.json();
      return Response.json(meetingActions({ rows: [meetingAction()], next_cursor: body.cursor ? undefined : 'page-2' }));
    });
    render(MessageMeetingSummary, { client, scope: { kind: 'direct', scope: { message_ids: [42] } }, onOpenMeeting });
    expect(await screen.findByText(/4 meetings/)).toBeDefined();
    expect(screen.queryByRole('region', { name: 'Meeting metrics' })).toBeNull();
    await fireEvent.click(await screen.findByRole('button', { name: /Action items/ }));
    const dialog = screen.getByRole('dialog', { name: 'Action items' });
    expect(within(dialog).getByText('Prepare review')).toBeDefined();
    const status = within(dialog).getByRole('combobox', { name: /^Status:/ });
    await fireEvent.click(status);
    await screen.findByRole('listbox');
    await fireEvent.keyDown(status, { key: 'Escape' });
    expect(screen.getByRole('dialog', { name: 'Action items' })).toBe(dialog);
    expect(screen.queryByRole('listbox')).toBeNull();
    await fireEvent.click(within(dialog).getByText('Meeting metrics', { selector: 'summary' }));
    const metrics = within(dialog).getByRole('region', { name: 'Meeting metrics' });
    expect(within(metrics).getByLabelText('Average known duration').textContent).toBe('33m 20s');
    expect(within(metrics).getByRole('table', { name: 'Monthly meeting activity' })).toBeDefined();
    await fireEvent.input(within(dialog).getByRole('textbox', { name: 'Assignee' }), { target: { value: 'person@example.test' } });
    await waitFor(() => expect(requests.filter((request) => request.url.endsWith('/actions'))).toHaveLength(2));
    await expect(requests.filter((request) => request.url.endsWith('/actions')).at(-1)!.json()).resolves.toEqual({ scope: { message_ids: [42] }, assignee_email: 'person@example.test', limit: 50 });
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Show more' }));
    await waitFor(() => expect(within(dialog).queryByRole('button', { name: 'Show more' })).toBeNull());
    await fireEvent.click(within(dialog).getByRole('link', { name: 'Archived review' }));
    expect(onOpenMeeting).toHaveBeenCalledWith(expect.objectContaining({ message_id: 42 }));
  });

  it.each([[0, '0s'], [30, '30s'], [90, '1m 30s'], [3600, '1h']])('formats %s known seconds as %s', async (seconds, expected) => {
    render(MessageMeetingSummary, { client: createAPIClient(async (input) => (input as Request).url.endsWith('/metrics')
      ? Response.json(meetingMetrics({ totals: { ...meetingMetrics().totals, total_known_seconds: seconds } }))
      : Response.json(meetingActions())), scope: { kind: 'direct', scope: { message_ids: [42] } } });
    expect(await screen.findByText(`4 meetings · ${expected} known`)).toBeDefined();
  });

  it('asks the workspace for fresh authority after a changed scope', async () => {
    const onReloadScope = vi.fn();
    render(MessageMeetingSummary, {
      client: createAPIClient(async () => Response.json({ error: 'meeting_scope_changed', message: 'Meetings changed.' }, { status: 409 })),
      scope: { kind: 'explore', explore: { predicate: {}, cache_revision: 'old', search_provenance: {} } }, onReloadScope,
    });
    await screen.findByText(/Meetings changed/);
    await fireEvent.click(screen.getByRole('button', { name: 'Reload meeting activity' }));
    expect(onReloadScope).toHaveBeenCalledOnce();
  });
});
