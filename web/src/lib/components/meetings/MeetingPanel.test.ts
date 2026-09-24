import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { meetingAction, meetingActions, meetingMetrics } from '../../meetings/fixtures.test-support';
import MeetingPanel from './MeetingPanel.svelte';

function response(request: Request): Response {
  return Response.json(new URL(request.url).pathname.endsWith('/metrics') ? meetingMetrics() : meetingActions());
}

describe('MeetingPanel', () => {
  it('shows metrics and scoped source evidence with page-one source filters', async () => {
    const requests: Request[] = [];
    render(MeetingPanel, { client: createAPIClient(async (input) => {
      const request = input instanceof Request ? input : new Request(input); requests.push(request); return response(request);
    }), scope: { kind: 'direct', scope: { person_id: 7 } } });
    expect(await screen.findByText('4 meetings')).toBeDefined();
    expect(await screen.findByText('Coverage: 1 available · 1 partial · 1 unsupported · 1 unavailable')).toBeDefined();
    expect(screen.getByRole('combobox', { name: 'Source status: All source statuses' })).toBeDefined();
    await fireEvent.input(screen.getByRole('textbox', { name: 'Assignee email' }), { target: { value: 'person@example.test' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Apply action filters' }));
    await waitFor(() => expect(requests.filter((request) => request.url.endsWith('/actions'))).toHaveLength(2));
    const last = requests.filter((request) => request.url.endsWith('/actions')).at(-1)!;
    await expect(last.clone().json()).resolves.toEqual({ scope: { person_id: 7 }, assignee_email: 'person@example.test', limit: 50 });
  });

  it('asks the owner for fresh Explore authority on explicit reload without repeating failed authority', async () => {
    const onReloadScope = vi.fn();
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ error: 'meeting_scope_changed', message: 'Meetings changed.' }, { status: 409 }));
    const props = { client: createAPIClient(fetchFn), scope: { kind: 'explore' as const, explore: {
      predicate: { filters: [] }, cache_revision: 'old-cache', search_provenance: {}
    } }, onReloadScope };
    const view = render(MeetingPanel, props);
    await screen.findByText(/Meetings changed.*Reload meeting activity/);
    await fireEvent.click(screen.getByRole('button', { name: 'Reload meeting activity' }));
    expect(onReloadScope).toHaveBeenCalledTimes(1);
    expect(fetchFn).toHaveBeenCalledTimes(2);
    await view.rerender({ ...props, scope: { ...props.scope, explore: { ...props.scope.explore, cache_revision: 'new-cache' } } });
    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(4));
  });

  it('keeps scope-too-large guidance visible and does not offer a looping reload', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ error: 'meeting_scope_too_large', message: 'Over 10000.' }, { status: 400 }));
    render(MeetingPanel, { client: createAPIClient(fetchFn), scope: { kind: 'direct', scope: { domains: ['example.test'] } } });
    expect(await screen.findByText(/Choose narrower filters/)).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Reload meeting activity' })).toBeNull();
    expect(fetchFn).toHaveBeenCalledTimes(2);
  });

  it('renders updated evidence once when a source edit repeats an action on the next page', async () => {
    const first = meetingAction();
    const updated = meetingAction({ action: { ...first.action, title: 'Review prepared', status: 'completed' } });
    const distinct = meetingAction({ action: { ...first.action, ordinal: 1, locator: 'action:1', title: 'Send notes' } });
    render(MeetingPanel, { client: createAPIClient(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.url.endsWith('/metrics')) return Response.json(meetingMetrics());
      const body = await request.json();
      return Response.json(body.cursor ? meetingActions({ rows: [updated, distinct], total_count: 2 })
        : meetingActions({ rows: [first], total_count: 2, next_cursor: 'second-page' }));
    }), scope: { kind: 'direct', scope: { person_id: 7 } } });
    await screen.findByText('Prepare review');
    await fireEvent.click(screen.getByRole('button', { name: 'Load more action items' }));
    await screen.findByText('Review prepared');
    const evidence = screen.getByRole('region', { name: 'Archived action items' });
    expect(within(evidence).getAllByRole('listitem')).toHaveLength(2);
    expect(within(evidence).getByText('completed')).toBeDefined();
    expect(within(evidence).getByText('Send notes')).toBeDefined();
    expect(screen.queryByText('Prepare review')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Load more action items' })).toBeNull();
  });
});
