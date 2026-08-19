import { render, screen, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { PersonFileSearchRow } from '../../explore/models';
import MediaThumbnail, { MAX_THUMBNAIL_BYTES } from './MediaThumbnail.svelte';

function media(overrides: Partial<PersonFileSearchRow> = {}): PersonFileSearchRow {
  return {
    id: 7, key: 'file:7', entry_key: 'message:11', message_id: 11, conversation_id: 21,
    occurred_at: '2026-07-18T12:00:00Z', source_id: 1, source_type: 'synthetic',
    source_identifier: 'archive@example.com', containing_title: 'Containing item',
    filename: 'photo.png', mime_type: 'image/png', mime_family: 'image', size_bytes: 8,
    content_state: 'local_content', content_available: true,
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

  it('rejects oversized declared or streamed images and isolates the failure to a placeholder', async () => {
    const declaredFetch = vi.fn<typeof fetch>();
    const declared = render(MediaThumbnail, {
      client: createAPIClient(declaredFetch),
      file: media({ size_bytes: MAX_THUMBNAIL_BYTES + 1 })
    });
    expect(await screen.findByText('Image preview unavailable')).toBeDefined();
    expect(declaredFetch).not.toHaveBeenCalled();
    declared.unmount();

    const streamedFetch = vi.fn<typeof fetch>(async () => new Response(new Uint8Array([137]), {
      headers: { 'Content-Type': 'image/png', 'Content-Length': String(MAX_THUMBNAIL_BYTES + 1) }
    }));
    render(MediaThumbnail, { client: createAPIClient(streamedFetch), file: media() });
    expect(await screen.findByText('Image preview unavailable')).toBeDefined();
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

  it('does not automatically decode image formats with unbounded animation frames', async () => {
    const fetchFn = vi.fn<typeof fetch>();
    render(MediaThumbnail, {
      client: createAPIClient(fetchFn),
      file: media({ filename: 'animation.gif', mime_type: 'image/gif' })
    });

    expect(await screen.findByText('Image preview unavailable')).toBeDefined();
    expect(fetchFn).not.toHaveBeenCalled();
    expect(screen.queryByRole('img')).toBeNull();
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
