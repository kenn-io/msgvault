import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import MessageRecordings from './MessageRecordings.svelte';

function recording(overrides: Record<string, unknown>) {
  return { attachment_id: 1, filename: 'voice.wav', size_bytes: 2048, state: 'ready', ...overrides };
}

function mount(body: unknown, status = 200) {
  const fetchFn = vi.fn<typeof fetch>(async () => Response.json(body, { status }));
  const view = render(MessageRecordings, { props: { client: createAPIClient(fetchFn), messageId: 9 } });
  return { fetchFn, ...view };
}

afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); });

describe('MessageRecordings', () => {
  it('requests the message recordings and renders a provider transcript with timing and speaker', async () => {
    const { fetchFn } = mount({
      message_id: 9,
      recordings: [
        recording({
          transcript: {
            origin: 'supplied',
            partial: false,
            units: [{ text: 'synthetic transcript', start_ms: 0, end_ms: 1500, speaker: 'alice' }, { text: 'second line' }]
          }
        }),
        recording({ attachment_id: 2, transcript: { origin: 'generated', partial: true, units: [{ text: 'synthetic transcript' }] } }),
        recording({ attachment_id: 3, transcript: { origin: 'supplied', partial: false, units: [] } })
      ]
    });

    const section = await screen.findByRole('region', { name: 'Recordings' });
    const request = fetchFn.mock.calls[0][0] as Request;
    expect(new URL(request.url).pathname).toBe('/api/v1/messages/9/recordings');
    expect(section.textContent).toContain('voice.wav');
    expect(section.textContent).toContain('2 KB');
    expect(section.textContent).toContain('Provider transcript');
    expect(section.textContent).toContain('Generated transcript · Partial');
    expect(section.textContent).toContain('The transcript has no text.');
    const lines = section.querySelector('.transcript')!.querySelectorAll('p');
    expect(lines).toHaveLength(2);
    expect(lines[0].querySelector('time')?.textContent).toBe('0:00');
    expect(lines[0].querySelector('strong')?.textContent).toBe('alice');
    expect(lines[0].textContent).toContain('synthetic transcript');
    expect(lines[1].querySelector('time')).toBeNull();
    expect(lines[1].querySelector('strong')).toBeNull();
    expect(lines[1].textContent).toBe('second line');
  });

  it('labels every non-ready state', async () => {
    const labels: Record<string, string> = {
      processing: 'Transcript is still processing.',
      missing: 'No transcript for this recording.',
      failed: 'Transcription failed.',
      unsupported: "This recording isn't supported for transcription.",
      media_missing: "The recording's audio is missing from the archive.",
      unavailable: 'Transcript unavailable.'
    };
    mount({
      message_id: 9,
      recordings: Object.keys(labels).map((state, index) => recording({ attachment_id: Math.max(1, index), state }))
    });

    const section = await screen.findByRole('region', { name: 'Recordings' });
    for (const label of Object.values(labels)) expect(section.textContent).toContain(label);
    expect(section.querySelectorAll('li')).toHaveLength(6);
    expect(section.querySelector('.transcript')).toBeNull();
  });

  it('renders nothing for a message without recordings', async () => {
    const { container, fetchFn } = mount({ message_id: 9, recordings: [] });

    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(1));
    await Promise.resolve();
    expect(container.textContent?.trim()).toBe('');
  });
  it('refreshes processing evidence while the reader remains open', async () => {
    vi.useFakeTimers();
    const fetchFn = vi.fn<typeof fetch>()
      .mockResolvedValueOnce(Response.json({ message_id: 9, recordings: [recording({ state: 'processing' })] }))
      .mockResolvedValueOnce(Response.json({ message_id: 9, recordings: [recording({ transcript: {
        origin: 'supplied', partial: false, units: [{ text: 'finished transcript' }]
      } })] }))
      .mockResolvedValueOnce(Response.json({ error: 'unavailable' }, { status: 503 }))
      .mockResolvedValueOnce(Response.json({ message_id: 9, recordings: [recording({ state: 'unavailable' })] }))
      .mockResolvedValue(Response.json({ message_id: 9, recordings: [] }));
    render(MessageRecordings, { props: { client: createAPIClient(fetchFn), messageId: 9 } });
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(2000);
    expect(fetchFn).toHaveBeenCalledTimes(2);
    expect(screen.getByText('finished transcript')).toBeTruthy();
    await vi.advanceTimersByTimeAsync(2000);
    expect(screen.queryByText('finished transcript')).toBeNull();
    expect(screen.getByRole('alert').textContent).toBe('Could not load recordings (503)');
    await vi.advanceTimersByTimeAsync(4000);
    expect(screen.getByText('Transcript unavailable.')).toBeTruthy();
    await vi.advanceTimersByTimeAsync(2000);
    expect(screen.queryByRole('region', { name: 'Recordings' })).toBeNull();
  });

  it('invalidates the same message ID and ignores a canceled response', async () => {
    vi.useFakeTimers();
    let resolveOld!: (response: Response) => void;
    let resolveNew!: (response: Response) => void;
    const old = new Promise<Response>((resolve) => { resolveOld = resolve; });
    const current = new Promise<Response>((resolve) => { resolveNew = resolve; });
    const evidence = (text: string) => Response.json({ message_id: 9, recordings: [recording({ transcript: {
      origin: 'supplied', partial: false, units: [{ text }]
    } })] });
    const fetchFn = vi.fn<typeof fetch>().mockResolvedValueOnce(evidence('earlier transcript'))
      .mockReturnValueOnce(old).mockReturnValueOnce(current);
    const client = createAPIClient(fetchFn);
    const view = render(MessageRecordings, { props: { client, messageId: 9, message: { id: 9 } } });
    await vi.advanceTimersByTimeAsync(0);
    await fireEvent(window, new Event('focus'));
    expect(screen.getByText('earlier transcript')).toBeTruthy();
    const request = fetchFn.mock.calls[1][0] as Request;
    await view.rerender({ client, messageId: 9, message: { id: 9, edited: true } });
    expect(request.signal.aborted).toBe(true);
    expect(screen.queryByText('earlier transcript')).toBeNull();
    resolveNew(evidence('current transcript'));
    await vi.advanceTimersByTimeAsync(0);
    expect(screen.getByText('current transcript')).toBeTruthy();
    resolveOld(evidence('obsolete transcript'));
    await vi.advanceTimersByTimeAsync(0);
    expect(screen.queryByText('obsolete transcript')).toBeNull();
    view.unmount();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(fetchFn).toHaveBeenCalledTimes(3);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('pauses hidden requests, refreshes on return and focus, and bounds slow reads', async () => {
    vi.useFakeTimers();
    const deadlines: AbortController[] = [];
    vi.spyOn(AbortSignal, 'timeout').mockImplementation(() => {
      const deadline = new AbortController();
      deadlines.push(deadline);
      return deadline.signal;
    });
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
    const fetchFn = vi.fn<typeof fetch>((input) => new Promise((_resolve, reject) => {
      (input as Request).signal.addEventListener('abort', () => reject(new Error('aborted')));
    }));
    const view = render(MessageRecordings, { props: { client: createAPIClient(fetchFn), messageId: 9 } });
    await vi.advanceTimersByTimeAsync(60_000);
    expect(fetchFn).not.toHaveBeenCalled();
    hidden.mockReturnValue(false);
    await fireEvent(document, new Event('visibilitychange'));
    await fireEvent(window, new Event('focus'));
    expect(fetchFn).toHaveBeenCalledTimes(1);
    const first = fetchFn.mock.calls[0][0] as Request;
    deadlines[0].abort();
    await vi.advanceTimersByTimeAsync(0);
    expect(first.signal.aborted).toBe(true);
    expect(screen.getByRole('alert').textContent).toBe('Could not load recordings');
    await fireEvent(window, new Event('focus'));
    expect(fetchFn).toHaveBeenCalledTimes(2);
    const second = fetchFn.mock.calls[1][0] as Request;
    hidden.mockReturnValue(true);
    await fireEvent(document, new Event('visibilitychange'));
    expect(second.signal.aborted).toBe(true);
    view.unmount();
  });

});
