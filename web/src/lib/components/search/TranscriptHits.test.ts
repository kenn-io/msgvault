import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import TranscriptHits from './TranscriptHits.svelte';

function hit(overrides: Record<string, unknown> = {}) {
  return { message_id: 9, conversation_id: 2, attachment_id: 1, filename: 'voice.wav', containing_title: 'Quarterly review',
    occurred_at: '2026-07-18T12:00:00Z', origin: 'supplied', excerpt: 'Quarterly <numbers>', ...overrides };
}

function report(overrides: Record<string, unknown> = {}) {
  return { coverage: { state: 'complete', binding_required: false, complete_documents: 1, scoped_documents: 1 },
    results: [hit()], partial: false, truncated: false, pending_occurrences: 0, unavailable_occurrences: 0,
    attribution_unavailable: 0, ...overrides };
}

function mount(fetchFn = vi.fn<typeof fetch>(async () => Response.json(report()))) {
  const client = createAPIClient(fetchFn);
  return { client, fetchFn, ...render(TranscriptHits, { props: { client, query: 'quarterly', supported: true } }) };
}

afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); });

describe('TranscriptHits', () => {
  it('requests lexical evidence and preserves source occurrences, provenance and optional timing', async () => {
    const { fetchFn } = mount(vi.fn<typeof fetch>(async () => Response.json(report({ results: [
      hit({ start_ms: 0, end_ms: 2800 }), hit({ message_id: 10, origin: 'generated', filename: 'other.wav' }),
    ] }))));
    const links = await screen.findAllByRole('link');
    expect(links.map(link => link.getAttribute('href'))).toEqual(['/messages/9', '/messages/10']);
    expect(links[0].textContent).toContain('Quarterly review');
    expect(links[0].textContent).toContain('voice.wav');
    expect(links[0].textContent).toContain('Provider transcript');
    expect(links[0].textContent).toContain('0:00 to 0:02');
    expect(links[0].textContent).toContain('Quarterly <numbers>');
    expect(links[0].querySelector('numbers')).toBeNull();
    expect(links[0].querySelector('time')?.getAttribute('datetime')).toBe('2026-07-18T12:00:00Z');
    expect(links[1].textContent).toContain('Generated transcript');
    expect(links[1].querySelectorAll('time')).toHaveLength(1);
    const url = new URL((fetchFn.mock.calls[0][0] as Request).url);
    expect(url.pathname).toBe('/api/v1/media/search');
    expect(Object.fromEntries(url.searchParams)).toEqual({ q: 'quarterly', mode: 'lexical', limit: '20' });
    expect(fetchFn).toHaveBeenCalledTimes(1);
  });

  it('keeps every incomplete zero-hit coverage state visible', async () => {
    mount(vi.fn<typeof fetch>(async () => Response.json(report({ results: [], partial: true, truncated: true,
      pending_occurrences: 2, unavailable_occurrences: 3, attribution_unavailable: 1 }))));
    await screen.findByText(/Coverage incomplete\./);
    expect(screen.getByRole('status').textContent).toContain('2 pending.');
    expect(screen.getByRole('status').textContent).toContain('3 unavailable.');
    expect(screen.getByRole('status').textContent).toContain('1 with unavailable attribution.');
    expect(screen.getByRole('status').textContent).toContain('More matches may exist.');
    expect(screen.getByText('No matching excerpts returned.')).toBeTruthy();
    expect(screen.queryByText('No spoken matches.')).toBeNull();
  });

  it('restricts unsupported scopes and clears the old query before ignoring its delayed response', async () => {
    let finish!: (value: Response) => void;
    const fetchFn = vi.fn<typeof fetch>().mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }))
      .mockResolvedValue(Response.json(report({ results: [hit({ excerpt: 'current evidence' })] })));
    const view = mount(fetchFn);
    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(1));
    const old = fetchFn.mock.calls[0][0] as Request;
    await view.rerender({ client: view.client, query: 'current', supported: true });
    expect(old.signal.aborted).toBe(true);
    await screen.findByText('current evidence');
    finish(Response.json(report({ results: [hit({ excerpt: 'obsolete evidence' })] })));
    await Promise.resolve();
    expect(screen.queryByText('obsolete evidence')).toBeNull();
    await view.rerender({ client: view.client, query: 'current', supported: false });
    expect(screen.queryByText('current evidence')).toBeNull();
    expect(screen.getByRole('status').textContent).toContain('Full text with no filters or grouping');
    expect(fetchFn).toHaveBeenCalledTimes(2);
  });

  it('clears stale excerpts after a failed refresh and allows retry', async () => {
    vi.useFakeTimers();
    const fetchFn = vi.fn<typeof fetch>().mockResolvedValueOnce(Response.json(report()))
      .mockRejectedValueOnce(new Error('connection lost'))
      .mockResolvedValue(Response.json(report({ results: [] })));
    const view = mount(fetchFn);
    await vi.advanceTimersByTimeAsync(300);
    expect(screen.getByText('Quarterly <numbers>')).toBeTruthy();
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    expect(screen.queryByText('Quarterly <numbers>')).toBeNull();
    await vi.advanceTimersByTimeAsync(300);
    expect(screen.getByText('Could not load recording matches.')).toBeTruthy();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await vi.advanceTimersByTimeAsync(300);
    expect(screen.getByText('No spoken matches.')).toBeTruthy();
    view.unmount();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(fetchFn).toHaveBeenCalledTimes(3);
  });

  it('refreshes removed evidence on focus and pauses while hidden', async () => {
    vi.useFakeTimers();
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
    let finish!: (value: Response) => void;
    const fetchFn = vi.fn<typeof fetch>().mockResolvedValueOnce(Response.json(report()))
      .mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }))
      .mockResolvedValue(Response.json(report({ results: [] })));
    const view = mount(fetchFn);
    await vi.advanceTimersByTimeAsync(300);
    await fireEvent(window, new Event('focus'));
    await fireEvent(window, new Event('focus'));
    await fireEvent(document, new Event('visibilitychange'));
    expect(screen.queryByText('Quarterly <numbers>')).toBeNull();
    expect(fetchFn).toHaveBeenCalledTimes(2);
    finish(Response.json(report({ results: [] })));
    await vi.advanceTimersByTimeAsync(0);
    hidden.mockReturnValue(true);
    await fireEvent(document, new Event('visibilitychange'));
    await vi.advanceTimersByTimeAsync(60_000);
    expect(fetchFn).toHaveBeenCalledTimes(2);
    hidden.mockReturnValue(false);
    await fireEvent(document, new Event('visibilitychange'));
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchFn).toHaveBeenCalledTimes(3);
    view.unmount();
  });

  it('expires evidence without polling and manually refreshes it', async () => {
    vi.useFakeTimers();
    const view = mount();
    await vi.advanceTimersByTimeAsync(300);
    await vi.advanceTimersByTimeAsync(30_000);
    expect(screen.queryByText('Quarterly <numbers>')).toBeNull();
    expect(screen.getByText('Recording results expired.')).toBeTruthy();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(view.fetchFn).toHaveBeenCalledTimes(1);
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    await vi.advanceTimersByTimeAsync(300);
    expect(screen.getByText('Quarterly <numbers>')).toBeTruthy();
    expect(view.fetchFn).toHaveBeenCalledTimes(2);
    view.unmount();
  });

  it('counts request time toward expiry and ignores an expired slow response', async () => {
    vi.useFakeTimers();
    let finish!: (value: Response) => void;
    const fetchFn = vi.fn<typeof fetch>(() => new Promise(resolve => { finish = resolve; }));
    const view = mount(fetchFn);
    await vi.advanceTimersByTimeAsync(300);
    const request = fetchFn.mock.calls[0][0] as Request;
    await vi.advanceTimersByTimeAsync(30_000);
    expect(request.signal.aborted).toBe(true);
    finish(Response.json(report()));
    await vi.advanceTimersByTimeAsync(0);
    expect(screen.getByText('Recording results expired.')).toBeTruthy();
    expect(screen.queryByRole('link')).toBeNull();
    expect(fetchFn).toHaveBeenCalledTimes(1);
    view.unmount();
  });

  it.each(['quarterly numbers', 'café 東京 2026', 'cafe\u0301'])('admits plain Unicode words in %s', async query => {
    const view = mount();
    await view.rerender({ client: view.client, query, supported: true });
    await screen.findByText('Quarterly <numbers>');
    expect(new URL((view.fetchFn.mock.calls.at(-1)![0] as Request).url).searchParams.get('q')).toBe(query);
  });

  it.each(['from:alice@example.com quarterly', 'after:2026-01-01 numbers', '"quarterly numbers"', 'quarterly OR numbers', 'AND', 'not', 'quarterly*', '\u0301'])('rejects unsupported syntax %s without searching', async query => {
    const fetchFn = vi.fn<typeof fetch>();
    render(TranscriptHits, { props: { client: createAPIClient(fetchFn), query, supported: true } });
    await screen.findByText(/Recording search supports plain words only/);
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it.each([
    [503, 'media_search_unavailable', 'Recording search unavailable.'],
    [400, 'media_search_scope_limit', 'This archive exceeds browser recording-search limits. Use person-scoped recording search in the CLI or API.'],
    [400, 'invalid_media_search', 'Recording search rejected this query. Try different plain words.'],
    [403, 'forbidden', 'Recording search cannot read this query. Check archive access or change the query.'],
  ])('keeps %s %s terminal until the query changes', async (status, error, text) => {
    vi.useFakeTimers();
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ error }, { status }));
    const view = mount(fetchFn);
    await vi.advanceTimersByTimeAsync(300);
    expect(screen.getByText(text)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Refresh' })).toBeNull();
    await fireEvent(window, new Event('focus'));
    await vi.advanceTimersByTimeAsync(90_000);
    expect(fetchFn).toHaveBeenCalledTimes(1);
    await view.rerender({ client: view.client, query: 'different', supported: true });
    await vi.advanceTimersByTimeAsync(300);
    expect(fetchFn).toHaveBeenCalledTimes(2);
    view.unmount();
  });

});
