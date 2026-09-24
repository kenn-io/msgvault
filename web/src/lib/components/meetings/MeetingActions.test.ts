import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { ActionsPage, MeetingActionsRequest, MeetingRef } from '../../api/generated/models';
import MeetingActions from './MeetingActions.svelte';

const request = {
  scope: { message_ids: [42] },
  limit: 200,
} satisfies MeetingActionsRequest;
const meeting: MeetingRef = {
  message_id: 42,
  conversation_id: 8,
  source_id: 3,
  source_type: 'zoom',
  source_identifier: 'synthetic-zoom',
  source_message_id: 'meeting-42',
  title: 'Café review',
  occurred_at: '2026-09-12T10:00:00Z',
  archive_path: '/api/v1/messages/42',
};

function page(overrides: Partial<ActionsPage> = {}): ActionsPage {
  return {
    schema_version: 1,
    archive_uid: 'archive-test',
    rows: [],
    total_count: 0,
    coverage: {
      meeting_count: 1,
      available: 1,
      partial: 0,
      unsupported: 0,
      unavailable: 0,
    },
    scope: { kind: 'direct' },
    ...overrides,
  };
}

describe('MeetingActions', () => {
  it('shows source evidence and passes authoritative meeting provenance to archive navigation', async () => {
    const requests: Request[] = [];
    const onOpenMeeting = vi.fn();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      requests.push(input instanceof Request ? input : new Request(input));
      return Response.json(
        page({
          rows: [
            {
              meeting,
              action: {
                ordinal: 0,
                locator: 'action:0',
                origin: 'source',
                title: 'Send résumé',
                description: 'Share the revised résumé without changing Unicode.',
                status: 'completed',
                source_status: 'true',
                assignee_name: 'Jamie Example',
                assignee_email: 'jamie@example.test',
                due_date: 'After the next review',
              },
            },
          ],
          total_count: 1,
        }),
      );
    });
    render(MeetingActions, {
      client: createAPIClient(fetchFn),
      request,
      onOpenMeeting,
    });

    expect(await screen.findByText('Send résumé')).toBeDefined();
    expect(screen.getByText('Share the revised résumé without changing Unicode.')).toBeDefined();
    const evidence = screen.getByText('Send résumé').closest('li')!;
    expect(within(evidence).getByText('Source status')).toBeDefined();
    expect(within(evidence).getByText('completed (source: true)')).toBeDefined();
    expect(screen.getByText(/Jamie Example.*jamie@example\.test/)).toBeDefined();
    expect(within(evidence).getByText('Due')).toBeDefined();
    expect(within(evidence).getByText('After the next review')).toBeDefined();
    expect(screen.getByText('Coverage: 1 available · 0 partial · 0 unsupported · 0 unavailable')).toBeDefined();
    const archiveLink = screen.getByRole('link', {
      name: 'Open archived meeting',
    });
    expect(archiveLink.getAttribute('href')).toBe('/api/v1/messages/42');
    await fireEvent.click(archiveLink);
    expect(onOpenMeeting).toHaveBeenCalledWith(meeting);
    await expect(requests[0]!.clone().json()).resolves.toEqual(request);
  });

  it.each([
    [
      {
        meeting_count: 1,
        available: 1,
        partial: 0,
        unsupported: 0,
        unavailable: 0,
      },
      'No recorded action items',
    ],
    [
      {
        meeting_count: 1,
        available: 0,
        partial: 1,
        unsupported: 0,
        unavailable: 0,
      },
      'Action item evidence is partial',
    ],
    [
      {
        meeting_count: 1,
        available: 0,
        partial: 0,
        unsupported: 1,
        unavailable: 0,
      },
      'Action items are not supported',
    ],
    [
      {
        meeting_count: 1,
        available: 0,
        partial: 0,
        unsupported: 0,
        unavailable: 1,
      },
      'Action item evidence is unavailable',
    ],
  ])('uses coverage to distinguish empty evidence state %#', async (coverage, expected) => {
    render(MeetingActions, {
      client: createAPIClient(async () => Response.json(page({ coverage }))),
      request,
    });

    expect(await screen.findByText(new RegExp(expected))).toBeDefined();
  });

  it('aborts the old action request and never renders its stale response after scope changes', async () => {
    let resolveFirst!: (response: Response) => void;
    let firstRequest!: Request;
    let requestCount = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      requestCount += 1;
      const outgoing = input instanceof Request ? input : new Request(input);
      if (requestCount === 1) {
        firstRequest = outgoing;
        return new Promise<Response>((resolve) => {
          resolveFirst = resolve;
        });
      }
      return Response.json(page());
    });
    const client = createAPIClient(fetchFn);
    const view = render(MeetingActions, { client, request });
    await waitFor(() => expect(firstRequest).toBeInstanceOf(Request));

    await view.rerender({
      client,
      request: { scope: { message_ids: [104] }, limit: 200 },
    });
    expect(firstRequest.signal.aborted).toBe(true);
    resolveFirst(
      Response.json(
        page({
          rows: [
            {
              meeting,
              action: {
                ordinal: 0,
                locator: 'stale',
                origin: 'source',
                title: 'Stale action',
                status: 'open',
              },
            },
          ],
          total_count: 1,
        }),
      ),
    );

    await screen.findByText('No recorded action items');
    expect(screen.queryByText('Stale action')).toBeNull();
  });

  it('shows endpoint failures as explicit unavailable evidence', async () => {
    render(MeetingActions, {
      client: createAPIClient(async () =>
        Response.json(
          {
            error: 'meetings_unavailable',
            message: 'Meeting intelligence is unavailable',
          },
          { status: 503 },
        ),
      ),
      request,
    });

    expect((await screen.findByRole('alert')).textContent).toContain('Meeting intelligence is unavailable');
  });
});


it('continues reader pages, reconciles evidence and keeps a failed page visible', async () => {
  let fail = true;
  const row = (title: string, locator = 'action:0') => ({ meeting, action: { ordinal: 0, locator, origin: 'source', title, status: 'pending' } });
  const client = createAPIClient(async (input) => {
    const outgoing = input as Request;
    const body = await outgoing.json();
    if (!body.cursor) return Response.json(page({ rows: [row('Initial action')], total_count: 2, next_cursor: 'next' }));
    if (fail) { fail = false; return Response.json({ error: 'unavailable', message: 'Continuation failed' }, { status: 503 }); }
    return Response.json(page({ rows: [row('Updated action'), row('Final action', 'action:1')], total_count: 2 }));
  });
  render(MeetingActions, { client, request });
  await screen.findByText('Initial action');
  expect(screen.getByText('Showing 1 of 2 action items')).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Load more action items' }));
  expect((await screen.findByRole('alert')).textContent).toContain('Continuation failed');
  expect(screen.getByText('Initial action')).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Retry action items' }));
  await screen.findByText('Final action');
  expect(screen.queryByText('Initial action')).toBeNull();
  expect(screen.getByText('Updated action')).toBeDefined();
  expect(screen.getByText('Showing 2 of 2 action items')).toBeDefined();
  expect(screen.queryByRole('button', { name: 'Load more action items' })).toBeNull();
});

it('cancels a pending continuation and ignores its response after changing reader scope', async () => {
  let resolveMore!: (response: Response) => void;
  let pending!: Request;
  const client = createAPIClient(async (input) => {
    const outgoing = input as Request;
    const body = await outgoing.json();
    if (body.cursor) { pending = outgoing; return new Promise<Response>((resolve) => { resolveMore = resolve; }); }
    if (body.scope.message_ids[0] === 104) return Response.json(page());
    return Response.json(page({ rows: [{ meeting, action: { ordinal: 0, locator: 'first', origin: 'source', title: 'First action', status: 'pending' } }], total_count: 2, next_cursor: 'next' }));
  });
  const view = render(MeetingActions, { client, request });
  await screen.findByText('First action');
  await fireEvent.click(screen.getByRole('button', { name: 'Load more action items' }));
  await waitFor(() => expect(pending).toBeDefined());
  await view.rerender({ client, request: { scope: { message_ids: [104] }, limit: 200 } });
  expect(pending.signal.aborted).toBe(true);
  resolveMore(Response.json(page({ rows: [{ meeting, action: { ordinal: 1, locator: 'stale', origin: 'source', title: 'Stale continuation', status: 'pending' } }], total_count: 2 })));
  await screen.findByText('No recorded action items');
  expect(screen.queryByText('First action')).toBeNull();
  expect(screen.queryByText('Stale continuation')).toBeNull();
});
