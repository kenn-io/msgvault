import type { APIClient } from '../../api/client';
import { imagePlaceholderBlock, inertTemplate } from '../../content/sanitize';
import {
  abortError,
  ARCHIVED_IMAGE_TYPES,
  bytesToDataURL,
  readBoundedStream,
  throwIfAborted,
  type DecodedByteBudget
} from './inline-images';

// Consented remote images are fetched by the authenticated shell through the
// daemon's SSRF-hardened proxy (GET /api/v1/content/remote-image) and
// injected into the archived document as data: URIs. The srcdoc frame's CSP
// therefore allowlists no remote origin, and the browser never contacts a
// sender-controlled host — the daemon validates every resolved address and
// redirect hop server-side, which also closes DNS rebinding.
export const MAX_ARCHIVED_REMOTE_IMAGE_URLS = 64;
export const MAX_ARCHIVED_REMOTE_IMAGE_BYTES = 10 * 1024 * 1024;
export const MAX_ARCHIVED_REMOTE_IMAGE_TOTAL_BYTES = 30 * 1024 * 1024;

interface RemoteGroup {
  url: string;
  dataURL?: string;
}

function unavailableRemoteImage(alt: string): HTMLElement {
  return imagePlaceholderBlock(document, `Remote image unavailable${alt ? `: ${alt}` : ''}`);
}

async function fetchRemoteImage(
  client: APIClient,
  url: string,
  budget: DecodedByteBudget,
  signal: AbortSignal
): Promise<string> {
  throwIfAborted(signal);
  const { data, response } = await client.GET('/api/v1/content/remote-image', {
    params: { query: { url } },
    parseAs: 'stream',
    signal
  });
  if (signal.aborted) {
    if (data instanceof ReadableStream) await data.cancel();
    throw abortError();
  }
  if (!response.ok || !(data instanceof ReadableStream)) throw new Error('Remote image unavailable');
  const mimeType = (response.headers.get('Content-Type') ?? '').split(';', 1)[0]!.trim().toLowerCase();
  if (!ARCHIVED_IMAGE_TYPES.has(mimeType)) {
    await data.cancel();
    throw new Error('Remote image type is not permitted');
  }
  const bytes = await readBoundedStream(data, budget, signal, {
    imageBytes: MAX_ARCHIVED_REMOTE_IMAGE_BYTES,
    totalBytes: MAX_ARCHIVED_REMOTE_IMAGE_TOTAL_BYTES
  });
  throwIfAborted(signal);
  return bytesToDataURL(bytes, mimeType);
}

/**
 * Replaces the sanitizer's indexed remote-image placeholders after explicit
 * consent: each approved URL is fetched once through the daemon proxy and
 * every occurrence becomes an embedded data: image. Failed fetches (and
 * URLs beyond the caps) degrade to a URL-free unavailable placeholder, so
 * the archived DOM never carries a sender URL either way.
 */
export async function resolveArchivedRemoteImages(options: {
  html: string;
  remoteImages: string[];
  client: APIClient | undefined;
  signal: AbortSignal;
}): Promise<string> {
  // The placeholders are URL-free, but the reassembled document may carry
  // data: images from earlier passes — keep the parse inert regardless.
  const template = inertTemplate(options.html);
  const occurrences: Array<{ placeholder: HTMLElement; url: string | undefined; alt: string }> = [];
  const groups = new Map<string, RemoteGroup>();
  for (const element of template.content.querySelectorAll<HTMLElement>('[data-archived-remote-image]')) {
    const index = Number(element.getAttribute('data-archived-remote-image'));
    const url = Number.isSafeInteger(index) && index >= 0
      ? options.remoteImages[index]
      : undefined;
    let admittedURL: string | undefined;
    if (url !== undefined && (groups.has(url) || groups.size < MAX_ARCHIVED_REMOTE_IMAGE_URLS)) {
      if (!groups.has(url)) groups.set(url, { url });
      admittedURL = url;
    }
    occurrences.push({
      placeholder: element,
      url: admittedURL,
      alt: element.getAttribute('data-archived-remote-alt') ?? ''
    });
  }

  const budget: DecodedByteBudget = { used: 0 };
  for (const group of groups.values()) {
    throwIfAborted(options.signal);
    if (!options.client || budget.used >= MAX_ARCHIVED_REMOTE_IMAGE_TOTAL_BYTES) continue;
    try {
      group.dataURL = await fetchRemoteImage(options.client, group.url, budget, options.signal);
    } catch (error) {
      if (options.signal.aborted || (error instanceof DOMException && error.name === 'AbortError')) {
        throw abortError();
      }
    }
  }

  for (const occurrence of occurrences) {
    const dataURL = occurrence.url ? groups.get(occurrence.url)?.dataURL : undefined;
    if (dataURL === undefined) {
      occurrence.placeholder.replaceWith(unavailableRemoteImage(occurrence.alt));
      continue;
    }
    const image = document.createElement('img');
    image.alt = occurrence.alt;
    image.src = dataURL;
    occurrence.placeholder.replaceWith(image);
  }
  throwIfAborted(options.signal);
  return template.innerHTML;
}
