import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { appShortcuts } from '@kenn-io/kit-ui';

import { createAPIClient } from '../../api/client';
import type { FileSearchRow } from '../../explore/models';
import { KataReadiness, kataReadinessKey } from '../../kata/kata-ready.svelte';
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
    key: 'source:1:message:m-11:file:7',
    entry_key: 'source:1:message:m-11',
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

  it('closes only the top Kata dialog on Escape, leaving the file open', async () => {
    const onClose = vi.fn();
    const metadata = {
      id: 7, message_id: 11, conversation_id: 21, filename: 'notes.txt', mime_type: 'image/png',
      size_bytes: 8, content_hash: 'a'.repeat(64), content_state: 'local_content', content_available: true
    };
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/status')) return Response.json({ state: 'ready', project: 'example', message: 'Ready' });
      if (path === '/api/v1/files/7') return Response.json(metadata);
      if (path.endsWith('/documents/search')) return Response.json({ results: [
        { attachment_id: 7, message_id: 11, extraction_id: 'extract-1', chunk_key: 'chunk-0', excerpt: 'Send the budget', excerpt_start_rune: 0, highlight_start: 0, highlight_end: 4, matched_signals: ['content'] },
      ] });
      if (path.endsWith('/evidence/prepare')) return new Promise<Response>(() => undefined);
      return new Response(new Uint8Array([137, 80, 78, 71, 13, 10, 26, 10]), { headers: { 'Content-Type': 'image/png' } });
    }));
    render(FileViewer, { props: { client, file: file(), onClose }, context: new Map([[kataReadinessKey, new KataReadiness(client)]]) });
    const escape = () => window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));

    await fireEvent.click(await screen.findByRole('button', { name: 'Add to task…' }));
    expect(screen.getByRole('dialog', { name: 'Add to task' })).toBeTruthy();
    escape();
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Add to task' })).toBeNull());
    expect(onClose).not.toHaveBeenCalled();

    await fireEvent.click(screen.getByRole('button', { name: 'Add to task…' }));
    await fireEvent.input(screen.getByLabelText('Search this file'), { target: { value: 'budget' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Search' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Use quote 1' }));
    expect(await screen.findByRole('dialog', { name: 'Kata issue' })).toBeTruthy();
    escape();
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Kata issue' })).toBeNull());
    expect(onClose).not.toHaveBeenCalled();

    escape();
    expect(onClose).toHaveBeenCalledOnce();
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
    await screen.findByText("PDF · 32 B · 1 page");
  });

  it.each([
    ['missing_blob', "The archive has this file's record, but its bytes are missing from storage."],
    ['metadata_only', "The import saved this file's name and size, not its contents."],
    ['url_only', "This file is a link, and msgvault didn't download it."]
  ] as const)('names %s without fetching content bytes', async (contentState, message) => {
    const fetchFn = viewerFetch({
      id: 7, message_id: 11, conversation_id: 21, filename: 'unavailable.bin', mime_type: '',
      size_bytes: 12, content_state: contentState, content_available: false
    });
    render(FileViewer, { client: createAPIClient(fetchFn), file: file({ content_state: contentState, content_available: false }) });
    expect(await screen.findByText(message)).toBeDefined();
    expect(fetchFn).toHaveBeenCalledOnce();
    expect(screen.queryByRole('button', { name: /Download/ })).toBeNull();
    expect(screen.getByRole('button', { name: 'Open message' }).className).toContain('solid');
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

    expect(await screen.findByRole('dialog', { name: 'View attachment 7' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Close file viewer' })).toBeDefined();
  });

  it('applies the attachment ID fallback to image alt text, download label, and the saved filename', async () => {
    const fetchFn = viewerFetch({
      id: 7, message_id: 11, conversation_id: 21, filename: '', mime_type: 'image/png',
      size_bytes: 8, content_hash: 'a'.repeat(64), content_state: 'local_content', content_available: true
    });
    render(FileViewer, { client: createAPIClient(fetchFn), file: file({ filename: '' }) });

    expect(await screen.findByRole('dialog', { name: 'View attachment 7' })).toBeDefined();
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

  it('deep links a metadata-only target through the canonical entry key returned by the endpoint', async () => {
    const onOpenItem = vi.fn();
    const onOpenConversation = vi.fn();
    const fetchFn = viewerFetch({
      id: 7, message_id: 11, conversation_id: 21, entry_key: 'source:1:message:m-11',
      filename: 'report.pdf', mime_type: 'application/pdf', size_bytes: 12,
      content_state: 'metadata_only', content_available: false
    });
    render(FileViewer, { client: createAPIClient(fetchFn), file: { id: 7 }, onOpenItem, onOpenConversation });
    await screen.findByText("The import saved this file's name and size, not its contents.");

    await fireEvent.click(screen.getByRole('button', { name: 'Open message' }));
    expect(onOpenItem).toHaveBeenCalledExactlyOnceWith('source:1:message:m-11');

    await fireEvent.click(screen.getByRole('button', { name: 'Open conversation' }));
    expect(onOpenConversation).toHaveBeenCalledExactlyOnceWith('source:1:message:m-11', 11, 21);
    expect(fetchFn).toHaveBeenCalledOnce();
  });

  it('prefers the authoritative metadata entry key over the listing row key', async () => {
    const onOpenItem = vi.fn();
    const fetchFn = viewerFetch({
      id: 7, message_id: 11, conversation_id: 21, entry_key: 'source:1:conversation:21',
      filename: 'photo.png', mime_type: 'image/png', size_bytes: 12,
      content_state: 'metadata_only', content_available: false
    });
    render(FileViewer, {
      client: createAPIClient(fetchFn),
      file: file({ content_state: 'metadata_only', content_available: false }),
      onOpenItem
    });
    await screen.findByText("The import saved this file's name and size, not its contents.");

    await fireEvent.click(screen.getByRole('button', { name: 'Open message' }));
    expect(onOpenItem).toHaveBeenCalledExactlyOnceWith('source:1:conversation:21');
  });

  it('clears loading and shows an error when the metadata request fails', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => {
      throw new TypeError('network unreachable');
    });
    render(FileViewer, { client: createAPIClient(fetchFn), file: file() });

    expect((await screen.findByRole('alert')).textContent).toContain('network unreachable');
    expect(screen.queryByText('Loading file…')).toBeNull();
  });

  it('ignores a stale metadata failure after the viewed file changes', async () => {
    let rejectStale: ((cause: Error) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>((input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname === '/api/v1/files/7') {
        return new Promise<Response>((_resolve, reject) => { rejectStale = reject; });
      }
      return Promise.resolve(Response.json({
        id: 8, message_id: 12, conversation_id: 22, filename: 'next.bin', mime_type: '',
        size_bytes: 4, content_state: 'missing_blob', content_available: false
      }));
    });
    const view = render(FileViewer, { client: createAPIClient(fetchFn), file: file() });
    await waitFor(() => expect(rejectStale).toBeDefined());

    await view.rerender({
      file: file({ id: 8, key: 'file:8', filename: 'next.bin', content_state: 'missing_blob', content_available: false })
    });
    rejectStale!(new TypeError('stale network failure'));

    expect(await screen.findByText("The archive has this file's record, but its bytes are missing from storage.")).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByText('stale network failure')).toBeNull();
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
    expect(await screen.findByText('No preview for this file type')).toBeDefined();
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
    await waitFor(() => expect(downloadButton.hasAttribute('disabled')).toBe(false));

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

it('shows context and steps with buttons and arrows, including from the preview', async () => {
  const onStep = vi.fn();
  render(FileViewer, { client: createAPIClient(viewerFetch({ ...file(), mime_type: 'application/zip' })), file: file({ participant_labels: ['Alex Example', 'Jordan Example'] }), position: { index: 2, total: 1038 }, onStep });
  await screen.findByText('3 of 1,038');
  expect(screen.getByText(/Containing item · Alex Example, Jordan Example/)).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Next file' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Previous file' }));
  await fireEvent.keyDown(screen.getByRole('region', { name: 'File preview preview.png' }), { key: 'ArrowRight' });
  await fireEvent.keyDown(window, { key: 'ArrowLeft' });
  expect(onStep.mock.calls).toEqual([[1], [-1], [1], [-1]]);
});

it.each([
  ['text/plain', 'notes.txt', 'Synthetic text', 'pre'],
  ['text/csv', 'table.csv', 'Name,Value\nExample,42', 'table'],
  ['text/tab-separated-values', 'table.tsv', 'Name\tValue\nExample\t42', 'table'],
])('previews %s', async (mime_type, filename, content, tag) => {
  render(FileViewer, { client: createAPIClient(viewerFetch({ ...file(), mime_type, filename }, new TextEncoder().encode(content))), file: file({ filename, mime_type }) });
  await waitFor(() => expect(document.querySelector(tag)?.textContent).toContain(tag === 'pre' ? 'Synthetic text' : 'Example'));
});

it('bounds large CSV previews to 200 rows', async () => {
  const content = 'Name,Value\n' + Array.from({ length: 205 }, (_, index) => `Row ${index},${index}`).join('\n');
  render(FileViewer, { client: createAPIClient(viewerFetch({ ...file(), mime_type: 'text/csv', size_bytes: 5 * 1024 * 1024 }, new TextEncoder().encode(content))), file: file() });
  await screen.findByText('Showing the first 200 rows.');
  expect(screen.getAllByRole('row')).toHaveLength(201);
  expect(screen.queryByText('Row 200')).toBeNull();
});

it('buffers authenticated audio for seeking and revokes its blob URL on close', async () => {
  const onStep = vi.fn();
  const create = vi.fn((_blob: Blob) => 'blob:audio-preview');
  const revoke = vi.fn();
  Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: create });
  Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: revoke });
  const fetchFn = viewerFetch({ ...file(), mime_type: 'audio/wav' });
  render(FileViewer, { client: createAPIClient(fetchFn), file: file(), position: { index: 0, total: 2 }, onStep });
  await waitFor(() => expect(document.querySelector('audio')?.getAttribute('src')).toBe('blob:audio-preview'));
  expect(fetchFn).toHaveBeenCalledTimes(2);
  expect(create.mock.calls[0]?.[0]).toBeInstanceOf(Blob);
  await fireEvent.keyDown(document.querySelector('audio')!, { key: 'ArrowRight' });
  expect(onStep).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole('button', { name: 'Close file viewer' }));
  expect(revoke).toHaveBeenCalledExactlyOnceWith('blob:audio-preview');
});

it('names spreadsheets and keeps Download in the footer', async () => {
  render(FileViewer, { client: createAPIClient(viewerFetch({ ...file(), mime_type: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet' })), file: file() });
  await screen.findByText('No preview for this file type');
  expect(screen.getByText(/Excel spreadsheet.*Download it/)).toBeDefined();
  expect(screen.getAllByRole('button', { name: /Download/ })).toHaveLength(1);
});

it('leaves arrows with a horizontally scrollable text view and editable controls', async () => {
  const onStep = vi.fn();
  render(FileViewer, { client: createAPIClient(viewerFetch({ ...file(), mime_type: 'text/plain' }, new TextEncoder().encode('Example text'))), file: file(), position: { index: 0, total: 2 }, onStep });
  await screen.findByText('Example text');
  const preview = screen.getByRole('region', { name: 'File preview preview.png' });
  Object.defineProperty(preview, 'scrollWidth', { configurable: true, value: 400 });
  Object.defineProperty(preview, 'clientWidth', { configurable: true, value: 200 });
  await fireEvent.keyDown(preview, { key: 'ArrowRight' });
  const input = document.createElement('input');
  preview.append(input);
  await fireEvent.keyDown(input, { key: 'ArrowRight' });
  expect(onStep).not.toHaveBeenCalled();
  Object.defineProperty(preview, 'scrollWidth', { value: 200 });
  await fireEvent.keyDown(preview, { key: 'ArrowRight' });
  expect(onStep).toHaveBeenCalledExactlyOnceWith(1);
});

it('renders the start of a text file beyond the byte limit', async () => {
  const content = new Uint8Array(2 * 1024 * 1024 + 50).fill(65);
  render(FileViewer, { client: createAPIClient(viewerFetch({ ...file(), mime_type: 'text/plain', size_bytes: content.length }, content)), file: file() });
  await screen.findByText('Showing the start of this file.');
  expect(document.querySelector('pre')?.textContent?.length).toBe(2 * 1024 * 1024);
  expect(screen.queryByRole('alert')).toBeNull();
});

it('keeps an exact-limit text file complete', async () => {
  const content = new Uint8Array(2 * 1024 * 1024).fill(65);
  render(FileViewer, { client: createAPIClient(viewerFetch({ ...file(), mime_type: 'text/plain', size_bytes: content.length }, content)), file: file() });
  await waitFor(() => expect(document.querySelector('pre')?.textContent?.length).toBe(content.length));
  expect(screen.queryByText('Showing the start of this file.')).toBeNull();
});
it.each([
  ['table.csv', 'application/vnd.ms-excel', 'Name,Value\nExample,42', 'table'],
  ['table.tsv', 'application/octet-stream', 'Name\tValue\nExample\t42', 'table'],
  ['notes.txt', 'application/x-unknown', 'Example text', 'pre'],
])('previews a generic MIME using %s', async (filename, mime_type, content, tag) => {
  render(FileViewer, { client: createAPIClient(viewerFetch({ ...file(), filename, mime_type }, new TextEncoder().encode(content))), file: file() });
  await waitFor(() => expect(document.querySelector(tag)?.textContent).toContain('Example'));
});
it('uses the response charset for the rendered text', async () => {
  render(FileViewer, { client: createAPIClient(viewerFetch({ ...file(), filename: 'notes.txt', mime_type: 'text/plain; charset=windows-1252' }, new Uint8Array([0x43, 0x61, 0x66, 0xe9]))), file: file() });
  expect(await screen.findByText('Café')).toBeDefined();
});
it('keeps Download disabled while availability is unknown', async () => {
  render(FileViewer, { client: createAPIClient(() => new Promise<Response>(() => undefined)), file: file() });
  expect(screen.getByRole('button', { name: 'Download preview.png' }).hasAttribute('disabled')).toBe(true);
  expect(screen.getByText('Loading file…')).toBeDefined();
  expect(screen.getByRole('button', { name: 'Open message' }).className).toContain('outline');
});
