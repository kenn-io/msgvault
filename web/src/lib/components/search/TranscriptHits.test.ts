import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { recording, recordingSearchHit as hit, recordingSearchReport as report } from '../../archive/recording-fixtures.test-support';
import TranscriptHits from './TranscriptHits.svelte';

function mount(fetchFn = vi.fn<typeof fetch>(async () => Response.json(report()))) {
  const client = createAPIClient(fetchFn);
  return { client, fetchFn, ...render(TranscriptHits, { props: { client, query: 'quarterly', supported: true } }) };
}

afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); });

describe('TranscriptHits', () => {
  it('debounces typing and immediately cancels and clears stale evidence', async () => {
    vi.useFakeTimers();
    let finish!: (value: Response) => void;
    const fetchFn = vi.fn<typeof fetch>().mockResolvedValueOnce(Response.json(report()))
      .mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }))
      .mockResolvedValue(Response.json(report({ results: [hit({ excerpt: 'final evidence' })] })));
    const view = mount(fetchFn);
    await vi.advanceTimersByTimeAsync(300);
    expect(screen.getByText('Quarterly numbers')).toBeTruthy();
    await view.rerender({ client: view.client, query: 'q', supported: true });
    expect(screen.queryByRole('link')).toBeNull();
    await vi.advanceTimersByTimeAsync(300);
    const stale = fetchFn.mock.calls[1][0] as Request;
    for (const query of ['qu', 'qua', 'quarter']) {
      await view.rerender({ client: view.client, query, supported: true });
      expect(stale.signal.aborted).toBe(true);
      await vi.advanceTimersByTimeAsync(100);
      expect(fetchFn).toHaveBeenCalledTimes(2);
    }
    finish(Response.json(report({ results: [hit({ excerpt: 'obsolete evidence' })] })));
    await vi.advanceTimersByTimeAsync(200);
    expect(fetchFn).toHaveBeenCalledTimes(3);
    expect(new URL((fetchFn.mock.calls[2][0] as Request).url).searchParams.get('q')).toBe('quarter');
    expect(screen.getByText('final evidence')).toBeTruthy();
    expect(screen.queryByText('obsolete evidence')).toBeNull();
    view.unmount();
  });

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

  it('clears evidence for unsupported queries and scopes', async () => {
    vi.useFakeTimers();
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(report({ results: [hit({ excerpt: 'current evidence' })] })));
    const view = mount(fetchFn);
    await vi.advanceTimersByTimeAsync(300);
    expect(fetchFn).toHaveBeenCalledTimes(1);
    expect(screen.getByText('current evidence')).toBeTruthy();
    await view.rerender({ client: view.client, query: '"quarterly"', supported: true });
    expect(screen.queryByText('current evidence')).toBeNull();
    expect(fetchFn).toHaveBeenCalledTimes(1);
    await view.rerender({ client: view.client, query: 'quarterly', supported: true });
    await vi.advanceTimersByTimeAsync(300);
    expect(new URL((fetchFn.mock.calls[1][0] as Request).url).searchParams.get('q')).toBe('quarterly');
    expect(screen.getByText('current evidence')).toBeTruthy();
    await view.rerender({ client: view.client, query: 'quarterly', supported: false });
    expect(screen.queryByText('current evidence')).toBeNull();
    expect(screen.getByRole('status').textContent).toContain('Full text with no grouping and at most one Person filter');
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
    await vi.advanceTimersByTimeAsync(300);
    expect(screen.getByText('No spoken matches.')).toBeTruthy();
    view.unmount();
  });

  it.each([false, true])('expires evidence without polling, slow response: %s', async slow => {
    vi.useFakeTimers();
    let finish!: (value: Response) => void;
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(report()));
    if (slow) fetchFn.mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }));
    const view = mount(fetchFn);
    await vi.advanceTimersByTimeAsync(300);
    await vi.advanceTimersByTimeAsync(30_000);
    expect(screen.queryByRole('link')).toBeNull();
    if (slow) {
      expect((fetchFn.mock.calls[0][0] as Request).signal.aborted).toBe(true);
      finish(Response.json(report()));
      await vi.advanceTimersByTimeAsync(300);
      expect(screen.getByText('Recording search timed out.')).toBeTruthy();
      expect(screen.getByRole('button', { name: 'Retry' })).toBeTruthy();
      expect(screen.queryByRole('button', { name: 'Refresh' })).toBeNull();
      expect(screen.queryByRole('link')).toBeNull();
    } else {
      expect(screen.getByText('Recording results expired.')).toBeTruthy();
    }
    await vi.advanceTimersByTimeAsync(60_000);
    expect(view.fetchFn).toHaveBeenCalledTimes(1);
    await fireEvent.click(screen.getByRole('button', { name: slow ? 'Retry' : 'Refresh' }));
    await vi.advanceTimersByTimeAsync(300);
    expect(screen.getByRole('link').textContent).toContain('Quarterly numbers');
    expect(view.fetchFn).toHaveBeenCalledTimes(2);
    view.unmount();
  });

  it.each([
    ['café 東京 2026', true], ['cafe\u0301', true], ['from:alice@example.com quarterly', false], ['"quarterly numbers"', false],
    ['not', true], ['AND', true], ['\u0301', false], ['quarterly-review', false],
  ] as const)('admits plain words and rejects unsupported syntax %s', async (query, searched) => {
    vi.useFakeTimers();
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(report({ attachments: {
      results: [{ message_id: 44, conversation_id: 22, attachment_id: 55, filename: 'report.pdf', containing_title: 'Report', excerpt: 'quarterly-review' }], truncated: false,
    } })));
    render(TranscriptHits, { props: { client: createAPIClient(fetchFn), query, supported: true } });
    await vi.advanceTimersByTimeAsync(300);
    if (searched) {
      expect(screen.getByText('Quarterly numbers')).toBeTruthy();
      expect(new URL((fetchFn.mock.calls.at(-1)![0] as Request).url).searchParams.get('q')).toBe(query);
    } else {
      expect(screen.getByRole('region', { name: 'Recordings' })).toBeTruthy();
      expect(screen.getByText(/Recording search supports plain words only/)).toBeTruthy();
      expect(fetchFn).toHaveBeenCalledTimes(1);
    }
    if (query === 'quarterly-review') {
      expect(screen.getByRole('region', { name: 'Attachments' }).textContent).toContain('quarterly-review');
    }
  });

  it.each([
    [400, 'media_search_scope_limit', 'Add a Person filter to search recordings.'],
    [400, 'invalid_media_search', 'Recording search rejected this query. Try different plain words.'],
    [400, 'invalid_parameter', 'direction must be from_person, to_person, or group'],
    [403, 'forbidden', 'Recording search cannot read this query. Check archive access or change the query.'],
    [400, 'invalid_parameter', 'Recording search query is too long. Use fewer words.', 'é'.repeat(4097), 'q is required and must be at most 8192 bytes'],
  ])('keeps %s %s terminal until the query changes', async (status, error, text, query = 'quarterly', message = text) => {
    vi.useFakeTimers();
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ error, message }, { status }));
    const client = createAPIClient(fetchFn);
    const view = render(TranscriptHits, { props: { client, query, supported: true } });
    await vi.advanceTimersByTimeAsync(300);
    expect(screen.getByText(text)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Refresh' })).toBeNull();
    await fireEvent(document, new Event('visibilitychange'));
    expect(fetchFn).toHaveBeenCalledTimes(1);
    const participantID = error === 'media_search_scope_limit' ? 7 : undefined;
    if (participantID !== undefined) {
      fetchFn.mockResolvedValueOnce(Response.json(report()));
      await view.rerender({ client, query, supported: true, participantID });
      await vi.advanceTimersByTimeAsync(300);
      expect(screen.getByRole('link').textContent).toContain(recording().filename);
    }
    await view.rerender({ client, query: 'different', supported: true, participantID });
    await vi.advanceTimersByTimeAsync(300);
    expect(fetchFn).toHaveBeenCalledTimes(participantID === undefined ? 2 : 3);
    expect(screen.getByText(participantID === undefined ? message : 'This Person filter exceeds recording-search limits. Use person-scoped recording search in the CLI.')).toBeTruthy();
    view.unmount();
  });

});

it('opens attachment messages and keeps recording metadata in its own list', async () => {
  const onOpen = vi.fn();
  const attachment = { message_id: 44, conversation_id: 22, attachment_id: 55, filename: 'report.pdf', containing_title: 'Quarterly report', occurred_at: '2026-07-18T12:00:00Z', excerpt: 'quartz inside the PDF' };
  const client = createAPIClient(vi.fn<typeof fetch>(async () => Response.json(report({ attachments: { results: [attachment], truncated: true } }))));
  render(TranscriptHits, { props: { client, query: 'quartz', supported: true, onOpen } });
  const link = await screen.findByRole('link', { name: 'Quarterly report' });
  expect(link.getAttribute('href')).toBe('/messages/44');
  expect(link.textContent).toContain('report.pdf');
  expect(link.textContent).toContain('quartz inside the PDF');
  expect(link.querySelectorAll('time')).toHaveLength(1);
  expect(screen.getByText('More may match')).toBeTruthy();
  await fireEvent.click(link);
  expect(onOpen).toHaveBeenCalledWith(attachment);
  expect(screen.getByText('Provider transcript')).toBeTruthy();
});

it.each(['empty', 'error', 'absent'])('shows the attachment %s state', async (state) => {
  mount(vi.fn<typeof fetch>(async () => Response.json(report({
    results: [],
    attachments: state === 'absent' ? undefined : { results: [], truncated: false, error: state === 'error' ? { error: 'transport', message: 'Attachment search timed out.' } : undefined },
  }))));
  if (state === 'absent') {
    await screen.findByText('No spoken matches.');
    expect(screen.queryByRole('heading', { name: /Attachments/ })).toBeNull();
  } else {
    await screen.findByRole('heading', { name: /Attachments/ });
    expect(screen.queryByText('No matches in text Docbank has extracted from mirrored attachments.') !== null).toBe(state !== 'error');
    if (state === 'error') {
      expect(screen.getByText('Attachment search timed out.')).toBeTruthy();
      expect(screen.getByRole('heading', { name: 'Attachments' }).textContent).toBe('Attachments');
    }
  }
});

it.each([
  ['transport', true], ['server_error', true], ['timeout', true], ['rate_limited', true],
  ['validation', false], ['unauthorized', false], ['invalid_request', false],
])('offers attachment retry for %s only when transient', async (error, retryable) => {
  const fetchFn = vi.fn<typeof fetch>().mockResolvedValueOnce(Response.json(report({ attachments: {
    results: [], truncated: false, error: { error, message: 'Attachment search is unavailable.' },
  } }))).mockResolvedValue(Response.json(report({ attachments: { results: [], truncated: false } })));
  mount(fetchFn);
  await screen.findByText('Attachment search is unavailable.');
  expect(screen.queryByRole('button', { name: 'Retry' }) !== null).toBe(retryable);
  if (retryable) {
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByText('No matches in text Docbank has extracted from mirrored attachments.');
  }
});

it.each([
  ['media_search_scope_limit', 'Add a Person filter to search recordings.', false],
  ['invalid_media_search', 'Recording search rejected this query. Try different plain words.', false],
  ['media_search_unavailable', 'Recording search unavailable.', true],
])('handles partial recording failure %s alongside attachment hits', async (error, message, retryable) => {
  const fetchFn = vi.fn<typeof fetch>().mockResolvedValueOnce(Response.json(report({ results: [], partial: true, recordings_error: { error, message: 'raw server error' }, attachments: {
    results: [{ message_id: 44, conversation_id: 22, attachment_id: 55, filename: 'report.pdf', containing_title: 'Report', excerpt: 'quartz' }], truncated: false,
  } }))).mockResolvedValue(Response.json(report()));
  mount(fetchFn);
  await screen.findByRole('link', { name: 'Report' });
  expect(screen.getByText(message)).toBeTruthy();
  expect(screen.queryByText('raw server error')).toBeNull();
  expect(screen.queryByText('No matches in searched transcripts.')).toBeNull();
  expect(screen.queryByText('No spoken matches.')).toBeNull();
  expect(screen.queryByRole('button', { name: 'Retry' }) !== null).toBe(retryable);
  if (retryable) {
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByText('Quarterly numbers');
    expect(screen.queryByRole('link', { name: 'Report' })).toBeNull();
  }
});

it.each([undefined, 7])('shows the scope-limit notice for attachments with Person %s', async (participantID) => {
  const client = createAPIClient(vi.fn<typeof fetch>(async () => Response.json(report({ results: [], attachments: {
    results: [], truncated: false, error: { error: 'media_search_scope_limit', message: 'Search supports at most 4096 media versions and source selectors' },
  } }))));
  render(TranscriptHits, { props: { client, query: 'quarterly', supported: true, participantID } });
  await screen.findByText(participantID === undefined
    ? 'Attachment search supports at most 4096 mirrored versions. Add a Person filter to search attachments.'
    : 'This Person filter exceeds the attachment-search limit of 4096 mirrored versions. Choose a Person filter with fewer attachments.');
  expect(screen.queryByText('No matches in text Docbank has extracted from mirrored attachments.')).toBeNull();
  expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
});

it('marks a selected message in only one result list', async () => {
  const client = createAPIClient(vi.fn<typeof fetch>(async () => Response.json(report({ results: [hit({ message_id: 44 })], attachments: {
    results: [{ message_id: 44, conversation_id: 22, attachment_id: 55, filename: 'report.pdf', containing_title: 'Report', excerpt: 'quartz' }], truncated: false,
  } }))));
  render(TranscriptHits, { props: { client, query: 'quartz', supported: true, selectedMessageID: 44 } });
  await screen.findByRole('link', { name: 'Report' });
  expect(screen.getAllByRole('link', { current: true })).toHaveLength(1);
});
