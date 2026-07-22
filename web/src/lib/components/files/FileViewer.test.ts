import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { appShortcuts } from '@kenn-io/kit-ui';

import { createAPIClient } from '../../api/client';
import type { FileSearchRow } from '../../explore/models';
import FileViewer from './FileViewer.svelte';

const { renderPDF } = vi.hoisted(() => ({
  renderPDF: vi.fn(async (_bytes: Uint8Array, host: HTMLElement) => {
    const canvas = document.createElement('canvas');
    canvas.setAttribute('aria-label', 'PDF page 1');
    host.append(canvas);
    return { pages: 1, destroy: vi.fn() };
  })
}));

vi.mock('./FileViewer.browser.svelte', () => ({
  MAX_PDF_BYTES: 25 * 1024 * 1024,
  renderPDF
}));

function file(overrides: Partial<FileSearchRow> = {}): FileSearchRow {
  return {
    id: 7,
    key: 'file:7',
    entry_key: 'message:11',
    message_id: 11,
    conversation_id: 21,
    occurred_at: '2026-07-18T12:00:00Z',
    source_id: 1,
    source_type: 'synthetic',
    source_identifier: 'archive@example.com',
    containing_title: 'Containing item',
    filename: 'preview.png',
    mime_type: 'image/png',
    mime_family: 'image',
    size_bytes: 68,
    content_state: 'local_content',
    content_available: true,
    ...overrides
  };
}

function viewerFetch(metadata: Record<string, unknown>, content = new Uint8Array([137, 80, 78, 71, 13, 10, 26, 10])) {
  return vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    if (new URL(request.url).pathname === '/api/v1/files/7') return Response.json(metadata);
    return new Response(content, { headers: { 'Content-Type': String(metadata.mime_type ?? 'application/octet-stream') } });
  });
}

describe('FileViewer', () => {
  const createObjectURL = vi.fn(() => 'blob:synthetic-preview');
  const revokeObjectURL = vi.fn();

  beforeEach(() => {
    renderPDF.mockClear();
    createObjectURL.mockClear();
    revokeObjectURL.mockClear();
    Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: createObjectURL });
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: revokeObjectURL });
  });

  afterEach(() => vi.useRealTimers());

  it('fetches authenticated image bytes in the shell and revokes the preview URL on close', async () => {
    const opener = document.createElement('button');
    document.body.append(opener);
    opener.focus();
    const onClose = vi.fn();
    const fetchFn = viewerFetch({
      id: 7, message_id: 11, conversation_id: 21, filename: 'preview.png', mime_type: 'image/png',
      size_bytes: 68, content_hash: 'a'.repeat(64), content_state: 'local_content', content_available: true
    });
    render(FileViewer, { client: createAPIClient(fetchFn), file: file(), returnFocus: opener, onClose });

    const image = await screen.findByRole('img', { name: 'Preview preview.png' });
    expect(image.getAttribute('src')).toBe('blob:synthetic-preview');
    expect(fetchFn).toHaveBeenCalledTimes(2);
    await fireEvent.click(screen.getByRole('button', { name: 'Close file viewer' }));
    expect(onClose).toHaveBeenCalledOnce();
    await waitFor(() => expect(document.activeElement).toBe(opener));
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:synthetic-preview');
  });

  it('lets Modal own Escape, suspends background shortcuts, and closes idempotently', async () => {
    const opener = document.createElement('button');
    document.body.append(opener);
    opener.focus();
    const onClose = vi.fn();
    const background = vi.fn();
    const unregister = appShortcuts.register('escape', background);
    const view = render(FileViewer, {
      client: createAPIClient(viewerFetch({
        id: 7, message_id: 11, conversation_id: 21, filename: 'preview.png', mime_type: 'image/png',
        size_bytes: 8, content_hash: 'a'.repeat(64), content_state: 'local_content', content_available: true
      })),
      file: file(), returnFocus: opener, onClose
    });
    await screen.findByRole('img');
    expect(appShortcuts.activeScope()).toBe('file-viewer');
    appShortcuts.handleKeydown(new KeyboardEvent('keydown', { key: 'Escape', cancelable: true }));
    expect(background).not.toHaveBeenCalled();

    const escape = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
    window.dispatchEvent(escape);
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));
    expect(escape.defaultPrevented).toBe(true);
    expect(onClose).toHaveBeenCalledOnce();
    await waitFor(() => expect(document.activeElement).toBe(opener));

    view.unmount();
    expect(appShortcuts.activeScope()).toBe('root');
    unregister();
  });

  it('rejects malformed image bytes before creating a URL', async () => {
    const fetchFn = viewerFetch({
      id: 7, message_id: 11, conversation_id: 21, filename: 'fake.png', mime_type: 'image/png',
      size_bytes: 13, content_hash: 'a'.repeat(64), content_state: 'local_content', content_available: true
    }, new TextEncoder().encode('<html></html>'));
    render(FileViewer, { client: createAPIClient(fetchFn), file: file({ filename: 'fake.png' }) });

    expect((await screen.findByRole('alert')).textContent).toMatch(/image preview was rejected/i);
    expect(createObjectURL).not.toHaveBeenCalled();
  });

  it('revokes a decoded image URL exactly once when the browser reports an image error', async () => {
    const fetchFn = viewerFetch({
      id: 7, message_id: 11, conversation_id: 21, filename: 'broken.png', mime_type: 'image/png',
      size_bytes: 8, content_hash: 'a'.repeat(64), content_state: 'local_content', content_available: true
    });
    render(FileViewer, { client: createAPIClient(fetchFn), file: file({ filename: 'broken.png' }) });
    const image = await screen.findByRole('img');

    await fireEvent.error(image);

    expect((await screen.findByRole('alert')).textContent).toMatch(/browser could not decode/i);
    expect(revokeObjectURL).toHaveBeenCalledTimes(1);
  });

  it('renders PDF bytes through the application renderer instead of a native document element', async () => {
    const fetchFn = viewerFetch({
      id: 7, message_id: 11, conversation_id: 21, filename: 'fixture.pdf', mime_type: 'application/pdf',
      size_bytes: 32, content_hash: 'b'.repeat(64), content_state: 'local_content', content_available: true
    }, new TextEncoder().encode('%PDF-1.4 synthetic'));
    render(FileViewer, {
      client: createAPIClient(fetchFn),
      file: file({ filename: 'fixture.pdf', mime_type: 'application/pdf', mime_family: 'pdf' })
    });

    expect(await screen.findByRole('region', { name: 'PDF preview fixture.pdf' })).toBeDefined();
    await waitFor(() => expect(renderPDF).toHaveBeenCalledOnce());
    expect(document.querySelector('iframe, embed, object')).toBeNull();
  });

  it.each([
    ['missing_blob', 'Archived bytes are missing.'],
    ['metadata_only', 'This attachment has metadata only.'],
    ['url_only', 'This attachment is URL-only and is not fetched as local content.']
  ] as const)('names %s without fetching content bytes', async (contentState, message) => {
    const fetchFn = viewerFetch({
      id: 7, message_id: 11, conversation_id: 21, filename: 'unavailable.bin', mime_type: '',
      size_bytes: 12, content_state: contentState, content_available: false
    });
    render(FileViewer, { client: createAPIClient(fetchFn), file: file({ content_state: contentState, content_available: false }) });
    expect(await screen.findByText(message)).toBeDefined();
    expect(fetchFn).toHaveBeenCalledOnce();
  });

  it('falls back to the attachment ID heading when the filename is empty or whitespace-only', async () => {
    const fetchFn = viewerFetch({
      id: 7, message_id: 11, conversation_id: 21, filename: '   ', mime_type: '',
      size_bytes: 12, content_state: 'missing_blob', content_available: false
    });
    render(FileViewer, {
      client: createAPIClient(fetchFn),
      file: file({ filename: '', content_state: 'missing_blob', content_available: false })
    });

    expect(await screen.findByRole('heading', { name: 'attachment 7' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Close file viewer' })).toBeDefined();
  });

  it('applies the attachment ID fallback to image alt text, download label, and the saved filename', async () => {
    const fetchFn = viewerFetch({
      id: 7, message_id: 11, conversation_id: 21, filename: '', mime_type: 'image/png',
      size_bytes: 8, content_hash: 'a'.repeat(64), content_state: 'local_content', content_available: true
    });
    render(FileViewer, { client: createAPIClient(fetchFn), file: file({ filename: '' }) });

    expect(await screen.findByRole('heading', { name: 'attachment 7' })).toBeDefined();
    expect(await screen.findByRole('img', { name: 'Preview attachment 7' })).toBeDefined();
    const downloadButton = screen.getByRole('button', { name: 'Download attachment 7' });

    let capturedDownload: string | undefined;
    const originalClick = HTMLAnchorElement.prototype.click;
    HTMLAnchorElement.prototype.click = function capture(this: HTMLAnchorElement) {
      capturedDownload = this.download;
    };
    try {
      await fireEvent.click(downloadButton);
      await waitFor(() => expect(capturedDownload).toBe('attachment 7'));
    } finally {
      HTMLAnchorElement.prototype.click = originalClick;
    }
  });

  it('keeps unsupported local content to metadata and an explicit download', async () => {
    const fetchFn = viewerFetch({
      id: 7, message_id: 11, conversation_id: 21, filename: 'archive.zip', mime_type: 'application/zip',
      size_bytes: 12, content_hash: 'c'.repeat(64), content_state: 'local_content', content_available: true
    });
    render(FileViewer, {
      client: createAPIClient(fetchFn),
      file: file({ filename: 'archive.zip', mime_type: 'application/zip', mime_family: 'archive' })
    });
    expect(await screen.findByText('Preview is not supported for this file type.')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Download archive.zip' })).toBeDefined();
    expect(fetchFn).toHaveBeenCalledOnce();
  });

  it('downloads by navigating an anchor at the streaming content endpoint without buffering bytes', async () => {
    const fetchFn = viewerFetch({
      id: 7, message_id: 11, conversation_id: 21, filename: 'archive.zip', mime_type: 'application/zip',
      size_bytes: 12, content_hash: 'c'.repeat(64), content_state: 'local_content', content_available: true
    });
    render(FileViewer, {
      client: createAPIClient(fetchFn),
      file: file({ filename: 'archive.zip', mime_type: 'application/zip', mime_family: 'archive' })
    });
    const downloadButton = await screen.findByRole('button', { name: 'Download archive.zip' });

    let capturedHref: string | undefined;
    let capturedDownload: string | undefined;
    const originalClick = HTMLAnchorElement.prototype.click;
    HTMLAnchorElement.prototype.click = function capture(this: HTMLAnchorElement) {
      capturedHref = this.getAttribute('href') ?? undefined;
      capturedDownload = this.download;
    };
    try {
      await fireEvent.click(downloadButton);
      await waitFor(() => expect(capturedHref).toBe('/api/v1/files/7/content'));
      expect(capturedDownload).toBe('archive.zip');
    } finally {
      HTMLAnchorElement.prototype.click = originalClick;
    }
    expect(fetchFn).toHaveBeenCalledOnce();
  });
});
