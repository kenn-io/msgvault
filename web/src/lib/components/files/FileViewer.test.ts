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
    return { pages: 1, setZoom: vi.fn(), destroy: vi.fn() };
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
    content_state: 'unverified',
    content_available: false,
    ...overrides
  };
}

function viewerFetch(metadata: Record<string, unknown>, content = new Uint8Array([137, 80, 78, 71, 13, 10, 26, 10]), contentType = String(metadata.mime_type ?? 'application/octet-stream')) {
  return vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    if (new URL(request.url).pathname === '/api/v1/files/7') return Response.json(metadata.content_state === 'unverified' ? { ...metadata, content_state: 'local_content', content_available: true } : metadata);
    return new Response(content, { headers: { 'Content-Type': contentType } });
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
    await screen.findByText("PDF · 32 B");
  });

  it.each([
    ['missing_blob', "The archive has this file's record, but its bytes are missing."],
    ['metadata_only', "The import saved this file's name and size, not its contents."],
    ['url_only', "This file is a link, and msgvault didn't download it."]
  ] as const)('names %s without fetching content bytes', async (contentState, message) => {
    const fetchFn = viewerFetch({
      id: 7, message_id: 11, conversation_id: 21, filename: 'unavailable.bin', mime_type: '',
      size_bytes: 12, content_state: contentState, content_available: false
    });
    render(FileViewer, { client: createAPIClient(fetchFn), file: file({ filename: 'unavailable.bin', mime_type: '', size_bytes: 12, content_state: contentState === 'missing_blob' ? 'unverified' : contentState, content_available: false }) });
    expect(await screen.findByText(message)).toBeDefined();
    expect(fetchFn).toHaveBeenCalledOnce();
    expect(screen.getByRole('button', { name: /Download/ }).hasAttribute('disabled')).toBe(true);
  });

  it('falls back to the attachment ID heading when the filename is empty or whitespace-only', async () => {
    const fetchFn = viewerFetch({
      id: 7, message_id: 11, conversation_id: 21, filename: '   ', mime_type: '',
      size_bytes: 12, content_state: 'missing_blob', content_available: false
    });
    render(FileViewer, {
      client: createAPIClient(fetchFn),
      file: file({ filename: '   ', mime_type: '', size_bytes: 12, content_state: 'unverified', content_available: false })
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

    await fireEvent.click(screen.getByRole('button', { name: 'More file actions' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Open conversation' }));
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

  it('shows a storage error rather than a missing-file notice', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ error: 'internal_error', message: 'Failed to open file content' }, { status: 500 }));
    render(FileViewer, { client: createAPIClient(fetchFn), file: file() });

    expect((await screen.findByRole('alert')).textContent).toContain('Failed to open file content');
    expect(screen.queryByText("This file's contents aren't in the archive")).toBeNull();
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
      file: file({ id: 8, key: 'file:8', filename: 'next.bin', content_state: 'unverified', content_available: false })
    });
    rejectStale!(new TypeError('stale network failure'));

    expect(await screen.findByText("The archive has this file's record, but its bytes are missing.")).toBeDefined();
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
    expect(screen.getByText('Download it to open in another app.')).toBeDefined();
    expect(screen.getAllByRole('button', { name: /Download/ })).toHaveLength(1);
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

it('shows context and steps with buttons and arrows', async () => {
  const mime_type = 'application/zip';
  const onStep = vi.fn();
  render(FileViewer, { client: createAPIClient(viewerFetch({ ...file(), mime_type }, new TextEncoder().encode('Example text'))), file: file({ mime_type, participant_labels: ['Alex Example', 'Jordan Example'] }), position: { index: 2, total: 1038 }, onStep });
  await screen.findByText('3 of 1,038');
  expect(screen.getByText(/Containing item · Alex Example, Jordan Example/)).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Next file' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Previous file' }));
  await fireEvent.keyDown(screen.getByRole('region', { name: 'File preview preview.png' }), { key: 'ArrowRight' });
  await fireEvent.keyDown(window, { key: 'ArrowLeft' });
  expect(onStep.mock.calls).toEqual([[1], [-1], [1], [-1]]);
});

it.each([
  { mime_type: 'text/plain', filename: 'notes.txt', content: 'Synthetic text', tag: 'pre', want: 'Synthetic text', target: { filename: 'notes.txt', mime_type: 'text/plain' } },
  { mime_type: 'text/csv', filename: 'table.csv', content: 'Name,Value\nExample,42', tag: 'table', want: 'Example', target: { filename: 'table.csv', mime_type: 'text/csv' } },
  { mime_type: 'text/tab-separated-values', filename: 'table.tsv', content: 'Name\tValue\nExample\t42', tag: 'table', want: 'Example', target: { filename: 'table.tsv', mime_type: 'text/tab-separated-values' } },
  { mime_type: 'application/vnd.ms-excel', filename: 'table.csv', content: 'Name,Value\nExample,42', tag: 'table', want: 'Example', target: { filename: 'table.csv', mime_type: 'application/vnd.ms-excel' } },
  { mime_type: 'application/octet-stream', filename: 'table.tsv', content: 'Name\tValue\nExample\t42', tag: 'table', want: 'Example', target: { filename: 'table.tsv', mime_type: 'application/octet-stream' } },
  { mime_type: 'application/x-unknown', filename: 'notes.txt', content: 'Example text', tag: 'pre', want: 'Example', target: { filename: 'notes.txt', mime_type: 'application/x-unknown' } },
])('previews $mime_type from $filename', async ({ mime_type, filename, content, tag, want, target }) => {
  render(FileViewer, { client: createAPIClient(viewerFetch({ ...file(), mime_type, filename }, new TextEncoder().encode(content))), file: file(target) });
  await waitFor(() => expect(document.querySelector(tag)?.textContent).toContain(want));
});

it('buffers authenticated audio for seeking and revokes its blob URL on close', async () => {
  const create = vi.fn((_blob: Blob) => 'blob:audio-preview');
  const revoke = vi.fn();
  Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: create });
  Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: revoke });
  const audioFile = file({ filename: 'sound.wav', mime_type: 'audio/wav' });
  const fetchFn = viewerFetch(audioFile);
  render(FileViewer, { client: createAPIClient(fetchFn), file: audioFile });
  await waitFor(() => expect(document.querySelector('audio')?.getAttribute('src')).toBe('blob:audio-preview'));
  expect(fetchFn).toHaveBeenCalledTimes(2);
  expect(create.mock.calls[0]?.[0]).toBeInstanceOf(Blob);
  await fireEvent.click(screen.getByRole('button', { name: 'Close file viewer' }));
  expect(revoke).toHaveBeenCalledExactlyOnceWith('blob:audio-preview');
});

it.each(['scroll and input', 'audio', 'menu ArrowLeft'])('leaves arrows with %s', async (kind) => {
  const onStep = vi.fn();
  const mime_type = kind === 'audio' ? 'audio/wav' : kind.startsWith('menu') ? 'application/zip' : 'text/plain';
  if (kind === 'audio') Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: vi.fn(() => 'blob:audio-preview') });
  const fetchFn = kind === 'audio' ? viewerFetch({ ...file(), mime_type }) : viewerFetch({ ...file(), mime_type }, new TextEncoder().encode('Example text'));
  render(FileViewer, { client: createAPIClient(fetchFn), file: file({ mime_type }), position: { index: kind.startsWith('menu') ? 1 : 0, total: kind.startsWith('menu') ? 3 : 2 }, onStep });
  if (kind.startsWith('menu')) {
    await fireEvent.click(screen.getByRole('button', { name: 'More file actions' }));
    const item = await screen.findByRole('menuitem', { name: 'Open conversation' });
    item.focus();
    await fireEvent.keyDown(item, { key: kind.endsWith('ArrowLeft') ? 'ArrowLeft' : 'ArrowRight' });
    expect(screen.getByRole('menu', { name: 'File actions' })).toBeDefined();
  } else if (kind === 'audio') {
    await waitFor(() => expect(document.querySelector('audio')?.getAttribute('src')).toBe('blob:audio-preview'));
    await fireEvent.keyDown(document.querySelector('audio')!, { key: 'ArrowRight' });
  } else {
    await screen.findByText('Example text');
    const preview = screen.getByRole('region', { name: 'File preview preview.png' });
    Object.defineProperty(preview, 'scrollWidth', { configurable: true, value: 400 });
    Object.defineProperty(preview, 'clientWidth', { configurable: true, value: 200 });
    await fireEvent.keyDown(preview, { key: 'ArrowRight' });
    expect(onStep).not.toHaveBeenCalled();
    Object.defineProperty(preview, 'scrollWidth', { value: 200 });
    const input = document.createElement('input');
    preview.append(input);
    await fireEvent.keyDown(input, { key: 'ArrowRight' });
  }
  expect(onStep).not.toHaveBeenCalled();
});

it.each([0, 50])('renders text with %s bytes beyond the preview limit', async (extraBytes) => {
  const content = new Uint8Array(2 * 1024 * 1024 + extraBytes).fill(65);
  render(FileViewer, { client: createAPIClient(viewerFetch({ ...file(), mime_type: 'text/plain', size_bytes: content.length }, content)), file: file({ mime_type: 'text/plain', size_bytes: content.length }) });
  await waitFor(() => expect(document.querySelector('pre')?.textContent?.length).toBe(2 * 1024 * 1024));
  if (extraBytes) expect(await screen.findByText('Showing the start of this file.')).toBeDefined();
  else expect(screen.queryByText('Showing the start of this file.')).toBeNull();
  expect(screen.queryByRole('alert')).toBeNull();
});
it('uses the response charset for the rendered text', async () => {
  render(FileViewer, { client: createAPIClient(viewerFetch({ ...file(), filename: 'notes.txt', mime_type: 'text/plain; charset=windows-1252' }, new Uint8Array([0x43, 0x61, 0x66, 0xe9]), 'text/plain; charset=windows-1252')), file: file({ filename: 'notes.txt', mime_type: 'text/plain; charset=windows-1252' }) });
  expect(await screen.findByText('Café')).toBeDefined();
});
it('keeps Download disabled while availability is unknown', async () => {
  render(FileViewer, { client: createAPIClient(() => new Promise<Response>(() => undefined)), file: file() });
  expect(screen.getByRole('button', { name: 'Download preview.png' }).hasAttribute('disabled')).toBe(true);
  expect(screen.getByText('Loading file…')).toBeDefined();
});

it.each(['bytes only', 'rows and bytes'])('shows CSV limits for %s', async (limit) => {
  const rows = limit === 'rows and bytes' ? Array.from({ length: 205 }, (_, i) => `Row ${i},${i}\n`).join('') : 'Complete,99\n';
  const prefix = 'Name,Amount\n' + rows + 'Partial,';
  const content = new TextEncoder().encode(prefix + 'x'.repeat(2 * 1024 * 1024 - prefix.length - 3) + ',123456');
  const target = file({ filename: 'cut.csv', mime_type: 'text/csv', size_bytes: content.length });
  render(FileViewer, { client: createAPIClient(viewerFetch(target, content)), file: target });
  expect(await screen.findByText('Preview stops at the 2 MiB size limit.')).toBeDefined();
  expect(screen.getByRole('table').textContent).not.toContain('Partial');
  if (limit === 'rows and bytes') {
    expect(screen.getByText('Showing the first 200 rows.')).toBeDefined();
    expect(screen.getAllByRole('row')).toHaveLength(201);
    expect(screen.queryByText('Row 200')).toBeNull();
  } else {
    expect(screen.getByRole('table').textContent).toContain('Complete');
    expect(screen.queryByText('Showing the first 200 rows.')).toBeNull();
  }
});
it('warns when malformed CSV rows could not be read', async () => {
  render(FileViewer, { client: createAPIClient(viewerFetch({ ...file(), filename: 'malformed.csv', mime_type: 'text/csv' }, new TextEncoder().encode('Name,Amount\nExample,"42\nOther,73'))), file: file({ filename: 'malformed.csv', mime_type: 'text/csv' }) });
  expect(await screen.findByText("Some rows couldn't be read.")).toBeDefined();
});
it('detects semicolon columns in the CSV viewer', async () => {
  render(FileViewer, { client: createAPIClient(viewerFetch({ ...file(), filename: 'regional.csv', mime_type: 'text/csv' }, new TextEncoder().encode('Name;Amount\nExample;42\nOther;73'))), file: file({ filename: 'regional.csv', mime_type: 'text/csv' }) });
  expect(await screen.findByRole('columnheader', { name: 'Amount' })).toBeDefined();
  expect(screen.getByRole('cell', { name: '42' })).toBeDefined();
});

it('stops at the loaded end and shows the list notice', async () => {
  const onStep = vi.fn();
  render(FileViewer, { client: createAPIClient(viewerFetch(file())), file: file(), position: { index: 1, total: 4 }, onStep, canStepForward: false, notice: 'Could not load more files' });
  expect(screen.getByRole('button', { name: 'Next file' }).hasAttribute('disabled')).toBe(true);
  await fireEvent.keyDown(window, { key: 'ArrowRight' });
  expect(onStep).not.toHaveBeenCalled();
  expect(screen.getByRole('alert').textContent).toBe('Could not load more files');
  await fireEvent.keyDown(window, { key: 'ArrowLeft' });
  expect(onStep).toHaveBeenCalledWith(-1);
});
it.each(['audio/wav', 'application/pdf', 'image/png'])('offers Download when %s exceeds the preview bound', async (mime_type) => {
  render(FileViewer, { client: createAPIClient(viewerFetch({ ...file(), mime_type, size_bytes: 26 * 1024 * 1024 })), file: file({ mime_type, size_bytes: 26 * 1024 * 1024 }) });
  expect(await screen.findByText('This file is too large to preview.')).toBeDefined();
  expect(screen.getByRole('button', { name: /Download/ }).hasAttribute('disabled')).toBe(false);
});
