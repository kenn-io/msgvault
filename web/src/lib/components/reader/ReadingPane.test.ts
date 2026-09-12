import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
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

describe('ReadingPane meeting evidence', () => {
  it('uses only an exact meeting transcript anchor for context and archived actions', async () => {
    const requests: Request[] = [];
    const createObjectURL = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:reader-meeting-context');
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/actions')) {
        return Response.json({
          schema_version: 1,
          archive_uid: 'archive-test',
          rows: [],
          total_count: 0,
          coverage: { meeting_count: 1, available: 1, partial: 0, unsupported: 0, unavailable: 0 },
          scope: { kind: 'direct' },
        });
      }
      return Response.json({
        schema_version: 1,
        format: 'json',
        content: '{"meeting":42}',
        content_bytes: 14,
        truncated: false,
        omitted_message_ids: [],
      });
    });
    render(ReadingPane, {
      client: createAPIClient(fetchFn),
      selection: {
        kind: 'entry',
        row: entryRow({
          message_type: 'meeting_transcript',
          anchor_message_id: 42,
          conversation_id: undefined,
        }),
      },
      predicate: {},
    });

    expect(await screen.findByText('No recorded action items')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Export meeting context' }));
    await waitFor(() => expect(createObjectURL).toHaveBeenCalledOnce());
    const bodies = await Promise.all(requests.map((request) => request.clone().json()));
    expect(bodies).toContainEqual({ scope: { message_ids: [42] }, limit: 200 });
    expect(bodies).toContainEqual({ message_ids: [42], format: 'json', include_transcript: false });
  });

  it.each(['meeting_notes', 'email'])('does not treat %s as a meeting transcript', (messageType) => {
    const fetchFn = vi.fn<typeof fetch>();
    render(ReadingPane, {
      client: createAPIClient(fetchFn),
      selection: {
        kind: 'entry',
        row: entryRow({ message_type: messageType, anchor_message_id: 42, conversation_id: undefined }),
      },
      predicate: {},
    });

    expect(screen.queryByRole('region', { name: 'Meeting context export' })).toBeNull();
    expect(screen.queryByText('Archived action items')).toBeNull();
    expect(fetchFn).not.toHaveBeenCalled();
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
