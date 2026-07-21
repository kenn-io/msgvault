<script lang="ts">
  import { Modal, appShortcuts } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { FileMetadata, FileViewerTarget } from '../../explore/models';
  import type { PDFRenderHandle } from './FileViewer.browser.svelte';
  import { isSupportedImageMIME, readBoundedStream, validatedImageBlob } from './preview-bytes';

  interface Props {
    client: APIClient;
    file: FileViewerTarget;
    returnFocus?: HTMLElement;
    onClose?: () => void;
    onOpenItem?: (entryKey: string) => void;
    onOpenConversation?: (entryKey: string, messageID: number, conversationID: number) => void;
  }

  let {
    client,
    file,
    returnFocus = undefined,
    onClose = undefined,
    onOpenItem = undefined,
    onOpenConversation = undefined
  }: Props = $props();

  const MAX_IMAGE_BYTES = 25 * 1024 * 1024;
  const MAX_PDF_BYTES = 25 * 1024 * 1024;
  let metadata = $state<FileMetadata>();
  let loading = $state(true);
  let error = $state('');
  let imageURL = $state('');
  let activeObjectURL: string | undefined;
  let pdfHost = $state<HTMLElement>();
  let controller: AbortController | undefined;
  let renderHandle: PDFRenderHandle | undefined;
  let generation = 0;
  let requestedFileID: number | undefined;
  let releaseShortcutScope: (() => void) | undefined;
  let closed = false;

  $effect(() => {
    const fileID = file.id;
    if (requestedFileID === fileID) return;
    requestedFileID = fileID;
    untrack(() => cleanupPreview());
    const currentGeneration = ++generation;
    controller = new AbortController();
    metadata = undefined;
    loading = true;
    error = '';
    void loadMetadata(currentGeneration, controller.signal);
  });

  $effect(() => {
    if (!metadata || metadata.content_state !== 'local_content' || !pdfHost || !isPDF(metadata)) return;
    const currentGeneration = generation;
    const signal = controller?.signal;
    if (!signal) return;
    void loadPDF(metadata, currentGeneration, signal);
  });

  onMount(() => {
    releaseShortcutScope = appShortcuts.pushScope('file-viewer');
    return releaseScope;
  });

  onDestroy(() => {
    cleanupPreview();
    releaseScope();
  });

  async function loadMetadata(currentGeneration: number, signal: AbortSignal): Promise<void> {
    const { data, error: responseError } = await client.GET('/api/v1/files/{id}', {
      params: { path: { id: file.id } },
      signal
    });
    if (signal.aborted || currentGeneration !== generation) return;
    if (!data) {
      error = responseError && typeof responseError === 'object' && 'message' in responseError
        ? String(responseError.message)
        : 'File metadata could not be loaded.';
      loading = false;
      return;
    }
    metadata = data;
    loading = false;
    if (data.content_state === 'local_content' && isImage(data)) {
      void loadImage(data, currentGeneration, signal);
    }
  }

  async function loadBytes(
    value: FileMetadata,
    signal: AbortSignal,
    maxBytes: number
  ): Promise<{ bytes: Uint8Array; contentType: string | null }> {
    if (value.size_bytes > maxBytes) throw new Error('File exceeds the preview byte limit.');
    const { data, response } = await client.GET('/api/v1/files/{id}/content', {
      params: { path: { id: value.id } },
      parseAs: 'stream',
      signal
    });
    if (!response.ok || !(data instanceof ReadableStream)) throw new Error('Archived content could not be loaded.');
    return {
      bytes: await readBoundedStream(data, response.headers, signal, maxBytes),
      contentType: response.headers.get('Content-Type')
    };
  }

  async function loadImage(value: FileMetadata, currentGeneration: number, signal: AbortSignal): Promise<void> {
    try {
      const loaded = await loadBytes(value, signal, MAX_IMAGE_BYTES);
      if (signal.aborted || currentGeneration !== generation) return;
      const blob = validatedImageBlob(loaded.bytes, value.mime_type, loaded.contentType);
      activeObjectURL = URL.createObjectURL(blob);
      imageURL = activeObjectURL;
    } catch (loadError) {
      if (!signal.aborted && currentGeneration === generation) error = errorMessage(loadError);
    }
  }

  async function loadPDF(value: FileMetadata, currentGeneration: number, signal: AbortSignal): Promise<void> {
    const host = pdfHost;
    if (!host || renderHandle) return;
    try {
      const { bytes } = await loadBytes(value, signal, MAX_PDF_BYTES);
      if (signal.aborted || currentGeneration !== generation || host !== pdfHost) return;
      if (bytes.length < 5 || new TextDecoder('ascii').decode(bytes.subarray(0, 5)) !== '%PDF-') {
        throw new Error('PDF preview was rejected because the file signature is invalid.');
      }
      const { renderPDF } = await import('./FileViewer.browser.svelte');
      if (signal.aborted || currentGeneration !== generation || host !== pdfHost) return;
      renderHandle = await renderPDF(bytes, host, signal);
    } catch (loadError) {
      if (!signal.aborted && currentGeneration === generation) error = errorMessage(loadError);
    }
  }

  async function download(): Promise<void> {
    if (!metadata || metadata.content_state !== 'local_content') return;
    const downloadController = new AbortController();
    try {
      const { bytes, contentType } = await loadBytes(metadata, downloadController.signal, Number.MAX_SAFE_INTEGER);
      const blob = new Blob([new Uint8Array(bytes).buffer], { type: contentType ?? metadata.mime_type });
      const url = URL.createObjectURL(blob);
      const link = document.createElement('a');
      link.href = url;
      link.download = displayFilename;
      link.click();
      queueMicrotask(() => URL.revokeObjectURL(url));
    } catch (downloadError) {
      error = errorMessage(downloadError);
    }
  }

  async function close(): Promise<void> {
    if (closed) return;
    closed = true;
    cleanupPreview();
    releaseScope();
    onClose?.();
    await tick();
    returnFocus?.focus();
  }

  function cleanupPreview(): void {
    generation += 1;
    controller?.abort();
    controller = undefined;
    renderHandle?.destroy();
    renderHandle = undefined;
    revokeImageURL();
  }

  function revokeImageURL(): void {
    if (activeObjectURL) URL.revokeObjectURL(activeObjectURL);
    activeObjectURL = undefined;
    imageURL = '';
  }

  function releaseScope(): void {
    releaseShortcutScope?.();
    releaseShortcutScope = undefined;
  }

  function imageFailed(): void {
    revokeImageURL();
    error = 'The browser could not decode this image preview.';
  }

  function isImage(value: FileMetadata): boolean {
    return isSupportedImageMIME(value.mime_type);
  }

  function isPDF(value: FileMetadata): boolean {
    return value.mime_type.toLowerCase() === 'application/pdf';
  }

  function errorMessage(value: unknown): string {
    return value instanceof Error ? value.message : 'File preview failed.';
  }

  function openItem(): void {
    const messageID = metadata?.message_id ?? file.message_id;
    const entryKey = file.entry_key ?? (messageID ? `message:${messageID}` : undefined);
    if (entryKey) onOpenItem?.(entryKey);
  }

  function openConversation(): void {
    const messageID = metadata?.message_id ?? file.message_id;
    const conversationID = metadata?.conversation_id ?? file.conversation_id;
    const entryKey = file.entry_key ?? (messageID ? `message:${messageID}` : undefined);
    if (entryKey && messageID && conversationID) onOpenConversation?.(entryKey, messageID, conversationID);
  }

  function stateMessage(value: FileMetadata): string | undefined {
    if (value.content_state === 'missing_blob') return 'Archived bytes are missing.';
    if (value.content_state === 'metadata_only') return 'This attachment has metadata only.';
    if (value.content_state === 'url_only') return 'This attachment is URL-only and is not fetched as local content.';
    if (!isImage(value) && !isPDF(value)) return 'Preview is not supported for this file type.';
    return undefined;
  }

  function namedFilename(value: string | undefined): string | undefined {
    return value?.trim() ? value : undefined;
  }

  const displayFilename = $derived(
    namedFilename(metadata?.filename) ?? namedFilename(file.filename) ?? `attachment ${file.id}`
  );
</script>

<Modal
  ariaLabel={`View ${displayFilename}`}
  closable={false}
  closeOnOverlayClick={false}
  width="min(960px, 94vw)"
  maxWidth="min(960px, 94vw)"
  onclose={() => { void close(); }}
>
  <div class="file-viewer">
    <div class="viewer-header">
      <div>
        <p class="eyebrow">File preview</p>
        <h2>{displayFilename}</h2>
      </div>
      <button type="button" aria-label="Close file viewer" onclick={() => { void close(); }}>×</button>
    </div>

    <div class="preview">
      {#if loading}
        <p role="status">Loading file metadata…</p>
      {:else if metadata}
        {#if stateMessage(metadata)}
          <p class="state-message">{stateMessage(metadata)}</p>
        {:else if isImage(metadata)}
          {#if imageURL}
            <img src={imageURL} alt={`Preview ${displayFilename}`} onerror={imageFailed} />
          {:else}
            <p role="status">Loading image preview…</p>
          {/if}
        {:else if isPDF(metadata)}
          <div class="pdf-preview" role="region" aria-label={`PDF preview ${displayFilename}`} bind:this={pdfHost}></div>
        {/if}
      {/if}
      {#if error}<p class="error" role="alert">{error}</p>{/if}
    </div>

    <footer>
      <div class="metadata">
        <span>{metadata?.mime_type || file.mime_type || 'Unknown type'}</span>
        <span>{formatBytes(metadata?.size_bytes ?? file.size_bytes ?? 0)}</span>
      </div>
      <div class="actions">
        <button type="button" disabled={!metadata && !file.entry_key} onclick={openItem}>Open containing item</button>
        <button type="button" disabled={!metadata && !file.entry_key} onclick={openConversation}>Open containing conversation</button>
        {#if metadata?.content_state === 'local_content'}
          <button type="button" aria-label={`Download ${displayFilename}`} onclick={() => { void download(); }}>Download</button>
        {/if}
      </div>
    </footer>
  </div>
</Modal>

<style>
  .file-viewer { display: flex; width: 100%; height: min(720px, calc(100vh - 96px)); flex-direction: column; overflow: hidden; background: var(--bg-surface); }
  .viewer-header, footer { display: flex; align-items: center; justify-content: space-between; gap: var(--space-4); padding: var(--space-4) var(--space-5); border-color: var(--border-default); }
  .viewer-header { border-bottom: 1px solid; }
  footer { border-top: 1px solid; }
  h2, p { margin: 0; }
  h2 { overflow: hidden; font-size: var(--font-size-lg); text-overflow: ellipsis; white-space: nowrap; }
  .eyebrow { color: var(--artifact-ink); font-size: var(--font-size-2xs); font-weight: 800; letter-spacing: .1em; text-transform: uppercase; }
  button { min-height: 32px; border: 1px solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-inset); color: var(--text-primary); cursor: pointer; }
  /* Sizes the "×" glyph, not text content — intentionally outside the --font-size-* type scale. */
  .viewer-header button { width: 34px; font-size: 22px; }
  .preview { min-height: 0; flex: 1; overflow: auto; padding: var(--space-5); background: var(--bg-inset); }
  .preview img { display: block; max-width: 100%; max-height: 100%; margin: auto; object-fit: contain; }
  .pdf-preview { display: grid; justify-items: center; gap: var(--space-5); }
  .pdf-preview :global(.pdf-page) { max-width: 100%; padding: var(--space-3); background: var(--bg-surface); box-shadow: var(--shadow-md); color: var(--text-primary); }
  .pdf-preview :global(.pdf-canvas) { display: block; max-width: 100%; height: auto; }
  .pdf-preview :global(.pdf-text) { max-width: 70ch; padding-top: var(--space-3); font-size: var(--font-size-xs); line-height: 1.5; }
  .state-message, .error { padding: var(--space-5); border: 1px solid var(--border-muted); border-radius: var(--radius-md); color: var(--text-secondary); }
  .error { margin-top: var(--space-3); color: var(--accent-red); }
  .metadata, .actions { display: flex; align-items: center; gap: var(--space-3); }
  .metadata { color: var(--text-muted); font-size: var(--font-size-xs); }
  .actions { justify-content: flex-end; }
  .actions button { padding: 0 var(--space-3); }
</style>

<script lang="ts" module>
  function formatBytes(value: number): string {
    if (value < 1024) return `${value} B`;
    if (value < 1024 * 1024) return `${Math.round(value / 1024)} KB`;
    return `${(value / (1024 * 1024)).toFixed(1)} MB`;
  }
</script>
