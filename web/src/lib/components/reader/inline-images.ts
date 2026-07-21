import type { APIClient } from '../../api/client';
import type { ArchivedInlineImage } from '../../content/sanitize';

export const MAX_ARCHIVED_INLINE_IMAGE_CIDS = 32;
export const MAX_ARCHIVED_INLINE_IMAGE_BYTES = 5 * 1024 * 1024;
export const MAX_ARCHIVED_INLINE_IMAGE_TOTAL_BYTES = 20 * 1024 * 1024;
export const MAX_ARCHIVED_INLINE_IMAGE_CONCURRENCY = 1;
export const MAX_ARCHIVED_INLINE_IMAGE_OCCURRENCES = 128;
export const MAX_ARCHIVED_INLINE_IMAGE_SERIALIZED_BYTES = 24 * 1024 * 1024;

const INLINE_IMAGE_TYPES = new Set(['image/gif', 'image/jpeg', 'image/png', 'image/webp']);

interface InlineOccurrence {
  alt: string;
  cid: string | undefined;
  placeholder: HTMLElement;
}

interface InlineGroup {
  cid: string;
  bytes?: Uint8Array;
  mimeType?: string;
  dataURL?: string;
}

interface DecodedByteBudget {
  used: number;
}

interface InlineImagePublicationLimits {
  occurrences?: number;
  dataURLBytes?: number;
}

function abortError(): DOMException {
  return new DOMException('Aborted', 'AbortError');
}

function throwIfAborted(signal: AbortSignal): void {
  if (signal.aborted) throw abortError();
}

function normalizedCID(value: string): string {
  let cid = value.trim();
  if (cid.startsWith('<') && cid.endsWith('>')) cid = cid.slice(1, -1).trim();
  return cid;
}

function hardBoundedLimit(value: number | undefined, hardLimit: number): number {
  if (value === undefined) return hardLimit;
  if (!Number.isFinite(value) || value <= 0) return 0;
  return Math.min(Math.floor(value), hardLimit);
}

function unavailableInlineImage(alt: string): HTMLElement {
  const placeholder = document.createElement('span');
  placeholder.textContent = `Inline image unavailable${alt ? `: ${alt}` : ''}`;
  return placeholder;
}

function bytesToDataURL(bytes: Uint8Array, mimeType: string): string {
  let binary = '';
  const chunkSize = 0x8000;
  for (let offset = 0; offset < bytes.length; offset += chunkSize) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + chunkSize));
  }
  return `data:${mimeType};base64,${btoa(binary)}`;
}

async function readBoundedStream(
  stream: ReadableStream<Uint8Array>,
  budget: DecodedByteBudget,
  signal: AbortSignal
): Promise<Uint8Array> {
  const reader = stream.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  const cancelOnAbort = (): void => {
    void reader.cancel(abortError()).catch(() => undefined);
  };
  signal.addEventListener('abort', cancelOnAbort, { once: true });
  try {
    while (true) {
      if (signal.aborted) {
        await reader.cancel();
        throw abortError();
      }
      const { done, value } = await reader.read();
      if (done) break;
      const nextImageTotal = total + value.byteLength;
      const nextAggregateTotal = budget.used + value.byteLength;
      if (nextImageTotal > MAX_ARCHIVED_INLINE_IMAGE_BYTES ||
        nextAggregateTotal > MAX_ARCHIVED_INLINE_IMAGE_TOTAL_BYTES) {
        budget.used = Math.min(MAX_ARCHIVED_INLINE_IMAGE_TOTAL_BYTES, nextAggregateTotal);
        await reader.cancel();
        throw new Error('Inline image exceeds decoded byte budget');
      }
      total = nextImageTotal;
      budget.used = nextAggregateTotal;
      chunks.push(value);
    }
  } finally {
    signal.removeEventListener('abort', cancelOnAbort);
    reader.releaseLock();
  }
  const bytes = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return bytes;
}

async function fetchInlineImage(
  client: APIClient,
  messageId: number,
  cid: string,
  budget: DecodedByteBudget,
  signal: AbortSignal
): Promise<{ bytes: Uint8Array; mimeType: string }> {
  throwIfAborted(signal);
  const { data, response } = await client.GET('/api/v1/messages/{id}/inline', {
    params: { path: { id: messageId }, query: { cid } },
    parseAs: 'stream',
    signal
  });
  if (signal.aborted) {
    if (data instanceof ReadableStream) await data.cancel();
    throw abortError();
  }
  if (!response.ok || !(data instanceof ReadableStream)) throw new Error('Inline image unavailable');
  const mimeType = (response.headers.get('Content-Type') ?? '').split(';', 1)[0]!.trim().toLowerCase();
  if (!INLINE_IMAGE_TYPES.has(mimeType)) {
    await data.cancel();
    throw new Error('Inline image type is not permitted');
  }
  const contentLength = response.headers.get('Content-Length');
  const remainingBytes = MAX_ARCHIVED_INLINE_IMAGE_TOTAL_BYTES - budget.used;
  if (contentLength !== null) {
    const declaredSize = Number(contentLength);
    if (!Number.isFinite(declaredSize) || declaredSize < 0 ||
      declaredSize > MAX_ARCHIVED_INLINE_IMAGE_BYTES || declaredSize > remainingBytes) {
      await data.cancel();
      throw new Error('Inline image exceeds decoded byte budget');
    }
  }
  const bytes = await readBoundedStream(data, budget, signal);
  throwIfAborted(signal);
  return { bytes, mimeType };
}

export async function resolveArchivedInlineImages(options: {
  html: string;
  inlineImages: ArchivedInlineImage[];
  client: APIClient | undefined;
  messageId: number;
  signal: AbortSignal;
  publicationLimits?: InlineImagePublicationLimits;
}): Promise<string> {
  const template = document.createElement('template');
  template.innerHTML = options.html;
  const placeholders = new Map<number, HTMLElement>();
  for (const element of template.content.querySelectorAll<HTMLElement>('[data-archived-inline-image]')) {
    const index = Number(element.dataset.archivedInlineImage);
    if (Number.isSafeInteger(index) && index >= 0) placeholders.set(index, element);
  }

  const groups = new Map<string, InlineGroup>();
  const orderedOccurrences: InlineOccurrence[] = [];
  options.inlineImages.forEach((inline, index) => {
    const loadingPlaceholder = placeholders.get(index);
    if (!loadingPlaceholder) return;
    const placeholder = unavailableInlineImage(inline.alt);
    loadingPlaceholder.replaceWith(placeholder);
    const cid = normalizedCID(inline.cid);
    let admittedCID: string | undefined;
    if (cid && groups.has(cid)) admittedCID = cid;
    else if (cid && groups.size < MAX_ARCHIVED_INLINE_IMAGE_CIDS) {
      groups.set(cid, { cid });
      admittedCID = cid;
    }
    orderedOccurrences.push({ alt: inline.alt, cid: admittedCID, placeholder });
  });

  const budget: DecodedByteBudget = { used: 0 };
  for (const group of groups.values()) {
    throwIfAborted(options.signal);
    if (!options.client || budget.used >= MAX_ARCHIVED_INLINE_IMAGE_TOTAL_BYTES) continue;
    try {
      const decoded = await fetchInlineImage(
        options.client,
        options.messageId,
        group.cid,
        budget,
        options.signal
      );
      group.bytes = decoded.bytes;
      group.mimeType = decoded.mimeType;
    } catch (error) {
      if (options.signal.aborted || (error instanceof DOMException && error.name === 'AbortError')) {
        throw abortError();
      }
    }
  }
  const maxPublishedOccurrences = hardBoundedLimit(
    options.publicationLimits?.occurrences,
    MAX_ARCHIVED_INLINE_IMAGE_OCCURRENCES
  );
  const maxSerializedBytes = hardBoundedLimit(
    options.publicationLimits?.dataURLBytes,
    MAX_ARCHIVED_INLINE_IMAGE_SERIALIZED_BYTES
  );
  let publishedOccurrences = 0;
  let serializedBytes = 0;
  for (const occurrence of orderedOccurrences) {
    if (publishedOccurrences >= maxPublishedOccurrences) break;
    const group = occurrence.cid ? groups.get(occurrence.cid) : undefined;
    if (group && !group.dataURL && group.bytes && group.mimeType) {
      group.dataURL = bytesToDataURL(group.bytes, group.mimeType);
      group.bytes = undefined;
    }
    const dataURL = group?.dataURL;
    if (!dataURL) continue;
    // Data URLs produced above are ASCII, so string length is also their exact
    // UTF-8 serialized byte charge, including MIME prefix and base64 expansion.
    if (serializedBytes + dataURL.length > maxSerializedBytes) continue;
    const image = document.createElement('img');
    image.alt = occurrence.alt;
    image.src = dataURL;
    occurrence.placeholder.replaceWith(image);
    publishedOccurrences += 1;
    serializedBytes += dataURL.length;
  }
  throwIfAborted(options.signal);
  return template.innerHTML;
}
