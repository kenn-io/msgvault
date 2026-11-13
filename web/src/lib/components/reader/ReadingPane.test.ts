import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { meetingActions, meetingMetrics } from '../../meetings/fixtures.test-support';
import type { EntryRow, ExploreFilter, ExplorePredicate } from '../../explore/models';
import ReadingPane from './ReadingPane.svelte';

function entryRow(overrides: Partial<EntryRow> = {}): EntryRow {
  return {
    key: 'entry-1',
    kind: 'message',
    title: 'Synthetic subject',
    preview: 'Synthetic preview',
    message_type: 'email',
    conversation_type: '',
    source_id: 1,
    source_type: 'gmail',
    source_identifier: 'archive@example.com',
    occurred_at: '2026-07-18T12:00:00Z',
    message_count: 1,
    attachment_count: 0,
    attachment_size: 0,
    has_attachments: false,
    deleted_from_source: false,
    matched_sender_identities: [],
    matched_recipient_identities: [],
    match: {},
    anchor_message_id: 42,
    ...overrides
  };
}

function renderPane(row: EntryRow) {
  return render(ReadingPane, {
    props: {
      client: createAPIClient(vi.fn<typeof fetch>()),
      selection: { kind: 'entry', row },
      predicate: {} satisfies ExplorePredicate
    }
  });
}

describe('ReadingPane header', () => {
  it('uses a plain title for a missing message selected by ID', () => {
    render(ReadingPane, {
      client: createAPIClient(vi.fn<typeof fetch>()),
      targetKey: 'recording:42001',
      status: 'missing',
      predicate: {},
    });
    expect(screen.getByRole('complementary', { name: 'Reading pane: Message' })).toBeDefined();
  });

  it.each([
    ['email', 'message', 'Email'],
    ['imessage', 'conversation', 'Conversation'],
    ['calendar', 'event', 'Calendar event'],
    ['meeting', 'meeting', 'Meeting']
  ])('labels %s (%s) entries %s in the meta strip', (messageType, kind, name) => {
    renderPane(entryRow({ message_type: messageType, kind, anchor_message_id: undefined }));
    const meta = document.querySelector('.pane-meta')?.textContent ?? '';
    expect(meta.startsWith(`${name} · `)).toBe(true);
    expect(meta).not.toContain(messageType === 'email' ? 'email ·' : messageType);
  });
  it('opens the header counterpart through onOpenPerson', async () => {
    const onOpenPerson = vi.fn();
    render(ReadingPane, {
      selection: { kind: 'entry', row: entryRow({ anchor_message_id: undefined, counterpart_participant_id: 22 }) },
      client: createAPIClient(vi.fn<typeof fetch>()),
      predicate: {}, onOpenPerson,
    });
    await fireEvent.click(screen.getByRole('button', { name: 'Open person' }));
    expect(onOpenPerson).toHaveBeenCalledWith(22);
  });
});

describe('ReadingPane conversation title', () => {
  it('shows a matching call summary once and updates the collapse control when the title changes', async () => {
    const summary = 'Incoming call with Example';
    const client = createAPIClient(vi.fn<typeof fetch>(async () => Response.json({
      id: 7, anchor_id: 42, messages: [{
        id: 42, conversation_id: 7, subject: summary, message_type: 'call_record',
        from: 'example@example.com', to: [], sent_at: '2026-07-18T12:00:00Z',
        snippet: 'Call preview', labels: [], has_attachments: false, size_bytes: 0,
        body: '', body_html: '', attachments: []
      }], has_before: false, has_after: false, total: 1
    })));
    const selection = { kind: 'entry' as const, row: entryRow({
      title: summary, message_type: 'call_record', conversation_id: 7
    }) };
    const view = render(ReadingPane, { client, selection, predicate: {} });
    const card = await screen.findByRole('article', { name: 'Message 42' });
    const collapse = within(card).getByRole('button', { name: 'Collapse message 42 from example@example.com' });
    expect(screen.getAllByText(summary)).toHaveLength(1);
    expect(collapse.textContent).toBe('');
    expect(collapse.querySelector('svg')).not.toBeNull();
    expect(collapse.getAttribute('aria-expanded')).toBe('true');

    await view.rerender({ selection: { ...selection, row: { ...selection.row, title: 'Different title' } } });
    expect(collapse.textContent).toBe(summary);
    await view.rerender({ selection });
    expect(collapse.textContent).toBe('');
    await fireEvent.click(collapse);
    const collapsed = screen.getByRole('button', { name: 'Expand message 42 from example@example.com' });
    expect(collapsed.textContent).toContain('Call preview');
    expect(collapsed.getAttribute('aria-expanded')).toBe('false');
  });
});

describe('ReadingPane identity matches', () => {
  it('shows via badges for email entries without replacing existing message metadata', () => {
    renderPane(entryRow({
      source_identifier: 'Original account header@example.test',
      matched_sender_identities: ['send-as@example.test'],
      matched_recipient_identities: ['masked@example.test']
    }));

    expect(screen.getByText(/Original account header@example\.test/)).toBeDefined();
    expect(screen.getByText('Sent via: send-as@example.test')).toBeDefined();
    expect(screen.getByText('Via: masked@example.test')).toBeDefined();
  });

  it('does not show via badges for non-email entries', () => {
    renderPane(entryRow({
      message_type: 'imessage',
      matched_sender_identities: ['hidden-non-email@example.test'],
      matched_recipient_identities: []
    }));

    expect(screen.queryByText('Sent via: hidden-non-email@example.test')).toBeNull();
  });

  it('shows via badges for legacy email entries with a blank message type', () => {
    renderPane(entryRow({
      message_type: '',
      matched_sender_identities: ['legacy-email@example.test'],
      matched_recipient_identities: []
    }));

    expect(screen.getByText('Sent via: legacy-email@example.test')).toBeDefined();
  });
});

describe('ReadingPane group file drill-down', () => {
  it.each(['participant', 'domain'] as const)('keeps %s files when meeting authority arrives', async dimension => {
    let fileRequests = 0;
    const client = createAPIClient(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path.endsWith('/explore/files')) {
        fileRequests += 1;
        return Response.json({ files: [{ key: 'file-1', filename: 'report.pdf', size: 128, occurred_at: '2026-07-18T12:00:00Z', title: 'Report', source_identifier: 'example.test' }], total_count: 1, cache_revision: 'cache-1', search_provenance: {} });
      }
      return Response.json(path.endsWith('/meetings/actions') ? meetingActions() : meetingMetrics());
    });
    const selection = { kind: 'group' as const, dimension, key: '42', label: 'Example' };
    const view = render(ReadingPane, { client, predicate: {}, selection });
    await screen.findByText('report.pdf');
    await view.rerender({ client, predicate: {}, selection: { ...selection, meetingScope: { predicate: {}, cache_revision: 'cache-1', search_provenance: {} } } });
    expect(fileRequests).toBe(1);
    expect(screen.getByText('report.pdf')).toBeDefined();
  });

  it('retries unavailable meeting authority', async () => {
    const onReloadMeetings = vi.fn();
    render(ReadingPane, { client: createAPIClient(async () => Response.json({ files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} })), predicate: {}, onReloadMeetings,
      selection: { kind: 'group', dimension: 'participant', key: '42', label: 'Example' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Reload meeting activity' }));
    expect(onReloadMeetings).toHaveBeenCalledOnce();
  });

  it('intersects a drilled-into participant group with an existing participant filter', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return Response.json({ files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} });
    });

    render(ReadingPane, {
      props: {
        client: createAPIClient(fetchFn),
        selection: { kind: 'group', dimension: 'participant', key: '99', label: 'Bob' },
        predicate: {
          filters: [{ dimension: 'participant', values: ['42'] }]
        } satisfies ExplorePredicate
      }
    });

    await waitFor(() => expect(requests).toHaveLength(1));
    const body = (await requests[0]!.clone().json()) as { predicate: { filters: ExploreFilter[] } };
    expect(body.predicate.filters).toEqual([
      { dimension: 'participant', values: ['42'] },
      { dimension: 'participant', values: ['99'] }
    ]);
  });
});

it('leads group details with the overview and a friendly grouping label', () => {
 render(ReadingPane, { client: createAPIClient(async () => Response.json({ files: [], total_count: 0, cache_revision: 'cache-1', search_provenance: {} })), predicate: {}, selection: {
  kind: 'group', dimension: 'participant', key: '7', label: 'Person Example', count: 3, latestAt: '2024-05-31T12:00:00Z',
  overview: { first_at: '2024-05-03T12:00:00Z', activity: [{ bucket: '2024-05', count: 3 }], co_participants: [] }
 } });
 expect(screen.getByText('Grouped by Person')).toBeDefined();
 expect(screen.getByLabelText('Group details').firstElementChild).toBe(screen.getByRole('region', { name: 'Group overview' }));
});
