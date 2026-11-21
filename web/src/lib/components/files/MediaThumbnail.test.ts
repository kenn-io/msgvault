import { render, screen, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { PersonFileSearchRow } from '../../explore/models';
import MediaThumbnail, { MAX_THUMBNAIL_BYTES } from './MediaThumbnail.svelte';
import { readBoundedStream } from './preview-bytes';

vi.mock('./preview-bytes', async importOriginal => {
  const original = await importOriginal<typeof import('./preview-bytes')>();
  return { ...original, readBoundedStream: vi.fn(original.readBoundedStream) };
});

function media(overrides: Partial<PersonFileSearchRow> = {}): PersonFileSearchRow {
  return {
    id: 7, key: 'file:7', entry_key: 'message:11', message_id: 11, conversation_id: 21,
    occurred_at: '2026-07-18T12:00:00Z', source_id: 1, source_type: 'synthetic',
    source_identifier: 'archive@example.com', containing_title: 'Containing item',
    filename: 'photo.png', mime_type: 'image/png', mime_family: 'image', size_bytes: 8,
    content_state: 'unverified', content_available: null,
    person_provenance: { participant_ids: [42], roles: ['from'], directions: ['from_person'] },
    ...overrides
  };
}

function pngBytes(width = 640, height = 480): Uint8Array {
  const u32 = (value: number): number[] => [
    (value >>> 24) & 0xff, (value >>> 16) & 0xff, (value >>> 8) & 0xff, value & 0xff
  ];
  const chunk = (type: string, data: number[]): number[] => [
    ...u32(data.length), ...new TextEncoder().encode(type), ...data, 0, 0, 0, 0
  ];
  return new Uint8Array([
    0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
    ...chunk('IHDR', [...u32(width), ...u32(height), 8, 2, 0, 0, 0]),
    ...chunk('IEND', [])
  ]);
}

describe('MediaThumbnail', () => {
  const createObjectURL = vi.fn(() => 'blob:person-thumbnail');
  const revokeObjectURL = vi.fn();

  beforeEach(() => {
    createObjectURL.mockClear();
    revokeObjectURL.mockClear();
    Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: createObjectURL });
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: revokeObjectURL });
  });

  it('loads authenticated validated image bytes and revokes its URL when the row changes', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => new Response(
      pngBytes().buffer as ArrayBuffer,
      { headers: { 'Content-Type': 'image/png' } }
    ));
    const view = render(MediaThumbnail, { client: createAPIClient(fetchFn), file: media() });

    expect((await screen.findByRole('img', { name: 'Thumbnail photo.png' })).getAttribute('src'))
      .toBe('blob:person-thumbnail');
    expect(new URL((fetchFn.mock.calls[0]![0] as Request).url).pathname).toBe('/api/v1/files/7/content');

    await view.rerender({ file: media({ id: 8, key: 'file:8', filename: 'next.png' }) });
    await waitFor(() => expect(revokeObjectURL).toHaveBeenCalledWith('blob:person-thumbnail'));
  });

  it.each([
    ['oversized metadata', { size_bytes: MAX_THUMBNAIL_BYTES + 1 }],
    ['GIF images', { filename: 'animation.gif', mime_type: 'image/gif' }],
  ])('refuses %s without fetching', async (_label, overrides) => {
    const fetchFn = vi.fn<typeof fetch>();
    render(MediaThumbnail, { client: createAPIClient(fetchFn), file: media(overrides) });
    expect(await screen.findByText('No preview')).toBeDefined();
    expect(fetchFn).not.toHaveBeenCalled();
    expect(screen.queryByRole('img')).toBeNull();
  });

  it('shows a placeholder when the bounded reader rejects the response', async () => {
    const response = new Response(new Uint8Array(pngBytes()).buffer, { headers: { 'Content-Type': 'image/png' } });
    vi.mocked(readBoundedStream).mockRejectedValueOnce(new Error('too large'));
    render(MediaThumbnail, { client: createAPIClient(async () => response), file: media() });
    expect(await screen.findByText('No preview')).toBeDefined();
    expect(readBoundedStream).toHaveBeenCalledWith(response.body, response.headers, expect.any(AbortSignal), 5 * 1024 * 1024);
    expect(createObjectURL).not.toHaveBeenCalled();
  });

  it('uses a typed placeholder for video without attempting browser decoding or a content request', async () => {
    const fetchFn = vi.fn<typeof fetch>();
    render(MediaThumbnail, {
      client: createAPIClient(fetchFn),
      file: media({ filename: 'clip.mp4', mime_type: 'video/mp4', mime_family: 'video' })
    });

    expect(screen.getByText('Video')).toBeDefined();
    expect(fetchFn).not.toHaveBeenCalled();
    expect(screen.queryByRole('video')).toBeNull();
  });

  it('waits for viewport proximity and releases the blob again when the card leaves it', async () => {
    let callback: IntersectionObserverCallback | undefined;
    let target: Element | undefined;
    class TestIntersectionObserver implements IntersectionObserver {
      readonly root = null;
      readonly rootMargin = '240px';
      readonly thresholds = [0];
      constructor(value: IntersectionObserverCallback) { callback = value; }
      observe(element: Element): void { target = element; }
      disconnect(): void {}
      unobserve(): void {}
      takeRecords(): IntersectionObserverEntry[] { return []; }
    }
    vi.stubGlobal('IntersectionObserver', TestIntersectionObserver);
    const fetchFn = vi.fn<typeof fetch>(async () => new Response(
      pngBytes().buffer as ArrayBuffer,
      { headers: { 'Content-Type': 'image/png' } }
    ));
    render(MediaThumbnail, { client: createAPIClient(fetchFn), file: media() });
    await waitFor(() => expect(target).toBeDefined());
    expect(fetchFn).not.toHaveBeenCalled();

    callback!([{ target: target!, isIntersecting: true } as IntersectionObserverEntry], {} as IntersectionObserver);
    expect(await screen.findByRole('img', { name: 'Thumbnail photo.png' })).toBeDefined();
    callback!([{ target: target!, isIntersecting: false } as IntersectionObserverEntry], {} as IntersectionObserver);
    await waitFor(() => expect(revokeObjectURL).toHaveBeenCalledWith('blob:person-thumbnail'));
    vi.unstubAllGlobals();
  });
});
