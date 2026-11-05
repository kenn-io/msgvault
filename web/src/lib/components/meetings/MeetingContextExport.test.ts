import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { ExploreSelection, MeetingContextRequest } from '../../api/generated/models';
import MeetingContextExport from './MeetingContextExport.svelte';

const selection: ExploreSelection = {
  mode: 'explicit',
  predicate: {
    query: 'planning',
    search_mode: 'full_text',
    filters: [],
    presentation: 'table',
  },
  row_keys: ['message:7', 'message:91'],
  cache_revision: 'cache-7',
  search_provenance: { lexical_index_revision: 'fts-4' },
};

describe('MeetingContextExport', () => {
  let createObjectURL: ReturnType<typeof vi.spyOn>;
  let revokeObjectURL: ReturnType<typeof vi.spyOn>;
  let anchorClick: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    createObjectURL = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:meeting-context');
    revokeObjectURL = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    anchorClick = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
  });

  afterEach(() => vi.restoreAllMocks());

  it('downloads exact Unicode content with transcript omitted by default and revokes the Blob URL', async () => {
    const requests: Request[] = [];
    const content = '{"title":"Café ☕","notes":"Привет 世界"}';
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      requests.push(input instanceof Request ? input : new Request(input));
      return Response.json({
        schema_version: 1,
        format: 'json',
        content,
        content_bytes: new TextEncoder().encode(content).byteLength,
        truncated: false,
        omitted_message_ids: [],
      });
    });
    render(MeetingContextExport, {
      client: createAPIClient(fetchFn),
      request: { selection } satisfies MeetingContextRequest,
    });

    expect(
      (
        screen.getByRole('checkbox', {
          name: 'Include transcript',
        }) as HTMLInputElement
      ).checked,
    ).toBe(false);
    await fireEvent.click(screen.getByRole('button', { name: 'Export meeting context' }));

    await waitFor(() => expect(createObjectURL).toHaveBeenCalledOnce());
    await expect(requests[0]!.clone().json()).resolves.toEqual({
      selection,
      format: 'json',
      include_transcript: false,
    });
    const blob = createObjectURL.mock.calls[0]![0] as Blob;
    expect(blob.type).toBe('application/json');
    await expect(blob.text()).resolves.toBe(content);
    expect((anchorClick.mock.instances[0] as HTMLAnchorElement).download).toBe('meeting-context.json');
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:meeting-context');
  });

  it('offers Markdown and reports explicit server truncation and omissions beside the download', async () => {
    const content = '# Café\n\nCoverage: partial\n';
    const fetchFn = vi.fn<typeof fetch>(async () =>
      Response.json({
        schema_version: 1,
        format: 'markdown',
        content,
        content_bytes: new TextEncoder().encode(content).byteLength,
        truncated: true,
        omitted_message_ids: [91, 104],
      }),
    );
    render(MeetingContextExport, {
      client: createAPIClient(fetchFn),
      request: { selection } satisfies MeetingContextRequest,
    });
    anchorClick.mockImplementationOnce(() => {
      expect(screen.getByText(/Export was truncated/)).toBeDefined();
      expect(screen.getByText(/91, 104/)).toBeDefined();
    });

    await fireEvent.click(screen.getByRole('radio', { name: 'Markdown' }));
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Include transcript' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Export meeting context' }));

    expect(await screen.findByText(/Export was truncated/)).toBeDefined();
    expect(screen.getByText(/91, 104/)).toBeDefined();
    await waitFor(() => expect(createObjectURL).toHaveBeenCalledOnce());
    const blob = createObjectURL.mock.calls[0]![0] as Blob;
    expect(blob.type).toBe('text/markdown');
    await expect(blob.text()).resolves.toBe(content);
    expect((anchorClick.mock.instances[0] as HTMLAnchorElement).download).toBe('meeting-context.md');
  });

  it('labels a delayed download with the format submitted before the control changes', async () => {
    let resolveResponse!: (response: Response) => void;
    let submittedRequest!: Request;
    const content = '{"format":"json","title":"Caf\u00e9 \u2615"}';
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      submittedRequest = input instanceof Request ? input : new Request(input);
      return new Promise<Response>((resolve) => {
        resolveResponse = resolve;
      });
    });
    render(MeetingContextExport, {
      client: createAPIClient(fetchFn),
      request: { selection } satisfies MeetingContextRequest,
    });

    await fireEvent.click(screen.getByRole('button', { name: 'Export meeting context' }));
    await waitFor(() => expect(resolveResponse).toBeTypeOf('function'));
    await fireEvent.click(screen.getByRole('radio', { name: 'Markdown' }));
    resolveResponse(
      Response.json({
        schema_version: 1,
        format: 'json',
        content,
        content_bytes: new TextEncoder().encode(content).byteLength,
        truncated: false,
        omitted_message_ids: [],
      }),
    );

    await waitFor(() => expect(createObjectURL).toHaveBeenCalledOnce());
    await expect(submittedRequest.clone().json()).resolves.toMatchObject({ format: 'json' });
    const blob = createObjectURL.mock.calls[0]![0] as Blob;
    expect(blob.type).toBe('application/json');
    await expect(blob.text()).resolves.toBe(content);
    expect((anchorClick.mock.instances[0] as HTMLAnchorElement).download).toBe('meeting-context.json');
  });

  it.each([
    ['selection_not_all_meetings', 400, 'Every row must be a meeting', 'Select meetings only'],
    [
      'meeting_selection_too_large',
      400,
      'Meeting context accepts at most 100 meetings',
      'Meeting context accepts at most 100 meetings',
    ],
    [
      'not_found',
      404,
      'No route matches POST /api/v1/meetings/context',
      'Meeting context export is unavailable with this daemon',
    ],
    ['meeting_not_found', 404, 'Meeting 91 was deleted from this archive.', 'Meeting 91 was deleted from this archive.'],
  ])('shows actionable state for %s', async (code, status, message, expected) => {
    render(MeetingContextExport, {
      client: createAPIClient(async () => Response.json({ error: code, message }, { status })),
      request: { selection } satisfies MeetingContextRequest,
    });

    await fireEvent.click(screen.getByRole('button', { name: 'Export meeting context' }));

    expect((await screen.findByRole('alert')).textContent).toContain(expected);
    expect(createObjectURL).not.toHaveBeenCalled();
  });

  it('explains a known over-limit selection without sending it', () => {
    const fetchFn = vi.fn<typeof fetch>();
    render(MeetingContextExport, {
      client: createAPIClient(fetchFn),
      request: { selection } satisfies MeetingContextRequest,
      disabledReason: 'Meeting context accepts at most 100 meetings.',
    });

    expect(screen.getByText('Meeting context accepts at most 100 meetings.')).toBeDefined();
    expect(
      (
        screen.getByRole('button', {
          name: 'Export meeting context',
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it('aborts and discards a completed export after its selection authority changes', async () => {
    let resolveFirst!: (response: Response) => void;
    let firstRequest!: Request;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      firstRequest = input instanceof Request ? input : new Request(input);
      return new Promise<Response>((resolve) => {
        resolveFirst = resolve;
      });
    });
    const request = { selection } satisfies MeetingContextRequest;
    const view = render(MeetingContextExport, {
      client: createAPIClient(fetchFn),
      request,
    });
    await fireEvent.click(screen.getByRole('button', { name: 'Export meeting context' }));
    await waitFor(() => expect(firstRequest).toBeInstanceOf(Request));

    await view.rerender({
      client: createAPIClient(fetchFn),
      request: { selection: { ...selection, row_keys: ['message:104'] } },
    });
    expect(firstRequest.signal.aborted).toBe(true);
    resolveFirst(
      Response.json({
        schema_version: 1,
        format: 'json',
        content: '{"stale":true}',
        content_bytes: 14,
        truncated: false,
        omitted_message_ids: [],
      }),
    );
    await Promise.resolve();
    await Promise.resolve();

    expect(createObjectURL).not.toHaveBeenCalled();
  });
});
