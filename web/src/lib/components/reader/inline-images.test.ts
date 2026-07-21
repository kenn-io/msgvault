import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { ArchivedInlineImage } from '../../content/sanitize';
import {
  MAX_ARCHIVED_INLINE_IMAGE_BYTES,
  MAX_ARCHIVED_INLINE_IMAGE_CIDS,
  MAX_ARCHIVED_INLINE_IMAGE_CONCURRENCY,
  MAX_ARCHIVED_INLINE_IMAGE_OCCURRENCES,
  MAX_ARCHIVED_INLINE_IMAGE_SERIALIZED_BYTES,
  MAX_ARCHIVED_INLINE_IMAGE_TOTAL_BYTES,
  resolveArchivedInlineImages
} from './inline-images';

function fixture(cids: string[]): { html: string; inlineImages: ArchivedInlineImage[] } {
  return {
    html: cids.map((_cid, index) =>
      `<span data-archived-inline-image="${index}">Inline image loading: Image ${index}</span>`
    ).join(''),
    inlineImages: cids.map((cid, index) => ({ cid, alt: `Image ${index}` }))
  };
}

function pngResponse(bytes: number): Response {
  return new Response(new Uint8Array(bytes), { headers: { 'Content-Type': 'image/png' } });
}

function parse(html: string): DocumentFragment {
  const template = document.createElement('template');
  template.innerHTML = html;
  return template.content;
}

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((resolvePromise) => { resolve = resolvePromise; });
  return { promise, resolve };
}

async function resolveWithPublicationByteLimit(
  cids: string[],
  fetchFn: typeof fetch,
  dataURLBytes: number
): Promise<string> {
  return await resolveArchivedInlineImages({
    ...fixture(cids),
    client: createAPIClient(fetchFn),
    messageId: 42,
    signal: new AbortController().signal,
    publicationLimits: { dataURLBytes }
  });
}

describe('resolveArchivedInlineImages', () => {
  it('exports conservative request and decoded-byte budgets', () => {
    expect(MAX_ARCHIVED_INLINE_IMAGE_CIDS).toBe(32);
    expect(MAX_ARCHIVED_INLINE_IMAGE_BYTES).toBe(5 * 1024 * 1024);
    expect(MAX_ARCHIVED_INLINE_IMAGE_TOTAL_BYTES).toBe(20 * 1024 * 1024);
    expect(MAX_ARCHIVED_INLINE_IMAGE_CONCURRENCY).toBe(1);
    expect(MAX_ARCHIVED_INLINE_IMAGE_OCCURRENCES).toBe(128);
    expect(MAX_ARCHIVED_INLINE_IMAGE_SERIALIZED_BYTES).toBe(24 * 1024 * 1024);
  });

  it('deduplicates thousands of normalized repeats but publishes at most 128 occurrences', async () => {
    const repeated = Array.from({ length: 2_000 }, (_value, index) =>
      index % 2 === 0 ? ' repeat@example.com ' : '<repeat@example.com>'
    );
    const input = fixture(repeated);
    const fetchFn = vi.fn<typeof fetch>(async () => pngResponse(16));

    const html = await resolveArchivedInlineImages({
      ...input,
      client: createAPIClient(fetchFn),
      messageId: 42,
      signal: new AbortController().signal
    });

    const output = parse(html);
    expect(fetchFn).toHaveBeenCalledOnce();
    expect(new URL((fetchFn.mock.calls[0]?.[0] as Request).url).searchParams.get('cid'))
      .toBe('repeat@example.com');
    expect(output.querySelectorAll('img')).toHaveLength(MAX_ARCHIVED_INLINE_IMAGE_OCCURRENCES);
    expect(new Set([...output.querySelectorAll('img')].map((image) => image.src)).size).toBe(1);
    expect(output.querySelectorAll('[data-archived-inline-image]')).toHaveLength(0);
    expect(output.textContent?.match(/Inline image unavailable/g)).toHaveLength(
      2_000 - MAX_ARCHIVED_INLINE_IMAGE_OCCURRENCES
    );
    expect(html).not.toMatch(/cid:|\/api\/v1\/messages/);
  });

  it.each([
    [MAX_ARCHIVED_INLINE_IMAGE_OCCURRENCES, MAX_ARCHIVED_INLINE_IMAGE_OCCURRENCES, 0],
    [MAX_ARCHIVED_INLINE_IMAGE_OCCURRENCES + 1, MAX_ARCHIVED_INLINE_IMAGE_OCCURRENCES, 1]
  ])('enforces the exact publication occurrence boundary for %i repeats', async (
    descriptorCount, expectedImages, expectedUnavailable
  ) => {
    const input = fixture(Array.from({ length: descriptorCount }, () => 'repeat@example.com'));
    const fetchFn = vi.fn<typeof fetch>(async () => pngResponse(1));
    const html = await resolveArchivedInlineImages({
      ...input,
      client: createAPIClient(fetchFn),
      messageId: 42,
      signal: new AbortController().signal
    });
    expect(fetchFn).toHaveBeenCalledOnce();
    expect(parse(html).querySelectorAll('img')).toHaveLength(expectedImages);
    expect(html.match(/Inline image unavailable/g) ?? []).toHaveLength(expectedUnavailable);
  });

  it.each([
    ['exact', 52, 2],
    ['one serialized byte over', 51, 1]
  ])('enforces a synthetic publication-byte budget at the %s boundary', async (
    _name, publicationBytes, expectedImages
  ) => {
    // A three-byte PNG produces 26 ASCII data-URL characters: the complete
    // serialized src charge, including MIME prefix and base64 expansion.
    const html = await resolveWithPublicationByteLimit(
      ['one@example.com', 'two@example.com', 'three@example.com'],
      vi.fn<typeof fetch>(async () => pngResponse(3)),
      publicationBytes
    );
    const output = parse(html);
    expect(output.querySelectorAll('img')).toHaveLength(expectedImages);
    expect(output.textContent?.match(/Inline image unavailable/g) ?? []).toHaveLength(3 - expectedImages);
    expect(html).not.toMatch(/cid:|\/api\/v1\/messages/);
  });

  it('shares one publication-byte budget across interleaved CIDs in DOM order', async () => {
    const html = await resolveWithPublicationByteLimit(
      ['first@example.com', 'second@example.com', 'first@example.com'],
      vi.fn<typeof fetch>(async () => pngResponse(3)),
      52
    );
    const output = parse(html);
    expect([...output.querySelectorAll('img')].map((image) => image.alt)).toEqual(['Image 0', 'Image 1']);
    expect(output.textContent).toContain('Inline image unavailable: Image 2');
  });

  it('does not base64-encode decoded bytes when publication admits no occurrences', async () => {
    const encode = vi.spyOn(globalThis, 'btoa');
    const html = await resolveArchivedInlineImages({
      ...fixture(['bounded@example.com']),
      client: createAPIClient(vi.fn<typeof fetch>(async () => pngResponse(32))),
      messageId: 42,
      signal: new AbortController().signal,
      publicationLimits: { occurrences: 0 }
    });

    expect(encode).not.toHaveBeenCalled();
    expect(html).toContain('Inline image unavailable: Image 0');
  });

  it('bounds repeated near-limit data URLs and the final serialized document', async () => {
    const repeatedCount = 10;
    const input = fixture(Array.from({ length: repeatedCount }, () => 'large@example.com'));
    const fetchFn = vi.fn<typeof fetch>(async () => pngResponse(MAX_ARCHIVED_INLINE_IMAGE_BYTES));
    const html = await resolveArchivedInlineImages({
      ...input,
      client: createAPIClient(fetchFn),
      messageId: 42,
      signal: new AbortController().signal
    });
    const dataURLBytes = 'data:image/png;base64,'.length +
      4 * Math.ceil(MAX_ARCHIVED_INLINE_IMAGE_BYTES / 3);
    const expectedImages = Math.floor(MAX_ARCHIVED_INLINE_IMAGE_SERIALIZED_BYTES / dataURLBytes);
    const output = parse(html);
    expect(fetchFn).toHaveBeenCalledOnce();
    expect(output.querySelectorAll('img')).toHaveLength(expectedImages);
    expect(output.textContent?.match(/Inline image unavailable/g)).toHaveLength(
      repeatedCount - expectedImages
    );
    expect(html).not.toMatch(/cid:|\/api\/v1\/messages/);
    expect(html.length).toBeLessThanOrEqual(
      MAX_ARCHIVED_INLINE_IMAGE_SERIALIZED_BYTES + input.html.length + 4_096
    );
  });

  it('fetches exactly 32 unique CIDs and leaves thousands beyond the cap visible', async () => {
    const input = fixture(Array.from({ length: 2_000 }, (_value, index) => `unique-${index}@example.com`));
    const fetchFn = vi.fn<typeof fetch>(async () => pngResponse(1));

    const html = await resolveArchivedInlineImages({
      ...input,
      client: createAPIClient(fetchFn),
      messageId: 42,
      signal: new AbortController().signal
    });

    const output = parse(html);
    expect(fetchFn).toHaveBeenCalledTimes(MAX_ARCHIVED_INLINE_IMAGE_CIDS);
    expect(output.querySelectorAll('img')).toHaveLength(MAX_ARCHIVED_INLINE_IMAGE_CIDS);
    expect(output.textContent?.match(/Inline image unavailable/g)).toHaveLength(
      2_000 - MAX_ARCHIVED_INLINE_IMAGE_CIDS
    );
  });

  it.each([
    [MAX_ARCHIVED_INLINE_IMAGE_CIDS, MAX_ARCHIVED_INLINE_IMAGE_CIDS, 0],
    [MAX_ARCHIVED_INLINE_IMAGE_CIDS + 1, MAX_ARCHIVED_INLINE_IMAGE_CIDS, 1]
  ])('enforces the exact unique-CID boundary for %i descriptors', async (
    descriptorCount, expectedImages, expectedUnavailable
  ) => {
    const input = fixture(Array.from(
      { length: descriptorCount }, (_value, index) => `boundary-${index}@example.com`
    ));
    const fetchFn = vi.fn<typeof fetch>(async () => pngResponse(1));
    const html = await resolveArchivedInlineImages({
      ...input,
      client: createAPIClient(fetchFn),
      messageId: 42,
      signal: new AbortController().signal
    });
    const output = parse(html);
    expect(fetchFn).toHaveBeenCalledTimes(expectedImages);
    expect(output.querySelectorAll('img')).toHaveLength(expectedImages);
    expect(output.textContent?.match(/Inline image unavailable/g) ?? []).toHaveLength(expectedUnavailable);
  });

  it('allows the exact per-image boundary and rejects one streamed byte over it', async () => {
    const input = fixture(['exact@example.com', 'over@example.com']);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const cid = new URL((input as Request).url).searchParams.get('cid');
      return pngResponse(cid === 'exact@example.com'
        ? MAX_ARCHIVED_INLINE_IMAGE_BYTES
        : MAX_ARCHIVED_INLINE_IMAGE_BYTES + 1);
    });

    const html = await resolveArchivedInlineImages({
      ...input,
      client: createAPIClient(fetchFn),
      messageId: 42,
      signal: new AbortController().signal
    });

    const output = parse(html);
    expect(fetchFn).toHaveBeenCalledTimes(2);
    expect(output.querySelectorAll('img')).toHaveLength(1);
    expect(output.textContent).toContain('Inline image unavailable: Image 1');
  });

  it('stops before fetching once exact aggregate decoded bytes are exhausted', async () => {
    const bytesPerImage = 4 * 1024 * 1024;
    const input = fixture(Array.from({ length: 6 }, (_value, index) => `aggregate-${index}@example.com`));
    const fetchFn = vi.fn<typeof fetch>(async () => pngResponse(bytesPerImage));

    const html = await resolveArchivedInlineImages({
      ...input,
      client: createAPIClient(fetchFn),
      messageId: 42,
      signal: new AbortController().signal,
      // Exercise the real decoded-byte ceiling without also allocating its
      // larger base64/DOM publication representation in the parallel suite.
      publicationLimits: { occurrences: 0 }
    });

    const output = parse(html);
    expect(bytesPerImage * 5).toBe(MAX_ARCHIVED_INLINE_IMAGE_TOTAL_BYTES);
    expect(fetchFn).toHaveBeenCalledTimes(5);
    expect(output.querySelectorAll('img')).toHaveLength(0);
    expect(output.textContent).toContain('Inline image unavailable: Image 4');
    expect(output.textContent).toContain('Inline image unavailable: Image 5');
  });

  it('charges streamed failure bytes to the aggregate budget', async () => {
    const input = fixture(Array.from({ length: 5 }, (_value, index) => `failed-${index}@example.com`));
    const fetchFn = vi.fn<typeof fetch>(async () => pngResponse(MAX_ARCHIVED_INLINE_IMAGE_BYTES + 1));

    const html = await resolveArchivedInlineImages({
      ...input,
      client: createAPIClient(fetchFn),
      messageId: 42,
      signal: new AbortController().signal
    });

    expect(fetchFn).toHaveBeenCalledTimes(4);
    expect(parse(html).querySelectorAll('img')).toHaveLength(0);
    expect(html.match(/Inline image unavailable/g)).toHaveLength(5);
  });

  it('never has more than one authenticated CID request in flight', async () => {
    const input = fixture(['one@example.com', 'two@example.com', 'three@example.com']);
    const pending = [deferred<Response>(), deferred<Response>(), deferred<Response>()];
    let active = 0;
    let maxActive = 0;
    let requestIndex = 0;
    const fetchFn = vi.fn<typeof fetch>(async (): Promise<Response> => {
      const currentRequest = requestIndex;
      requestIndex += 1;
      active += 1;
      maxActive = Math.max(maxActive, active);
      const response = await pending[currentRequest]!.promise;
      active -= 1;
      return response;
    });

    const resolution = resolveArchivedInlineImages({
      ...input,
      client: createAPIClient(fetchFn),
      messageId: 42,
      signal: new AbortController().signal
    });
    await vi.waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(1));
    pending[0]!.resolve(pngResponse(1));
    await vi.waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(2));
    pending[1]!.resolve(pngResponse(1));
    await vi.waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(3));
    pending[2]!.resolve(pngResponse(1));
    await resolution;

    expect(maxActive).toBe(MAX_ARCHIVED_INLINE_IMAGE_CONCURRENCY);
  });

  it('keeps failures visible, continues within budget, and rejects cancellation', async () => {
    const input = fixture(['bad@example.com', 'good@example.com']);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const cid = new URL((input as Request).url).searchParams.get('cid');
      return cid === 'bad@example.com' ? new Response('missing', { status: 404 }) : pngResponse(1);
    });
    const html = await resolveArchivedInlineImages({
      ...input,
      client: createAPIClient(fetchFn),
      messageId: 42,
      signal: new AbortController().signal
    });
    expect(parse(html).querySelectorAll('img')).toHaveLength(1);
    expect(html).toContain('Inline image unavailable: Image 0');

    const pending = deferred<Response>();
    let request: Request | undefined;
    const cancellationFetch = vi.fn<typeof fetch>(async (input) => {
      request = input as Request;
      return await pending.promise;
    });
    const controller = new AbortController();
    const cancelled = resolveArchivedInlineImages({
      ...fixture(['cancel@example.com']),
      client: createAPIClient(cancellationFetch),
      messageId: 42,
      signal: controller.signal
    });
    await vi.waitFor(() => expect(request).toBeDefined());
    controller.abort();
    expect(request?.signal.aborted).toBe(true);
    pending.resolve(pngResponse(1));
    await expect(cancelled).rejects.toMatchObject({ name: 'AbortError' });
  });

  it('cancels a response stream that is blocked mid-read', async () => {
    let streamCancelled = false;
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new Uint8Array([1]));
      },
      cancel() {
        streamCancelled = true;
      }
    });
    const fetchFn = vi.fn<typeof fetch>(async () => new Response(stream, {
      headers: { 'Content-Type': 'image/png' }
    }));
    const controller = new AbortController();
    const resolution = resolveArchivedInlineImages({
      ...fixture(['blocked@example.com']),
      client: createAPIClient(fetchFn),
      messageId: 42,
      signal: controller.signal
    });
    await vi.waitFor(() => expect(fetchFn).toHaveBeenCalledOnce());
    await Promise.resolve();
    controller.abort();

    await expect(resolution).rejects.toMatchObject({ name: 'AbortError' });
    expect(streamCancelled).toBe(true);
  }, 1_000);
});
