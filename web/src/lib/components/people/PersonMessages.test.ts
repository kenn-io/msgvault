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
function setup(paged: boolean, row = saved) {
  const requests: Request[] = [];
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    requests.push(request);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/participants/1/timeline') {
      const body = await request.clone().json();
      return Response.json({ rows: paged && !body.cursor ? [first] : [row], total_count: paged ? 2 : 1, next_cursor: paged && !body.cursor ? 'page-two' : undefined, cache_revision: 'cache-1', search_provenance: {} });
    }
    if (path === '/api/v1/integrations/tasks/status') return Response.json({ state: 'disabled', message: 'Tasks disabled.' });
    if (path === '/api/v1/cli/accounts') return Response.json({ accounts: [] });
    if (path === '/api/v1/conversations/2') return Response.json({ id: 2, messages: [{ id: 2, conversation_id: 2, subject: 'Saved message', sent_at: at, message_type: row.message_type, from: 'Alice Example', to: [], body: 'Restored body', snippet: 'Restored body', attachments: [] }], anchor_id: 2, total: 1, has_before: false, has_after: false });
    return meetingFixtureResponse(path) ?? Response.json({ rows: [] });
  });
  const props = { client, identityScope: { kind: 'person' as const, id: 1 }, target: 'cluster:1', predicate: {}, onOpenFiles: vi.fn(), onFiltersChange: vi.fn(), onClearPeriod: vi.fn() };
  return { props, requests };
}
it.each([
  { name: 'opened after the first result page', paged: true, period: undefined },
  { name: 'with its selected calendar day window', paged: false, period: { after: '2026-07-18T00:00:00.000Z', before: '2026-07-19T00:00:00.000Z' } },
])('restores a reader $name', async ({ paged, period }) => {
  const row = period ? { ...saved, key: 'conversation:2', kind: 'conversation', message_type: 'slack', conversation_type: 'chat', source_type: 'slack' } : saved;
  const { props, requests } = setup(paged, row);
  let view = render(PersonMessages, { props: { ...props, period } });
  try {
    const grid = await screen.findByRole('grid', { name: 'Messages results' });
    if (paged) { await within(grid).findByText('First page message'); await fireEvent.keyDown(grid, { key: 'End' }); }
    await fireEvent.click(await within(grid).findByText('Saved message'));
    await screen.findByText('Restored body');
    await waitFor(() => expect(window.history.state[RELATIONSHIP_READER_HISTORY_KEY]?.selection.row.key).toBe(period ? 'conversation:2' : 'message:2'));
    view.unmount();
    if (period) row.anchor_message_id = 99;
    view = render(PersonMessages, { props });
    if (paged) await screen.findByText('First page message');
    await screen.findByText('Restored body');
    if (period) {
      const conversation = requests.filter(request => new URL(request.url).pathname === '/api/v1/conversations/2').at(-1)!;
      expect(new URL(conversation.url).searchParams.get('start')).toBe(period.after);
      expect(new URL(conversation.url).searchParams.get('end')).toBe(period.before);
      expect(new URL(conversation.url).searchParams.get('anchor')).toBe('2');
    }
  } finally { view.unmount(); }
});
it('clears the previous reader on a period change and opens the first clicked conversation', async () => {
  const { props, requests } = setup(false, { ...saved, key: 'conversation:2', kind: 'conversation', message_type: 'slack', conversation_type: 'chat', source_type: 'slack' });
  const view = render(PersonMessages, { props });
  try {
    const grid = await screen.findByRole('grid', { name: 'Messages results' });
    await fireEvent.click(await within(grid).findByText('Saved message'));
    await screen.findByText('Restored body');
    await waitFor(() => expect(window.history.state[RELATIONSHIP_READER_HISTORY_KEY]).toBeDefined());
    await view.rerender({ ...props, period: { after: '2026-07-18T00:00:00.000Z', before: '2026-07-19T00:00:00.000Z' } });
    await waitFor(() => expect(window.history.state[RELATIONSHIP_READER_HISTORY_KEY]).toBeUndefined());
    await waitFor(() => expect(grid.getAttribute('aria-busy')).toBe('false'));
    await fireEvent.click(await within(grid).findByText('Saved message'));
    await screen.findByText('Restored body');
    expect(requests.filter(request => new URL(request.url).pathname.endsWith('/timeline'))).toHaveLength(2);
  } finally { view.unmount(); }
});

it('Escape dismisses the message filter before closing the reader', async () => {
  const { props } = setup(false);
  const view = render(PersonMessages, { props });
  try {
    const grid = await screen.findByRole('grid', { name: 'Messages results' });
    await fireEvent.click(await within(grid).findByText('Saved message'));
    await screen.findByText('Restored body');
    const filter = screen.getByRole('button', { name: 'Filter' });
    await fireEvent.click(filter);
    await screen.findByRole('dialog', { name: 'Filter' });
    await fireEvent.keyDown(filter, { key: 'Escape' });
    expect(screen.queryByText('Restored body')).not.toBeNull();
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Filter' })).toBeNull());
  } finally { view.unmount(); }
});

it('opens Tasks setup from a person message reader', async () => {
  const { props } = setup(false);
  const onOpenSettings = vi.fn();
  const view = render(PersonMessages, { props: { ...props, onOpenSettings } });
  try {
    await fireEvent.click(await within(await screen.findByRole('grid', { name: 'Messages results' })).findByText('Saved message'));
    await fireEvent.click(await screen.findByRole('button', { name: /^Set up$/ }));
    expect(onOpenSettings).toHaveBeenCalledOnce();
  } finally { view.unmount(); }
});
