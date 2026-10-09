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
      .mockResolvedValueOnce(Response.json({ error: 'unavailable' }, { status: 503 }))
      .mockResolvedValue(Response.json(report({ results: [] })));
    const view = mount(fetchFn);
    await vi.advanceTimersByTimeAsync(0);
    expect(screen.getByText('Quarterly <numbers>')).toBeTruthy();
    await vi.advanceTimersByTimeAsync(2000);
    expect(screen.queryByText('Quarterly <numbers>')).toBeNull();
    expect(screen.getByText('Recording search unavailable.')).toBeTruthy();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await vi.advanceTimersByTimeAsync(0);
    expect(screen.getByText('No spoken matches.')).toBeTruthy();
    view.unmount();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(fetchFn).toHaveBeenCalledTimes(3);
  });

  it('refreshes removed evidence on focus and pauses while hidden', async () => {
    vi.useFakeTimers();
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
    const fetchFn = vi.fn<typeof fetch>().mockResolvedValueOnce(Response.json(report()))
      .mockResolvedValue(Response.json(report({ results: [] })));
    const view = mount(fetchFn);
    await vi.advanceTimersByTimeAsync(0);
    await fireEvent(window, new Event('focus'));
    await vi.advanceTimersByTimeAsync(0);
    expect(screen.queryByText('Quarterly <numbers>')).toBeNull();
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

  it('bounds a hanging read and clears unavailable evidence', async () => {
    const deadline = new AbortController();
    vi.spyOn(AbortSignal, 'timeout').mockReturnValue(deadline.signal);
    const fetchFn = vi.fn<typeof fetch>((input) => new Promise((_resolve, reject) => {
      (input as Request).signal.addEventListener('abort', () => reject(new Error('aborted')));
    }));
    const view = mount(fetchFn);
    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(1));
    deadline.abort();
    expect(await screen.findByText('Recording search unavailable.')).toBeTruthy();
    view.unmount();
  });
});
