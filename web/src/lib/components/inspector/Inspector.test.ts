import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { EntryRow } from '../../explore/models';
import Inspector from './Inspector.svelte';

function entry(key = 'message:1'): EntryRow {
  return {
    key,
    kind: 'message',
    message_type: 'email',
    conversation_type: 'email',
    title: 'Synthetic subject',
    preview: 'Synthetic excerpt',
    occurred_at: '2026-07-18T12:00:00Z',
    source_id: 1,
    source_identifier: 'archive@example.com',
    source_type: 'synthetic',
    participant_labels: ['Example Person'],
    participant_ids: [42],
    counterpart_participant_id: 99,
    attachment_count: 1,
    attachment_size: 42,
    has_attachments: true,
    deleted_from_source: false,
    message_count: 1,
    anchor_message_id: 42,
    conversation_id: 7,
    match: {}
  };
}

describe('Inspector', () => {
  it('shows a concrete row in the pinned side panel and exposes close', async () => {
    const onClose = vi.fn();
    render(Inspector, {
      client: createAPIClient(vi.fn()),
      selection: { kind: 'entry', row: entry() },
      predicate: { filters: [], presentation: 'table' },
      width: 380,
      onClose
    });

    const aside = screen.getByRole('complementary', { name: 'Inspect Synthetic subject' });
    expect(within(aside).getByText('Synthetic excerpt')).toBeDefined();
    expect(document.querySelector('.kit-detail-drawer-overlay')).toBeNull();
    await fireEvent.click(within(aside).getByRole('button', { name: 'Close inspector' }));
    expect(onClose).toHaveBeenCalledOnce();
  });

  it('opens the entry\'s server-computed counterpart into the Relationships hub from the header action', async () => {
    const onOpenRelationship = vi.fn();
    render(Inspector, {
      client: createAPIClient(vi.fn()),
      selection: { kind: 'entry', row: entry() },
      predicate: { filters: [], presentation: 'table' },
      width: 380,
      onOpenRelationship
    });

    await fireEvent.click(screen.getByRole('button', { name: 'Open relationship' }));
    // counterpart_participant_id (99) is asserted, not participant_ids[0]
    // (42) — the entry fixture deliberately sets them to different values so
    // this test fails if the component regresses to the old
    // participant_ids[0] heuristic, which frequently resolved to the
    // archive owner rather than the other side of the conversation.
    expect(onOpenRelationship).toHaveBeenCalledWith(99);
  });

  it('hides the Open relationship action when the caller has no destination for it, or the entry carries no counterpart', async () => {
    render(Inspector, {
      client: createAPIClient(vi.fn()),
      selection: { kind: 'entry', row: entry() },
      predicate: { filters: [], presentation: 'table' },
      width: 380
    });
    expect(screen.queryByRole('button', { name: 'Open relationship' })).toBeNull();

    const onOpenRelationship = vi.fn();
    render(Inspector, {
      client: createAPIClient(vi.fn()),
      selection: { kind: 'entry', row: { ...entry(), counterpart_participant_id: undefined } },
      predicate: { filters: [], presentation: 'table' },
      width: 380,
      onOpenRelationship
    });
    expect(screen.queryByRole('button', { name: 'Open relationship' })).toBeNull();
  });

  it('shows selected group details and chronological files from one bounded request', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return Response.json({
        files: [
          {
            key: 'file:new', entry_key: 'message:2', message_id: 2,
            occurred_at: '2026-07-18T12:00:00Z', source_id: 1,
            source_identifier: 'archive@example.com', title: 'Newer entry',
            filename: 'new.txt', size: 20
          },
          {
            key: 'file:old', entry_key: 'message:1', message_id: 1,
            occurred_at: '2026-07-17T12:00:00Z', source_id: 1,
            source_identifier: 'archive@example.com', title: 'Older entry',
            filename: 'old.txt', size: 10
          }
        ],
        total_count: 3, cache_revision: 'cache-1', search_provenance: {}
      });
    });
    render(Inspector, {
      client: createAPIClient(fetchFn),
      selection: {
        kind: 'group', dimension: 'participant', key: '42', label: 'Example Person',
        count: 12, estimatedBytes: 30, latestAt: '2026-07-18T12:00:00Z'
      },
      predicate: { query: 'pasta', search_mode: 'semantic', filters: [], presentation: 'table' },
      width: 380
    });

    await screen.findByText('new.txt');
    const facts = screen.getAllByRole('listitem').map((item) => item.textContent);
    expect(facts[0]).toContain('old.txt');
    expect(facts[1]).toContain('new.txt');
    expect(screen.getByText('Showing a bounded sample of 2 of 3 files.')).toBeDefined();
    expect(screen.queryByText(/Open Files/)).toBeNull();
    expect(screen.getByText('12 items')).toBeDefined();
    expect(requests).toHaveLength(1);
    await expect(requests[0]!.clone().json()).resolves.toMatchObject({
      limit: 100,
      predicate: {
        query: 'pasta', search_mode: 'semantic',
        filters: [{ dimension: 'participant', values: ['42'] }],
        presentation: 'table'
      }
    });
    expect((await requests[0]!.clone().json()).predicate).not.toHaveProperty('grouping');
  });

  it('aborts superseded file facts and ignores a stale response that still settles', async () => {
    const signals: AbortSignal[] = [];
    const resolvers: Array<(response: Response) => void> = [];
    const fetchFn = vi.fn<typeof fetch>((input) => {
      const request = input instanceof Request ? input : new Request(input);
      signals.push(request.signal);
      return new Promise<Response>((resolve) => resolvers.push(resolve));
    });
    const rendered = render(Inspector, {
      client: createAPIClient(fetchFn),
      selection: { kind: 'group', dimension: 'participant', key: '1', label: 'First person' },
      predicate: { filters: [], presentation: 'table' },
      width: 380
    });
    await waitFor(() => expect(signals).toHaveLength(1));

    await rendered.rerender({
      client: createAPIClient(fetchFn),
      selection: { kind: 'group', dimension: 'domain', key: 'example.com', label: 'example.com' },
      predicate: { filters: [], presentation: 'table' },
      width: 380
    });
    await waitFor(() => expect(signals).toHaveLength(2));
    expect(signals[0]!.aborted).toBe(true);

    resolvers[1]!(Response.json({
      files: [{
        key: 'file:second', entry_key: 'message:2', message_id: 2,
        occurred_at: '2026-07-18T12:00:00Z', source_id: 1,
        source_identifier: 'archive@example.com', title: 'Second entry',
        filename: 'second.txt', size: 20
      }],
      total_count: 1, cache_revision: 'cache-2', search_provenance: {}
    }));
    expect(await screen.findByText('second.txt')).toBeDefined();

    resolvers[0]!(Response.json({
      files: [{
        key: 'file:first', entry_key: 'message:1', message_id: 1,
        occurred_at: '2026-07-17T12:00:00Z', source_id: 1,
        source_identifier: 'archive@example.com', title: 'First entry',
        filename: 'stale.txt', size: 10
      }],
      total_count: 1, cache_revision: 'cache-1', search_provenance: {}
    }));
    await Promise.resolve();
    expect(screen.queryByText('stale.txt')).toBeNull();
  });

  it('resizes a pinned inspector through the kit handle', async () => {
    const onWidthChange = vi.fn();
    render(Inspector, {
      client: createAPIClient(vi.fn()),
      selection: { kind: 'entry', row: entry() },
      predicate: { filters: [], presentation: 'table' },
      width: 400,
      onWidthChange
    });

    await fireEvent.keyDown(screen.getByRole('button', { name: 'Resize inspector' }), {
      key: 'ArrowLeft'
    });
    expect(onWidthChange).toHaveBeenCalledWith(424);
  });

  it('enters and exits an explicit content-focus mode', async () => {
    const onContentFocusChange = vi.fn();
    render(Inspector, {
      client: createAPIClient(vi.fn()),
      selection: { kind: 'entry', row: entry() },
      predicate: { filters: [], presentation: 'table' },
      width: 400,
      onContentFocusChange
    });

    await fireEvent.click(screen.getByRole('button', { name: 'Focus inspector content' }));
    const content = screen.getByRole('region', { name: 'Inspector content' });
    expect(document.activeElement).toBe(content);
    expect(onContentFocusChange).toHaveBeenCalledWith(true);

    await fireEvent.keyDown(content, { key: 'Escape' });
    expect(onContentFocusChange).toHaveBeenLastCalledWith(false);
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Focus inspector content' }));
  });

  it('opens the containing conversation from an email or chat row', async () => {
    const onViewConversation = vi.fn();
    const rendered = render(Inspector, {
      client: createAPIClient(vi.fn()),
      selection: { kind: 'entry', row: entry() },
      predicate: { filters: [], presentation: 'table' },
      width: 400,
      onViewConversation
    });

    await fireEvent.click(screen.getByRole('button', { name: 'View conversation' }));
    expect(onViewConversation).toHaveBeenCalledWith(7, 42);

    await rendered.rerender({
      client: createAPIClient(vi.fn()),
      selection: { kind: 'entry', row: { ...entry(), kind: 'conversation', message_type: 'imessage' } },
      predicate: { filters: [], presentation: 'table' },
      width: 400,
      onViewConversation
    });
    await fireEvent.click(screen.getByRole('button', { name: 'View conversation' }));
    expect(onViewConversation).toHaveBeenLastCalledWith(7, 42);
  });

  it('threads optional conversation start/end bounds to the conversation fetch', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return Response.json({ id: 7, anchor_id: 42, messages: [], has_before: false, has_after: false, total: 0 });
    });
    render(Inspector, {
      client: createAPIClient(fetchFn),
      selection: { kind: 'entry', row: entry() },
      predicate: { filters: [], presentation: 'table' },
      width: 400,
      conversationAnchorId: 42,
      conversationStart: '2026-07-19T00:00:00.000Z',
      conversationEnd: '2026-07-20T00:00:00.000Z'
    });

    await waitFor(() => expect(requests).toHaveLength(1));
    const url = new URL(requests[0]!.url);
    expect(url.searchParams.get('start')).toBe('2026-07-19T00:00:00.000Z');
    expect(url.searchParams.get('end')).toBe('2026-07-20T00:00:00.000Z');
  });
});
