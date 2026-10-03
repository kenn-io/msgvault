import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import MessageAttachments from './MessageAttachments.svelte';

const hash = 'a'.repeat(64);
const recording = { filename: 'call.wav', mimeType: 'audio/wav', sizeBytes: 2048, contentHash: hash };

describe('MessageAttachments', () => {
  it('plays and downloads archived recording bytes without preloading audio', () => {
    render(MessageAttachments, { attachments: [recording] });
    const player = screen.getByLabelText('Play call.wav') as HTMLAudioElement;
    expect(player.getAttribute('src')).toBe(`/api/v1/attachments/${hash}/content`);
    expect(player.controls).toBe(true);
    expect(player.preload).toBe('none');
    expect(player.autoplay).toBe(false);
    const link = screen.getByRole('link', { name: 'Download call.wav' });
    expect(link.getAttribute('href')).toBe(player.getAttribute('src'));
    expect(link.getAttribute('download')).toBe('call.wav');
  });

  it('reports unavailable bytes and never turns an external URL into a media request', () => {
    render(MessageAttachments, { attachments: [
      { ...recording, contentHash: undefined },
      { ...recording, filename: 'unsafe.wav', contentHash: 'https://recordings.example/call.wav' },
    ] });
    expect(screen.queryByRole('link')).toBeNull();
    expect(screen.queryByLabelText('Play call.wav')).toBeNull();
    expect(screen.queryByLabelText('Play unsafe.wav')).toBeNull();
    expect(screen.getAllByText('Content has not been archived locally.')).toHaveLength(2);
  });

  it('keeps unsupported formats downloadable without trying audio playback', () => {
    render(MessageAttachments, { attachments: [{ ...recording, filename: 'notes.json', mimeType: 'application/json' }] });
    expect(screen.getByRole('link', { name: 'Download notes.json' })).toBeTruthy();
    expect(screen.queryByLabelText('Play notes.json')).toBeNull();
  });

  it('keeps download available when browser playback fails', async () => {
    render(MessageAttachments, { attachments: [recording] });
    await fireEvent.error(screen.getByLabelText('Play call.wav'));
    expect(screen.getByRole('alert').textContent).toContain('could not be played');
    expect(screen.getByRole('link', { name: 'Download call.wav' })).toBeTruthy();
  });

  it('does not show an empty attachment section', () => {
    render(MessageAttachments, { attachments: [] });
    expect(screen.queryByRole('region', { name: 'Attachments' })).toBeNull();
  });
});
