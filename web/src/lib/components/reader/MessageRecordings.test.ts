import { render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

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
        })
      ]
    });

    const section = await screen.findByRole('region', { name: 'Recordings' });
    const request = fetchFn.mock.calls[0][0] as Request;
    expect(new URL(request.url).pathname).toBe('/api/v1/messages/9/recordings');
    expect(section.textContent).toContain('voice.wav');
    expect(section.textContent).toContain('2 KB');
    expect(section.textContent).toContain('Provider transcript');
    const lines = section.querySelectorAll('.transcript p');
    expect(lines).toHaveLength(2);
    expect(lines[0].querySelector('time')?.textContent).toBe('0:00');
    expect(lines[0].querySelector('strong')?.textContent).toBe('alice');
    expect(lines[0].textContent).toContain('synthetic transcript');
    expect(lines[1].querySelector('time')).toBeNull();
    expect(lines[1].querySelector('strong')).toBeNull();
    expect(lines[1].textContent).toBe('second line');
  });

  it('marks a partial generated transcript and an empty one', async () => {
    mount({
      message_id: 9,
      recordings: [
        recording({ attachment_id: 1, transcript: { origin: 'generated', partial: true, units: [{ text: 'synthetic transcript' }] } }),
        recording({ attachment_id: 2, transcript: { origin: 'supplied', partial: false, units: [] } })
      ]
    });

    const section = await screen.findByRole('region', { name: 'Recordings' });
    expect(section.textContent).toContain('Generated transcript · Partial');
    expect(section.textContent).toContain('The transcript has no text.');
  });

  it('labels every non-ready state', async () => {
    const labels: Record<string, string> = {
      processing: 'Transcript is still processing.',
      missing: 'No transcript for this recording.',
      failed: 'Transcription failed.',
      unsupported: "This audio format can't be transcribed.",
      media_missing: "The recording's audio is missing from the archive.",
      unavailable: 'Transcript unavailable.'
    };
    mount({
      message_id: 9,
      recordings: Object.keys(labels).map((state, index) => recording({ attachment_id: index + 1, state }))
    });

    const section = await screen.findByRole('region', { name: 'Recordings' });
    for (const label of Object.values(labels)) expect(section.textContent).toContain(label);
    expect(section.querySelector('.transcript')).toBeNull();
  });

  it('lists two live revisions of one attachment', async () => {
    mount({
      message_id: 9,
      recordings: [recording({ state: 'processing' }), recording({ state: 'unavailable' })]
    });

    const section = await screen.findByRole('region', { name: 'Recordings' });
    expect(section.querySelectorAll('li')).toHaveLength(2);
    expect(section.textContent).toContain('Transcript is still processing.');
    expect(section.textContent).toContain('Transcript unavailable.');
  });

  it('reports a failed request', async () => {
    mount({ error: 'unavailable', message: 'unavailable' }, 503);

    expect((await screen.findByRole('alert')).textContent).toBe('Could not load recordings (503)');
  });

  it('renders nothing for a message without recordings', async () => {
    const { container, fetchFn } = mount({ message_id: 9, recordings: [] });

    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(1));
    await Promise.resolve();
    expect(container.textContent?.trim()).toBe('');
  });
});
