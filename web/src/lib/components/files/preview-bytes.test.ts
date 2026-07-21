import { describe, expect, it, vi } from 'vitest';

import { readBoundedStream, validatedImageBlob } from './preview-bytes';

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
});
