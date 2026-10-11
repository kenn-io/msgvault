import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { recordingSearchHit as hit, recordingSearchReport as report } from '../../archive/recording-fixtures.test-support';
import TranscriptHits from './TranscriptHits.svelte';
import { EverythingSessionState } from '../shell/EverythingSessionState.svelte';

function mount(fetchFn = vi.fn<typeof fetch>(async () => Response.json(report()))) {
  const client = createAPIClient(fetchFn);
  return { client, fetchFn, ...render(TranscriptHits, { props: { client, session: new EverythingSessionState(), query: 'quarterly', supported: true } }) };
}

afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); });

describe('TranscriptHits', () => {
  it('requests lexical evidence and preserves source occurrences, provenance and optional timing', async () => {
    const { fetchFn } = mount(vi.fn<typeof fetch>(async () => Response.json(report({ results: [
      hit({ start_ms: 0, end_ms: 2800, excerpt: 'Quarterly <numbers>' }), hit({ message_id: 10, origin: 'generated', filename: 'other.wav' }),
    ] }))));
    const links = await screen.findAllByRole('link');
    expect(links.map(link => link.getAttribute('href'))).toEqual(['/messages/9', '/messages/10']);
    expect(links[0].textContent).toContain('Team chat');
    expect(links[0].textContent).toContain('voice.wav');
    expect(links[0].textContent).toContain('Provider transcript');
    expect(links[0].querySelector('.transcript time')?.getAttribute('title')).toBe('Until 0:02');
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
    await screen.findByText(/No matches in searched transcripts\./);
    expect(screen.getByRole('status').textContent).toContain('2 pending');
    expect(screen.getByRole('status').textContent).toContain('3 unavailable');
    expect(screen.getByRole('status').textContent).toContain('1 unattributed');
    expect(screen.getByRole('status').textContent).toContain('More may match');
    expect(screen.queryByText('No spoken matches.')).toBeNull();
  });

  it('restricts unsupported scopes and clears the old query before ignoring its delayed response', async () => {
    vi.useFakeTimers();
    let finish!: (value: Response) => void;
    const fetchFn = vi.fn<typeof fetch>().mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }))
      .mockResolvedValue(Response.json(report({ results: [hit({ excerpt: 'current evidence' })] })));
    const view = mount(fetchFn);
    await vi.advanceTimersByTimeAsync(300);
    expect(fetchFn).toHaveBeenCalledTimes(1);
    const old = fetchFn.mock.calls[0][0] as Request;
    for (const query of ['q', 'qu', 'quarterly']) {
      await view.rerender({ client: view.client, query, supported: true });
      expect(screen.queryByText('current evidence')).toBeNull();
      expect(fetchFn).toHaveBeenCalledTimes(1);
    }
    expect(old.signal.aborted).toBe(true);
    await vi.advanceTimersByTimeAsync(299);
    expect(fetchFn).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(new URL((fetchFn.mock.calls[1][0] as Request).url).searchParams.get('q')).toBe('quarterly');
    expect(screen.getByText('current evidence')).toBeTruthy();
    finish(Response.json(report({ results: [hit({ excerpt: 'obsolete evidence' })] })));
    await vi.advanceTimersByTimeAsync(0);
    expect(screen.queryByText('obsolete evidence')).toBeNull();
    await view.rerender({ client: view.client, query: 'quarterly', supported: false });
    expect(screen.queryByText('current evidence')).toBeNull();
    expect(screen.getByRole('status').textContent).toContain('Full text with no filters or grouping');
    expect(fetchFn).toHaveBeenCalledTimes(2);
  });

  it.each([
    { status: 429, refresh: true }, { status: 503, refresh: true }, { status: 503, refresh: false },
  ])('clears excerpts after a failed search %j and allows retry', async ({ status, refresh }) => {
    vi.useFakeTimers();
    const fetchFn = vi.fn<typeof fetch>();
    if (refresh) fetchFn.mockResolvedValueOnce(Response.json(report()));
    fetchFn.mockResolvedValueOnce(Response.json({ error: status === 503 ? 'media_search_unavailable' : 'rate_limit_exceeded' }, { status }));
    fetchFn.mockResolvedValue(Response.json(report({ results: refresh ? [] : [hit()] })));
    const view = mount(fetchFn);
    expect(screen.getByRole('region', { name: 'Recordings' })).toBeTruthy();
    if (refresh) {
      await vi.advanceTimersByTimeAsync(300);
      expect(screen.getByRole('link').textContent).toContain('Quarterly numbers');
      await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    }
    expect(screen.queryByRole('link')).toBeNull();
    await vi.advanceTimersByTimeAsync(300);
    expect(screen.getByRole('region', { name: 'Recordings' })).toBeTruthy();
    expect(screen.getByText(status === 503 ? 'Recording search unavailable.' : 'Could not load recording matches.')).toBeTruthy();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await vi.advanceTimersByTimeAsync(300);
    if (refresh) expect(screen.getByText('No spoken matches.')).toBeTruthy();
    else expect(screen.getByRole('link')).toBeTruthy();
    view.unmount();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(fetchFn).toHaveBeenCalledTimes(refresh ? 3 : 2);
  });

  it('clears hidden evidence and coalesces visibility refreshes', async () => {
    vi.useFakeTimers();
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
    let finish!: (value: Response) => void;
    const fetchFn = vi.fn<typeof fetch>().mockResolvedValueOnce(Response.json(report()))
      .mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }))
      .mockResolvedValue(Response.json(report({ results: [] })));
    const view = mount(fetchFn);
    await vi.advanceTimersByTimeAsync(300);
    hidden.mockReturnValue(true);
    await fireEvent(document, new Event('visibilitychange'));
    expect(screen.queryByRole('link')).toBeNull();
    hidden.mockReturnValue(false);
    await fireEvent(document, new Event('visibilitychange'));
    await fireEvent(document, new Event('visibilitychange'));
    expect(fetchFn).toHaveBeenCalledTimes(2);
    finish(Response.json(report({ results: [] })));
    await vi.advanceTimersByTimeAsync(0);
    expect(screen.getByText('No spoken matches.')).toBeTruthy();
    view.unmount();
  });

  it('expires evidence without polling and manually refreshes it', async () => {
    vi.useFakeTimers();
    const view = mount();
    await vi.advanceTimersByTimeAsync(300);
    await vi.advanceTimersByTimeAsync(30_000);
    expect(screen.queryByRole('link')).toBeNull();
    expect(screen.getByText('Recording results expired.')).toBeTruthy();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(view.fetchFn).toHaveBeenCalledTimes(1);
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    await vi.advanceTimersByTimeAsync(300);
    expect(screen.getByRole('link').textContent).toContain('Quarterly numbers');
    expect(view.fetchFn).toHaveBeenCalledTimes(2);
    view.unmount();
  });

  it('counts request time toward expiry and ignores an expired slow response', async () => {
    vi.useFakeTimers();
    let finish!: (value: Response) => void;
    const fetchFn = vi.fn<typeof fetch>(() => new Promise(resolve => { finish = resolve; }));
    const client = createAPIClient(fetchFn);
    const view = render(TranscriptHits, { props: { client, session: new EverythingSessionState(), query: 'quarterly', supported: true } });
    await vi.advanceTimersByTimeAsync(300);
    const request = fetchFn.mock.calls[0][0] as Request;
    await vi.advanceTimersByTimeAsync(30_000);
    expect(request.signal.aborted).toBe(true);
    finish(Response.json(report()));
    await vi.advanceTimersByTimeAsync(0);
    expect(screen.getByText('Recording search timed out.')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Retry' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Refresh' })).toBeNull();
    expect(screen.queryByRole('link')).toBeNull();
    expect(fetchFn).toHaveBeenCalledTimes(1);
    view.unmount();
  });

  it.each([
    ['café 東京 2026', true], ['cafe\u0301', true], ['from:alice@example.com quarterly', false], ['"quarterly numbers"', false],
    ['not', true], ['AND', true], ['\u0301', false],
  ] as const)('admits plain words and rejects unsupported syntax %s', async (query, searched) => {
    vi.useFakeTimers();
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(report()));
    render(TranscriptHits, { props: { client: createAPIClient(fetchFn), session: new EverythingSessionState(), query, supported: true } });
    await vi.advanceTimersByTimeAsync(301);
    if (searched) {
      expect(screen.getByText('Quarterly numbers')).toBeTruthy();
      expect(new URL((fetchFn.mock.calls.at(-1)![0] as Request).url).searchParams.get('q')).toBe(query);
    } else {
      expect(screen.getByRole('region', { name: 'Recordings' })).toBeTruthy();
      expect(screen.getByText(/Recording search supports plain words only/)).toBeTruthy();
      expect(fetchFn).not.toHaveBeenCalled();
    }
  });

  it.each([
    [400, 'media_search_scope_limit', 'This archive exceeds browser recording-search limits. Use person-scoped recording search in the CLI or API.'],
    [400, 'invalid_media_search', 'Recording search rejected this query. Try different plain words.'],
    [400, 'invalid_parameter', 'Recording search query is too long. Use fewer words.'],
    [403, 'forbidden', 'Recording search cannot read this query. Check archive access or change the query.'],
  ])('keeps %s %s terminal for the query or session scope', async (status, error, text) => {
    vi.useFakeTimers();
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ error }, { status }));
    const client = createAPIClient(fetchFn);
    const session = new EverythingSessionState();
    const view = render(TranscriptHits, { props: { client, session, query: 'quarterly', supported: true } });
    await vi.advanceTimersByTimeAsync(300);
    expect(screen.getByText(text)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Refresh' })).toBeNull();
    await fireEvent(document, new Event('visibilitychange'));
    expect(fetchFn).toHaveBeenCalledTimes(1);
    await view.rerender({ client, query: 'different', supported: true });
    await vi.advanceTimersByTimeAsync(301);
    expect(fetchFn).toHaveBeenCalledTimes(error === 'media_search_scope_limit' ? 1 : 2);
    expect(screen.getByText(text)).toBeTruthy();
    view.unmount();
    if (error === 'media_search_scope_limit') {
      const remounted = render(TranscriptHits, { props: { client, session, query: 'another', supported: true } });
      await vi.advanceTimersByTimeAsync(301);
      expect(fetchFn).toHaveBeenCalledTimes(1);
      expect(screen.getByText(text)).toBeTruthy();
      remounted.unmount();
    }
  });

});
