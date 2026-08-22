import { describe, expect, it, vi } from 'vitest';

import {
  readBoundedStream,
  validatedImageBlob,
  validatedThumbnailBlob
} from './preview-bytes';

const imageCases = [
  ['image/png', [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]],
  ['image/jpeg', [0xff, 0xd8, 0xff, 0xe0]],
  ['image/gif', [...new TextEncoder().encode('GIF89a')]],
  ['image/webp', [...new TextEncoder().encode('RIFF'), 4, 0, 0, 0, ...new TextEncoder().encode('WEBP')]]
] as const;

describe('preview byte validation', () => {
  it.each(imageCases)('accepts %s only when metadata, response MIME, and magic agree', (mime, bytes) => {
    const blob = validatedImageBlob(new Uint8Array(bytes), mime, `${mime}; charset=binary`);
    expect(blob.type).toBe(mime);
  });

  it.each([
    ['SVG', 'image/svg+xml', new TextEncoder().encode('<svg></svg>')],
    ['HTML', 'image/png', new TextEncoder().encode('<html></html>')],
    ['mismatched metadata', 'image/jpeg', new Uint8Array(imageCases[0]![1])]
  ])('rejects %s preview bytes', (_name, mime, bytes) => {
    expect(() => validatedImageBlob(bytes, mime, 'image/png')).toThrow(/image preview/i);
  });

  it('enforces Content-Length before reading and a byte limit while streaming', async () => {
    await expect(readBoundedStream(
      new Response(new Uint8Array([1])).body,
      new Headers({ 'Content-Length': '5' }),
      new AbortController().signal,
      4
    )).rejects.toThrow(/byte limit/i);

    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new Uint8Array([1, 2, 3]));
        controller.enqueue(new Uint8Array([4, 5]));
        controller.close();
      }
    });
    await expect(readBoundedStream(stream, new Headers(), new AbortController().signal, 4))
      .rejects.toThrow(/byte limit/i);
  });

  it('cancels an oversized declared response body exactly once before reporting the size error', async () => {
    const cancel = vi.fn(async () => undefined);
    const stream = new ReadableStream<Uint8Array>({ cancel });

    await expect(readBoundedStream(
      stream, new Headers({ 'Content-Length': '5' }), new AbortController().signal, 4
    )).rejects.toThrow(/byte limit/i);

    expect(cancel).toHaveBeenCalledOnce();
  });

  it('keeps the declared size error when the body is absent or cancellation rejects', async () => {
    await expect(readBoundedStream(
      null, new Headers({ 'Content-Length': '5' }), new AbortController().signal, 4
    )).rejects.toThrow(/byte limit/i);

    const cancel = vi.fn(async () => { throw new Error('cancel failed'); });
    const stream = new ReadableStream<Uint8Array>({ cancel });
    await expect(readBoundedStream(
      stream, new Headers({ 'Content-Length': '5' }), new AbortController().signal, 4
    )).rejects.toThrow(/byte limit/i);
    expect(cancel).toHaveBeenCalledOnce();
  });

  it('accepts bounded static PNG and JPEG thumbnails', () => {
    const png = pngBytes(640, 480);
    const jpeg = jpegBytes(800, 600);

    expect(validatedThumbnailBlob(png, 'image/png', 'image/png').type).toBe('image/png');
    expect(validatedThumbnailBlob(jpeg, 'image/jpeg', 'image/jpeg').type).toBe('image/jpeg');
  });

  it('rejects thumbnails with excessive dimensions or decoded pixels', () => {
    expect(() => validatedThumbnailBlob(
      pngBytes(4097, 1), 'image/png', 'image/png'
    )).toThrow(/thumbnail/i);
    expect(() => validatedThumbnailBlob(
      jpegBytes(4096, 4096), 'image/jpeg', 'image/jpeg'
    )).toThrow(/thumbnail/i);
  });

  it('rejects animated and unbounded image formats before browser decoding', () => {
    expect(() => validatedThumbnailBlob(
      pngBytes(320, 240, true), 'image/png', 'image/png'
    )).toThrow(/thumbnail/i);
    expect(() => validatedThumbnailBlob(
      new Uint8Array(imageCases[2]![1]), 'image/gif', 'image/gif'
    )).toThrow(/thumbnail/i);
    expect(() => validatedThumbnailBlob(
      new Uint8Array(imageCases[3]![1]), 'image/webp', 'image/webp'
    )).toThrow(/thumbnail/i);
  });
});

function pngBytes(width: number, height: number, animated = false): Uint8Array {
  const chunks = [pngChunk('IHDR', [
    ...u32(width), ...u32(height), 8, 2, 0, 0, 0
  ])];
  if (animated) chunks.push(pngChunk('acTL', [...u32(2), ...u32(0)]));
  chunks.push(pngChunk('IEND', []));
  return new Uint8Array([
    0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
    ...chunks.flat()
  ]);
}

function pngChunk(type: string, data: number[]): number[] {
  return [...u32(data.length), ...new TextEncoder().encode(type), ...data, 0, 0, 0, 0];
}

function jpegBytes(width: number, height: number): Uint8Array {
  return new Uint8Array([
    0xff, 0xd8,
    0xff, 0xc0, 0, 11, 8, ...u16(height), ...u16(width), 1, 1, 0x11, 0,
    0xff, 0xd9
  ]);
}

function u16(value: number): number[] {
  return [(value >>> 8) & 0xff, value & 0xff];
}

function u32(value: number): number[] {
  return [(value >>> 24) & 0xff, (value >>> 16) & 0xff, (value >>> 8) & 0xff, value & 0xff];
}
