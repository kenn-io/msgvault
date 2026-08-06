import { render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
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
