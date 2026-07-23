import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import {
  MAX_ARCHIVED_REMOTE_IMAGE_BYTES,
  MAX_ARCHIVED_REMOTE_IMAGE_TOTAL_BYTES,
  MAX_ARCHIVED_REMOTE_IMAGE_URLS,
  resolveArchivedRemoteImages
} from './remote-images';

function fixture(alts: string[]): string {
  return alts.map((alt, index) =>
    `<span data-archived-remote-image="${index}" data-archived-remote-alt="${alt}">` +
    `Remote image not loaded: ${alt}</span>`
  ).join('');
}

function pngResponse(bytes: number): Response {
  return new Response(new Uint8Array(bytes), { headers: { 'Content-Type': 'image/png' } });
}

function parse(html: string): DocumentFragment {
  const template = document.createElement('template');
  template.innerHTML = html;
  return template.content;
}

describe('resolveArchivedRemoteImages', () => {
  it('exports byte and URL budgets aligned with the daemon proxy caps', () => {
    expect(MAX_ARCHIVED_REMOTE_IMAGE_URLS).toBe(64);
    expect(MAX_ARCHIVED_REMOTE_IMAGE_BYTES).toBe(10 * 1024 * 1024);
    expect(MAX_ARCHIVED_REMOTE_IMAGE_TOTAL_BYTES).toBe(30 * 1024 * 1024);
  });

  it('fetches each consented URL through the daemon proxy and embeds data: images', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => pngResponse(16));

    const html = await resolveArchivedRemoteImages({
      html: fixture(['Chart', 'Logo']),
      remoteImages: [
        'https://images.example/chart.png?token=synthetic',
        'https://cdn.example/logo.png'
      ],
      client: createAPIClient(fetchFn),
      signal: new AbortController().signal
    });

    expect(fetchFn).toHaveBeenCalledTimes(2);
    const requested = fetchFn.mock.calls.map((call) => {
      const url = new URL((call[0] as Request).url);
      expect(url.pathname).toBe('/api/v1/content/remote-image');
      return url.searchParams.get('url');
    });
    expect(requested).toEqual([
      'https://images.example/chart.png?token=synthetic',
      'https://cdn.example/logo.png'
    ]);
    const images = [...parse(html).querySelectorAll('img')];
    expect(images.map((image) => image.alt)).toEqual(['Chart', 'Logo']);
    for (const image of images) {
      expect(image.src.startsWith('data:image/png;base64,')).toBe(true);
    }
    // The rendered document never carries the sender URL — not even on the
    // successfully loaded images.
    expect(html).not.toMatch(/images\.example|cdn\.example/);
  });

  it('fetches a repeated URL once and replaces every occurrence', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => pngResponse(16));

    const html = await resolveArchivedRemoteImages({
      html: fixture(['One', 'Two']),
      remoteImages: ['https://images.example/pixel.png', 'https://images.example/pixel.png'],
      client: createAPIClient(fetchFn),
      signal: new AbortController().signal
    });

    expect(fetchFn).toHaveBeenCalledOnce();
    expect(parse(html).querySelectorAll('img')).toHaveLength(2);
  });

  it.each([
    ['HTTP failure', new Response('{"error":"bad_gateway"}', { status: 502 })],
    ['non-image content type', new Response('<html>page</html>', { headers: { 'Content-Type': 'text/html' } })],
    ['SVG content type', new Response('<svg/>', { headers: { 'Content-Type': 'image/svg+xml' } })]
  ])('degrades to a URL-free unavailable placeholder for %s', async (_name, response) => {
    const fetchFn = vi.fn<typeof fetch>(async () => response);

    const html = await resolveArchivedRemoteImages({
      html: fixture(['Chart']),
      remoteImages: ['https://images.example/chart.png'],
      client: createAPIClient(fetchFn),
      signal: new AbortController().signal
    });

    const output = parse(html);
    expect(output.querySelectorAll('img')).toHaveLength(0);
    expect(output.querySelector('[data-archived-image-caption]')?.textContent)
      .toBe('Remote image unavailable: Chart');
    expect(html).not.toContain('images.example');
  });

  it('rejects an image that overruns the per-image decoded byte cap', async () => {
    const oversize = MAX_ARCHIVED_REMOTE_IMAGE_BYTES + 1;
    const fetchFn = vi.fn<typeof fetch>(async () => pngResponse(oversize));

    const html = await resolveArchivedRemoteImages({
      html: fixture(['Huge']),
      remoteImages: ['https://images.example/huge.png'],
      client: createAPIClient(fetchFn),
      signal: new AbortController().signal
    });

    const output = parse(html);
    expect(output.querySelectorAll('img')).toHaveLength(0);
    expect(output.querySelector('[data-archived-image-caption]')?.textContent)
      .toBe('Remote image unavailable: Huge');
  });

  it('fetches nothing without a client and keeps URL-free placeholders', async () => {
    const html = await resolveArchivedRemoteImages({
      html: fixture(['Chart']),
      remoteImages: ['https://images.example/chart.png'],
      client: undefined,
      signal: new AbortController().signal
    });

    const output = parse(html);
    expect(output.querySelectorAll('img')).toHaveLength(0);
    expect(output.querySelector('[data-archived-image-caption]')?.textContent)
      .toBe('Remote image unavailable: Chart');
    expect(html).not.toContain('images.example');
  });

  it('ignores placeholders whose index has no consented URL', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => pngResponse(16));

    const html = await resolveArchivedRemoteImages({
      html: '<span data-archived-remote-image="7" data-archived-remote-alt="Forged">x</span>' +
        '<span data-archived-remote-image="junk" data-archived-remote-alt="Junk">y</span>',
      remoteImages: ['https://images.example/chart.png'],
      client: createAPIClient(fetchFn),
      signal: new AbortController().signal
    });

    expect(fetchFn).not.toHaveBeenCalled();
    const captions = [...parse(html).querySelectorAll('[data-archived-image-caption]')]
      .map((caption) => caption.textContent);
    expect(captions).toEqual(['Remote image unavailable: Forged', 'Remote image unavailable: Junk']);
  });

  it('caps the number of unique proxied URLs', async () => {
    const count = MAX_ARCHIVED_REMOTE_IMAGE_URLS + 5;
    const alts = Array.from({ length: count }, (_value, index) => `Image ${index}`);
    const urls = Array.from({ length: count }, (_value, index) => `https://images.example/${index}.png`);
    const fetchFn = vi.fn<typeof fetch>(async () => pngResponse(8));

    const html = await resolveArchivedRemoteImages({
      html: fixture(alts),
      remoteImages: urls,
      client: createAPIClient(fetchFn),
      signal: new AbortController().signal
    });

    expect(fetchFn).toHaveBeenCalledTimes(MAX_ARCHIVED_REMOTE_IMAGE_URLS);
    expect(parse(html).querySelectorAll('img')).toHaveLength(MAX_ARCHIVED_REMOTE_IMAGE_URLS);
  });

  it('throws an AbortError when the signal aborts mid-resolution', async () => {
    const controller = new AbortController();
    const fetchFn = vi.fn<typeof fetch>(async () => {
      controller.abort();
      return pngResponse(16);
    });

    await expect(resolveArchivedRemoteImages({
      html: fixture(['Chart']),
      remoteImages: ['https://images.example/chart.png'],
      client: createAPIClient(fetchFn),
      signal: controller.signal
    })).rejects.toMatchObject({ name: 'AbortError' });
  });
});
