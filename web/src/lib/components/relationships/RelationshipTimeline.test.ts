import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import type { RelationshipTimelineRow } from '../../relationships/models';
import RelationshipTimeline from './RelationshipTimeline.svelte';

function emailRow(key: string, occurredAt: string, title = key): RelationshipTimelineRow {
  return {
    key, kind: 'email', occurred_at: occurredAt, preview: `Preview for ${key}`,
    source_id: 1, title, has_attachments: false, message_count: 1,
    anchor_message_id: 90, conversation_id: 70
  };
}

function burstRow(key: string, occurredAt: string, messageCount: number): RelationshipTimelineRow {
  return {
    key, kind: 'chat_burst', occurred_at: occurredAt, first_at: occurredAt, preview: 'Latest chat snippet',
    source_id: 2, title: 'Team Chat', has_attachments: false, message_count: messageCount,
    anchor_message_id: 500, conversation_id: 71
  };
}

describe('RelationshipTimeline', () => {
  it('groups rows by month header and renders chat bursts as "N messages in <title>"', () => {
    render(RelationshipTimeline, {
      rows: [
        emailRow('message:1', '2026-07-18T12:00:00Z', 'July email'),
        burstRow('burst:1', '2026-07-05T12:00:00Z', 6),
        emailRow('message:2', '2026-06-20T12:00:00Z', 'June email')
      ],
      onRowOpen: vi.fn()
    });

    const grid = screen.getByRole('grid', { name: 'Relationship activity' });
    const headers = screen.getAllByRole('gridcell', { name: /^Month: / });
    expect(headers).toHaveLength(2);
    expect(headers[0]!.closest('[role="row"]')).toBe(grid.querySelector('.month-header'));
    expect(screen.getByText('6 messages in Team Chat')).toBeDefined();
    expect(screen.getByText('July email')).toBeDefined();
    expect(screen.getByText('June email')).toBeDefined();
  });

  it('shows a selection ring on the row matching selectedKey', () => {
    render(RelationshipTimeline, {
      rows: [emailRow('message:1', '2026-07-18T12:00:00Z'), emailRow('message:2', '2026-07-17T12:00:00Z')],
      selectedKey: 'message:2',
      onRowOpen: vi.fn()
    });

    const selectedRow = document.querySelector('[data-row-key="message:2"]');
    expect(selectedRow?.classList.contains('selected')).toBe(true);
    expect(document.querySelector('[data-row-key="message:1"]')?.classList.contains('selected')).toBe(false);
  });

  it('moves the keyboard cursor with j/k and opens the active row with Enter', async () => {
    const onRowOpen = vi.fn();
    render(RelationshipTimeline, {
      rows: [emailRow('message:1', '2026-07-18T12:00:00Z'), emailRow('message:2', '2026-07-17T12:00:00Z')],
      onRowOpen
    });
    const grid = screen.getByRole('grid', { name: 'Relationship activity' });
    grid.focus();

    await fireEvent.keyDown(grid, { key: 'j' });
    await fireEvent.keyDown(grid, { key: 'Enter' });
    expect(onRowOpen).toHaveBeenCalledWith(expect.objectContaining({ key: 'message:2' }));
  });

  it('opens a row with a single click', async () => {
    const onRowOpen = vi.fn();
    render(RelationshipTimeline, {
      rows: [emailRow('message:1', '2026-07-18T12:00:00Z', 'Click target')],
      onRowOpen
    });

    await fireEvent.click(screen.getByText('Click target').closest('[role="row"]')!);
    expect(onRowOpen).toHaveBeenCalledOnce();
    expect(onRowOpen).toHaveBeenCalledWith(expect.objectContaining({ key: 'message:1' }));
  });

  it('requests more rows once scroll nears the loaded end, but never on End alone', async () => {
    const onLoadMore = vi.fn();
    render(RelationshipTimeline, {
      rows: [emailRow('message:1', '2026-07-18T12:00:00Z')],
      hasMore: true,
      onLoadMore,
      onRowOpen: vi.fn()
    });
    const grid = screen.getByRole('grid', { name: 'Relationship activity' });
    grid.focus();

    await fireEvent.keyDown(grid, { key: 'End' });
    expect(onLoadMore).not.toHaveBeenCalled();

    await fireEvent.scroll(grid);
    await waitFor(() => expect(onLoadMore).toHaveBeenCalled());
  });

  it('does not request more rows while a page is already loading', async () => {
    const onLoadMore = vi.fn();
    render(RelationshipTimeline, {
      rows: [emailRow('message:1', '2026-07-18T12:00:00Z')],
      hasMore: true,
      loadingMore: true,
      onLoadMore,
      onRowOpen: vi.fn()
    });

    await fireEvent.scroll(screen.getByRole('grid', { name: 'Relationship activity' }));
    expect(onLoadMore).not.toHaveBeenCalled();
    expect(screen.getByText('Loading more…')).toBeDefined();
  });

  it('offers a retry action on a pagination error while the cursor survived', async () => {
    const onLoadMore = vi.fn();
    render(RelationshipTimeline, {
      rows: [emailRow('message:1', '2026-07-18T12:00:00Z')],
      error: 'boom',
      hasMore: true,
      onLoadMore,
      onRowOpen: vi.fn()
    });

    expect(screen.getByRole('alert').textContent).toContain('boom');
    await fireEvent.click(screen.getByRole('button', { name: 'Retry loading more' }));
    expect(onLoadMore).toHaveBeenCalledOnce();
  });

  it('hides the retry action for a terminal timeline error (no cursor left)', () => {
    render(RelationshipTimeline, {
      rows: [emailRow('message:1', '2026-07-18T12:00:00Z')],
      error: 'Pagination stopped because the server repeated a cursor without progress.',
      hasMore: false,
      onRowOpen: vi.fn()
    });

    expect(screen.getByRole('alert')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Retry loading more' })).toBeNull();
  });

  it('shows the 409-restart notice as a persistent one-line status', () => {
    render(RelationshipTimeline, {
      rows: [emailRow('message:1', '2026-07-18T12:00:00Z')],
      restartNotice: 'Timeline restarted: the archive changed.',
      onRowOpen: vi.fn()
    });

    expect(screen.getByText('Timeline restarted: the archive changed.')).toBeDefined();
  });

  it('shows an empty state when there is no activity', () => {
    render(RelationshipTimeline, { rows: [], onRowOpen: vi.fn() });
    expect(screen.getByText('No activity yet')).toBeDefined();
  });
});
