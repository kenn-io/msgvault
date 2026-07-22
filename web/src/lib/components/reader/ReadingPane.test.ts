import { render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { EntryRow, ExplorePredicate } from '../../explore/models';
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

describe('ReadingPane task gating', () => {
  it('offers Tasks for a typed email entry', () => {
    renderPane(entryRow());
    expect(screen.getByLabelText('Tasks for this message')).toBeDefined();
  });

  it('offers Tasks for a legacy entry with a blank message type', () => {
    renderPane(entryRow({ message_type: '' }));
    expect(screen.getByLabelText('Tasks for this message')).toBeDefined();
  });

  it('hides Tasks for non-email entries', () => {
    renderPane(entryRow({ message_type: 'imessage' }));
    expect(screen.queryByLabelText('Tasks for this message')).toBeNull();
  });

  it('hides Tasks when the entry has no anchor message', () => {
    renderPane(entryRow({ anchor_message_id: undefined }));
    expect(screen.queryByLabelText('Tasks for this message')).toBeNull();
  });
});
