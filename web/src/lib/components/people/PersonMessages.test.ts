import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { meetingFixtureResponse } from '../../meetings/fixtures.test-support';
import { RELATIONSHIP_READER_HISTORY_KEY } from '../../relationships/reader-history';
import PersonMessages from './PersonMessages.svelte';

const at = '2026-07-18T10:00:00Z';
const saved = { key: 'message:2', kind: 'email', title: 'Saved message', occurred_at: at, first_at: at, message_type: 'email', conversation_type: 'email', source_id: 1, source_type: 'gmail', source_identifier: 'owner@example.com', participant_labels: [], participant_ids: [], attachment_count: 0, attachment_size: 0, message_count: 1, deleted_from_source: false, conversation_id: 2, anchor_message_id: 2, match: {} };
const first = { ...saved, key: 'message:1', title: 'First page message', conversation_id: 1, anchor_message_id: 1 };
afterEach(() => window.history.replaceState(null, '', '/'));
function setup(paged: boolean) {
  const requests: Request[] = [];
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    requests.push(request);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/participants/1/timeline') {
      const body = await request.clone().json();
      return Response.json({ rows: paged && !body.cursor ? [first] : [saved], total_count: paged ? 2 : 1, next_cursor: paged && !body.cursor ? 'page-two' : undefined, cache_revision: 'cache-1', search_provenance: {} });
    }
    if (path === '/api/v1/conversations/2') return Response.json({ id: 2, messages: [{ id: 2, conversation_id: 2, subject: 'Saved message', sent_at: at, message_type: 'email', from: 'Alice Example', to: [], body: 'Restored body', snippet: 'Restored body', attachments: [] }], anchor_id: 2, total: 1, has_before: false, has_after: false });
    return meetingFixtureResponse(path) ?? Response.json({ rows: [] });
  });
  const props = { client, identityScope: { kind: 'person' as const, id: 1 }, target: 'cluster:1', predicate: {}, onOpenFiles: vi.fn(), onFiltersChange: vi.fn(), onClearPeriod: vi.fn() };
  return { props, requests };
}
it('restores a reader opened after the first result page', async () => {
  window.history.replaceState(null, '', '/');
  const { props } = setup(true);
  let view = render(PersonMessages, { props });
  try {
    const grid = await screen.findByRole('grid', { name: 'Messages results' });
    await within(grid).findByText('First page message');
    await fireEvent.keyDown(grid, { key: 'End' });
    await fireEvent.click(await within(grid).findByText('Saved message'));
    await screen.findByText('Restored body');
    await waitFor(() => expect(window.history.state[RELATIONSHIP_READER_HISTORY_KEY]?.selection.row.key).toBe('message:2'));
    view.unmount();
    view = render(PersonMessages, { props });
    await screen.findByText('First page message');
    await screen.findByText('Restored body');
  } finally { view.unmount(); }
});
it('restores the selected calendar day conversation window', async () => {
  window.history.replaceState(null, '', '/');
  const { props, requests } = setup(false);
  const period = { after: '2026-07-18T00:00:00.000Z', before: '2026-07-19T00:00:00.000Z' };
  let view = render(PersonMessages, { props: { ...props, period } });
  try {
    const grid = await screen.findByRole('grid', { name: 'Messages results' });
    await fireEvent.click(await within(grid).findByText('Saved message'));
    await screen.findByText('Restored body');
    await waitFor(() => expect(window.history.state[RELATIONSHIP_READER_HISTORY_KEY]?.selection.row.key).toBe('message:2'));
    const initial = requests.filter(request => new URL(request.url).pathname === '/api/v1/conversations/2').at(-1)!;
    expect(new URL(initial.url).searchParams.get('start')).toBe(period.after);
    view.unmount();
    view = render(PersonMessages, { props });
    await screen.findByText('Restored body');
    const conversation = requests.filter(request => new URL(request.url).pathname === '/api/v1/conversations/2').at(-1)!;
    expect(new URL(conversation.url).searchParams.get('start')).toBe(period.after);
    expect(new URL(conversation.url).searchParams.get('end')).toBe(period.before);
  } finally { view.unmount(); }
});
