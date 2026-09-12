import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import type { MessageDetail } from '../../api/generated/models';
import { chooseSelectOption } from '../../../test/kit-ui';
import { ExploreState } from '../../explore/state.svelte';
import { meetingActions, meetingMetrics } from '../../meetings/fixtures.test-support';
import AppShell from '../shell/AppShell.svelte';

const detail: MessageDetail = { id: 42, conversation_id: 8, message_type: 'meeting_transcript', subject: 'Archived review', sent_at: '2026-01-01T00:00:00Z', from: 'Example', to: [], body: 'Archived transcript body', labels: [], attachments: [], has_attachments: false, size_bytes: 10, snippet: 'Archived' };
const action = { meeting: { message_id: 42, conversation_id: 8, source_id: 3, source_type: 'zoom', source_identifier: 'example-zoom', source_message_id: 'source-42', title: 'Action provenance title', occurred_at: detail.sent_at, archive_path: '/api/v1/messages/42' },
  action: { ordinal: 0, locator: 'action:0', origin: 'source', title: 'Prepare review', status: 'pending' } };

async function metricsResponse(request: Request): Promise<Response> {
  const body = await request.clone().json();
  return Response.json(body.scope?.message_ids?.length === 1
    ? meetingMetrics({ totals: { meeting_count: 1, known_duration_count: 0, unknown_duration_count: 1, total_known_seconds: 0, average_known_seconds: null } })
    : meetingMetrics());
}

function handler(detailResponse: () => Response | Promise<Response> = () => Response.json(detail)) {
  const requests: Request[] = [];
  const client = createAPIClient(async (input) => {
    const request = input instanceof Request ? input : new Request(input); requests.push(request);
    const path = new URL(request.url).pathname;
    if (path.endsWith('/meetings/metrics')) return metricsResponse(request);
    if (path.endsWith('/meetings/actions')) return Response.json(meetingActions({ rows: [action], total_count: 1 }));
    if (path.endsWith('/messages/42')) return detailResponse();
    if (path === '/api/v1/people/7') return Response.json({ id: 7, revision: 1, display_name: 'Example Person', participant_ids: [7], vcard_uid: 'person-7', created_at: '', updated_at: '' });
    if (path === '/api/v1/people/directory') return Response.json({ people: [] });
    if (path.endsWith('/brief-enrollment')) return Response.json({ person_id: 7, enrolled: false });
    if (path.endsWith('/tracking')) return Response.json({ person_id: 7, tracked: false });
    if (path.startsWith('/api/v1/people/') || path.startsWith('/api/v1/carddav/')) return Response.json({ error: 'not_found', message: 'Synthetic optional section unavailable.' }, { status: 404 });
    if (path.endsWith('/conversations/8')) return Response.json({ id: 8, messages: [detail], anchor_id: 42, total: 1, has_before: false, has_after: false });
    return Response.json({ rows: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} });
  });
  return { client, requests };
}
function stateWithMeetingFilter(selectedRow: string | null = null): ExploreState {
  window.history.replaceState(null, '', '/');
  const state = new ExploreState(window);
  state.commitNavigation({ workspace: 'everything', selectedRow, filters: [{ dimension: 'message_type', values: ['meeting_transcript'] }, { dimension: 'source', values: ['3'] }] });
  return state;
}
const historyChange = () => new Promise<void>((resolve) => window.addEventListener('popstate', () => resolve(), { once: true }));

function groupNavigationClient() {
  const second = { ...detail, id: 43, subject: 'Archived follow-up', body: 'Follow-up transcript body' };
  return createAPIClient(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    const url = new URL(request.url);
    if (url.pathname.endsWith('/meetings/metrics')) return metricsResponse(request);
    if (url.pathname.endsWith('/meetings/actions')) {
      const body = await request.json();
      const messageID = body.scope?.message_ids?.[0];
      const rows = messageID === 43 ? [] : messageID === 42
        ? [{ ...action, meeting: { ...action.meeting, message_id: 43, archive_path: '/api/v1/messages/43' } }] : [action];
      return Response.json(meetingActions({ rows, total_count: rows.length }));
    }
    if (url.pathname.endsWith('/messages/42')) return Response.json(detail);
    if (url.pathname.endsWith('/messages/43')) return Response.json(second);
    if (url.pathname.endsWith('/conversations/8')) return Response.json({ id: 8, messages: [detail, second],
      anchor_id: Number(url.searchParams.get('anchor')), total: 2, has_before: false, has_after: false });
    if (url.pathname.endsWith('/groups')) return Response.json({ rows: [{ key: 'exact.example', label: 'Exact domain', count: 2 }],
      total_count: 1, cache_revision: 'group-cache', search_provenance: {} });
    if (url.pathname.endsWith('/files')) return Response.json({ files: [], total_count: 0, cache_revision: 'files-cache', search_provenance: {} });
    return Response.json({ rows: [], total_count: 0, cache_revision: 'outer-cache', search_provenance: {} });
  });
}

function stateWithGroup(): ExploreState {
  window.history.replaceState(null, '', '/');
  const state = new ExploreState(window);
  state.commitNavigation({ workspace: 'everything', selectedRow: 'group:domain:exact.example',
    filters: [{ dimension: 'source', values: ['3'] }] });
  return state;
}

describe('meeting archive reader navigation', () => {
  it('preflights detail, opens the real reader, and restores scope and focus through Back/Forward and Close', async () => {
    let resolveDetail!: (response: Response) => void;
    const { client, requests } = handler(() => new Promise<Response>((resolve) => { resolveDetail = resolve; }));
    const state = stateWithMeetingFilter();
    const view = render(AppShell, { client, state });
    await fireEvent.input(await screen.findByRole('textbox', { name: 'Assignee email' }), { target: { value: 'person@example.test' } });
    await chooseSelectOption(screen.getByRole('combobox', { name: /^Source status:/ }), 'Pending');
    await fireEvent.click(screen.getByRole('button', { name: 'Apply action filters' }));
    const link = await screen.findByRole('link', { name: 'Open archived meeting' });
    const filters = state.current.filters;
    link.focus();
    await fireEvent.click(link);
    await waitFor(() => expect(resolveDetail).toBeDefined());
    expect(state.current.selectedRow).toBeNull();
    expect(screen.queryByRole('dialog', { name: 'Archived meeting' })).toBeNull();
    resolveDetail(Response.json(detail));
    const reader = await screen.findByRole('dialog', { name: 'Archived meeting' });
    expect(await within(reader).findByText('Archived transcript body')).toBeDefined();
    expect(within(reader).getByRole('complementary', { name: 'Reading pane: Archived review' })).toBeDefined();
    expect(state.current.workspace).toBe('everything');
    expect(state.current.filters).toEqual(filters);
    expect(state.current.selectedRow).toBe('archive-meeting:42');
    const back = historyChange(); window.history.back(); await back;
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Archived meeting' })).toBeNull());
    expect(state.current.filters).toEqual(filters);
    expect(screen.getByText('4 meetings')).toBeDefined();
    expect((screen.getByRole('textbox', { name: 'Assignee email' }) as HTMLInputElement).value).toBe('person@example.test');
    expect(screen.getByRole('combobox', { name: 'Source status: Pending' })).toBeDefined();
    const forward = historyChange(); window.history.forward(); await forward;
    await screen.findByRole('dialog', { name: 'Archived meeting' });
    const close = historyChange();
    await fireEvent.click(screen.getByRole('button', { name: 'Close archived meeting' }));
    await close;
    await waitFor(() => expect(document.activeElement).toBe(link));
    const metricRequests = await Promise.all(requests.filter((request) => request.url.endsWith('/meetings/metrics')).map((request) => request.clone().json()));
    expect(metricRequests.filter((body) => body.scope?.message_ids === undefined)).toHaveLength(2);
    expect(metricRequests.filter((body) => body.scope?.message_ids?.[0] === 42)).toEqual([{ scope: { message_ids: [42] } }]);
    view.unmount(); state.destroy();
  });

  it('restores an archive marker from a deep URL without fabricating a list row or draining pages', async () => {
    const { client, requests } = handler();
    window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything', selectedRow: 'archive-meeting:42' }))}`);
    const state = new ExploreState(window);
    const view = render(AppShell, { client, state });
    const reader = await screen.findByRole('dialog', { name: 'Archived meeting' });
    expect(await within(reader).findByText('Archived transcript body')).toBeDefined();
    expect(requests.some((request) => request.url.endsWith('/messages/42'))).toBe(true);
    await fireEvent.click(screen.getByRole('button', { name: 'Close archived meeting' }));
    await waitFor(() => expect(state.current.selectedRow).toBeNull());
    view.unmount(); state.destroy();
  });

  it('does not navigate after the originating scope changes while getMessage ignores abort', async () => {
    let resolveDetail!: (response: Response) => void;
    const { client, requests } = handler(() => new Promise<Response>((resolve) => { resolveDetail = resolve; }));
    const state = stateWithMeetingFilter(); const view = render(AppShell, { client, state });
    await fireEvent.click(await screen.findByRole('link', { name: 'Open archived meeting' }));
    await waitFor(() => expect(resolveDetail).toBeDefined());
    state.commitNavigation({ filters: [{ dimension: 'source', values: ['4'] }] });
    await waitFor(() => expect(requests.find((request) => request.url.endsWith('/messages/42'))!.signal.aborted).toBe(true));
    resolveDetail(Response.json(detail));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(state.current.selectedRow).toBeNull();
    expect(screen.queryByRole('dialog', { name: 'Archived meeting' })).toBeNull();
    view.unmount(); state.destroy();
  });

  it('shows missing archive detail without navigating to an invented row', async () => {
    const { client } = handler(() => Response.json({ error: 'not_found', message: 'Archived meeting missing.' }, { status: 404 }));
    const state = stateWithMeetingFilter(); const view = render(AppShell, { client, state });
    await fireEvent.click(await screen.findByRole('link', { name: 'Open archived meeting' }));
    expect(await screen.findByText('Archived meeting missing.')).toBeDefined();
    expect(state.current.selectedRow).toBeNull();
    view.unmount(); state.destroy();
  });

  it('returns to the selected group after expanding another message and transient reader updates', async () => {
    const state = stateWithGroup();
    const view = render(AppShell, { client: groupNavigationClient(), state });
    try {
      const source = await screen.findByRole('link', { name: 'Open archived meeting' });
      source.focus();
      await fireEvent.click(source);
      const reader = await screen.findByRole('dialog', { name: 'Archived meeting' });
      await fireEvent.click(await within(reader).findByRole('button', { name: 'Expand message 43 from Example' }));
      await waitFor(() => expect(state.current.conversationAnchor).toBe('43'));
      expect(await within(reader).findByText('Follow-up transcript body')).toBeDefined();
      state.replaceTransient({ columnWidths: { title: 432 } });
      await fireEvent.click(within(reader).getByRole('button', { name: 'Close archived meeting' }));
      await waitFor(() => expect(state.current.selectedRow).toBe('group:domain:exact.example'));
      expect(screen.getByRole('complementary', { name: 'Reading pane: Exact domain' })).toBeDefined();
      expect(state.current.filters).toEqual([{ dimension: 'source', values: ['3'] }]);
      expect(state.current.conversationAnchor).toBeNull();
      await waitFor(() => expect(document.activeElement).toBe(source));
    } finally { view.unmount(); state.destroy(); }
  });

  it('keeps the first reader history ownership through a nested reader, Back, and Close', async () => {
    const state = stateWithGroup();
    const view = render(AppShell, { client: groupNavigationClient(), state });
    try {
      await fireEvent.click(await screen.findByRole('link', { name: 'Open archived meeting' }));
      const firstReader = await screen.findByRole('dialog', { name: 'Archived meeting' });
      await fireEvent.click(await within(firstReader).findByRole('link', { name: 'Open archived meeting' }));
      await screen.findByRole('complementary', { name: 'Reading pane: Archived follow-up' });
      expect(state.current.selectedRow).toBe('archive-meeting:43');
      const back = historyChange(); window.history.back(); await back;
      await screen.findByRole('complementary', { name: 'Reading pane: Archived review' });
      expect(state.current.selectedRow).toBe('archive-meeting:42');
      await fireEvent.click(screen.getByRole('button', { name: 'Close archived meeting' }));
      await waitFor(() => expect(state.current.selectedRow).toBe('group:domain:exact.example'));
      expect(screen.getByRole('complementary', { name: 'Reading pane: Exact domain' })).toBeDefined();
      expect(state.current.filters).toEqual([{ dimension: 'source', values: ['3'] }]);
    } finally { view.unmount(); state.destroy(); }
  });
});

 it.each([false, true])('opens Directory meeting actions through the shell and returns to the person (narrow=%s)', async (narrow) => {
   vi.stubGlobal('matchMedia', () => ({ matches: narrow, addEventListener: vi.fn(), removeEventListener: vi.fn() }));
   const { client } = handler();
   window.history.replaceState(null, '', '/');
   const state = new ExploreState(window);
   state.commitNavigation({ workspace: 'directory', directoryPersonID: 7 });
   const view = render(AppShell, { client, state });
   try {
     const link = await screen.findByRole('link', { name: 'Open archived meeting' });
     await fireEvent.click(link);
     const reader = await screen.findByRole('dialog', { name: 'Archived meeting' });
     expect(await within(reader).findByText('Archived transcript body')).toBeDefined();
     expect(state.current.workspace).toBe('directory');
     expect(state.current.directoryPersonID).toBe(7);
     const close = historyChange(); await fireEvent.click(within(reader).getByRole('button', { name: 'Close archived meeting' })); await close;
     await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Archived meeting' })).toBeNull());
     expect(state.current.directoryPersonID).toBe(7);
     expect(screen.getByText('4 meetings')).toBeDefined();
   } finally { view.unmount(); state.destroy(); vi.unstubAllGlobals(); }
 });
