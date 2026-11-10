<script lang="ts">
  import { getFile as generatedGetFile, getFileContent as generatedGetFileContent } from '../../api/generated/api/api';
  import { Button, EmptyState, IconButton, Menu, MenuContent, MenuItem, MenuTrigger, Modal, Table, TableHeaderCell, appShortcuts } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { FileMetadata, FileViewerTarget } from '../../explore/models';
  import type { PDFRenderHandle } from './FileViewer.browser.svelte';
  import { isSupportedImageMIME, readBoundedStream, readStreamPrefix, validatedImageBlob } from './preview-bytes';
  import Ellipsis from '@lucide/svelte/icons/ellipsis';
  import Minus from '@lucide/svelte/icons/minus';
  import Plus from '@lucide/svelte/icons/plus';
  import ChevronLeft from '@lucide/svelte/icons/chevron-left';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import FileIcon from '@lucide/svelte/icons/file';
  import FileX from '@lucide/svelte/icons/file-x';
  import FileVolume from '@lucide/svelte/icons/file-volume';
  import { decodeText, parseDelimited } from './text-preview';
  import { fileTypeLabel } from '../../explore/labels';
  import { formatShortDate, formatBytes } from '../../util/format';
  interface Props {
    client: APIClient;
    file: FileViewerTarget;
    returnFocus?: HTMLElement;
    position?: { index: number; total: number };
    onStep?: (delta: -1 | 1) => void;
    notice?: string;
    canStepForward?: boolean;
    onClose?: () => void;
    onOpenItem?: (entryKey: string) => void;
    onOpenConversation?: (entryKey: string, messageID: number, conversationID: number) => void;
  }
  let {
    client,
    file,
    returnFocus = undefined,
    position = undefined,
    onStep = undefined,
    notice = '',
    canStepForward = true,
    onClose = undefined,
    onOpenItem = undefined,
    onOpenConversation = undefined,
  }: Props = $props();
  const MAX_IMAGE_BYTES = 25 * 1024 * 1024;
  const MAX_PDF_BYTES = 25 * 1024 * 1024;
  const MAX_AUDIO_BYTES = 25 * 1024 * 1024;
  const MAX_TEXT_BYTES = 2 * 1024 * 1024;
  let text = $state<string>();
  let delimited = $state<Awaited<ReturnType<typeof parseDelimited>>>();
  let textTruncated = $state(false);
  let imageSize = $state('');
  let pdfPages = $state(0);
  let pdfPage = $state(1);
  let pdfZoom = $state(1);
  const zoomSteps = [0.75, 1, 1.5, 2];
  function zoomPDF(value: number): void {
    pdfZoom = value;
    renderHandle?.setZoom(value);
  }
  function updatePDFPage(event: Event): void {
    const preview = event.currentTarget as HTMLElement;
    const top = preview.getBoundingClientRect().top + preview.clientHeight / 3;
    const pages = [...preview.querySelectorAll<HTMLElement>('.pdf-page')];
    const index = pages.findIndex((page) => page.getBoundingClientRect().bottom > top);
    if (pages.length) pdfPage = index < 0 ? pages.length : index + 1;
  }
  let metadata = $state<FileMetadata>();
  let loading = $state(true);
  let error = $state('');
  let imageURL = $state('');
  let audioURL = $state('');
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
    let metadataResponse;
    try {
      metadataResponse = await generatedGetFile(
        { id: file.id },
        {
          ...client,
          signal,
        },
      );
    } catch (cause: unknown) {
      if (!signal.aborted && currentGeneration === generation) {
        error = cause instanceof Error ? cause.message : 'File metadata could not be loaded.';
        loading = false;
      }
      return;
    }
    const { data, error: responseError } = metadataResponse;
    if (signal.aborted || currentGeneration !== generation) return;
    if (!data) {
      error =
        responseError && typeof responseError === 'object' && 'message' in responseError
          ? String(responseError.message)
          : 'File metadata could not be loaded.';
      loading = false;
      return;
    }
    metadata = data;
    loading = false;
    if (data.content_state === 'local_content' && isImage(data)) {
      void loadImage(data, currentGeneration, signal);
    } else if (data.content_state === 'local_content' && previewKind(data) === 'audio') {
      void loadAudio(data, currentGeneration, signal);
    } else if (data.content_state === 'local_content' && ['csv', 'tsv', 'text'].includes(previewKind(data))) {
      void loadText(data, currentGeneration, signal);
    }
  }
  async function loadBytes(
    value: FileMetadata,
    signal: AbortSignal,
    maxBytes: number,
    truncate = false,
  ): Promise<{
    bytes: Uint8Array;
    contentType: string | null;
    truncated: boolean;
  }> {
    if (!truncate && value.size_bytes > maxBytes) throw new Error('This file is too large to preview.');
    const { data, response } = await generatedGetFileContent(
      { id: value.id },
      {
        ...client,
        signal,
      },
    );
    if (!response.ok || !(data instanceof ReadableStream)) throw new Error('Archived content could not be loaded.');
    return {
      ...(truncate ? await readStreamPrefix(data, signal, maxBytes) : { bytes: await readBoundedStream(data, response.headers, signal, maxBytes), truncated: false }),
      contentType: response.headers.get('Content-Type'),
    };
  }
  async function loadText(value: FileMetadata, currentGeneration: number, signal: AbortSignal): Promise<void> {
    try {
      const { bytes, contentType, truncated } = await loadBytes(value, signal, MAX_TEXT_BYTES, true);
      const decoded = decodeText(bytes, contentType ?? value.mime_type, truncated);
      const kind = previewKind(value);
      const parsed = kind === 'text' ? undefined : await parseDelimited(decoded, kind === 'tsv' ? '\t' : '', 200, truncated);
      if (signal.aborted || currentGeneration !== generation) return;
      text = decoded;
      delimited = parsed;
      textTruncated = truncated;
    } catch (loadError) {
      if (!signal.aborted && currentGeneration === generation) error = errorMessage(loadError);
    }
  }
  async function loadAudio(value: FileMetadata, currentGeneration: number, signal: AbortSignal): Promise<void> {
    try {
      const { bytes } = await loadBytes(value, signal, MAX_AUDIO_BYTES);
      if (signal.aborted || currentGeneration !== generation) return;
      activeObjectURL = URL.createObjectURL(new Blob([new Uint8Array(bytes).buffer], { type: value.mime_type.split(';', 1)[0] }));
      audioURL = activeObjectURL;
    } catch (loadError) {
      if (!signal.aborted && currentGeneration === generation) error = errorMessage(loadError);
    }
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
      const handle = await renderPDF(bytes, host, signal);
      if (signal.aborted || currentGeneration !== generation) { handle.destroy(); return; }
      renderHandle = handle;
      pdfPages = handle.pages;
    } catch (loadError) {
      if (!signal.aborted && currentGeneration === generation) error = errorMessage(loadError);
    }
  }
  // Navigates an anchor straight at the same-origin, session-authenticated
  // content endpoint so the browser streams the file to disk itself — no JS
  // buffering, so a multi-gigabyte attachment cannot exhaust tab memory. The
  // server's Content-Disposition: attachment header carries the authoritative
  // filename; the download attribute only names the fallback when the archive
  // recorded no filename.
  function download(): void {
    if (!metadata || metadata.content_state !== 'local_content') return;
    const link = document.createElement('a');
    link.href = `/api/v1/files/${metadata.id}/content`;
    link.download = displayFilename;
    link.rel = 'noopener';
    link.click();
  }
  async function close(): Promise<void> {
    if (closed) return;
    closed = true;
    cleanupPreview();
    releaseScope();
    const focusTarget = returnFocus;
    onClose?.();
    await tick();
    focusTarget?.focus();
  }
  function cleanupPreview(): void {
    generation += 1;
    controller?.abort();
    controller = undefined;
    renderHandle?.destroy();
    renderHandle = undefined;
    revokePreviewURL();
    text = undefined;
    delimited = undefined;
    textTruncated = false;
    imageSize = '';
    pdfPages = 0;
    pdfPage = 1;
    pdfZoom = 1;
  }
  function revokePreviewURL(): void {
    if (activeObjectURL) URL.revokeObjectURL(activeObjectURL);
    activeObjectURL = undefined;
    imageURL = '';
    audioURL = '';
  }
  function releaseScope(): void {
    releaseShortcutScope?.();
    releaseShortcutScope = undefined;
  }
  function imageFailed(): void {
    revokePreviewURL();
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
  // The metadata endpoint returns the canonical explore entry key for the
  // containing item; never synthesize one locally — a synthesized key cannot
  // match any listed explore entry.
  function openItem(): void {
    const entryKey = metadata?.entry_key ?? file.entry_key;
    if (entryKey) onOpenItem?.(entryKey);
  }
  function openConversation(): void {
    const messageID = metadata?.message_id ?? file.message_id;
    const conversationID = metadata?.conversation_id ?? file.conversation_id;
    const entryKey = metadata?.entry_key ?? file.entry_key;
    if (entryKey && messageID && conversationID) onOpenConversation?.(entryKey, messageID, conversationID);
  }
  function previewKind(value: FileMetadata): string {
    const mime = value.mime_type.split(';', 1)[0]!.trim().toLowerCase();
    if (isImage(value)) return 'image';
    if (isPDF(value)) return 'pdf';
    if (mime === 'text/csv' || mime === 'application/csv') return 'csv';
    if (mime === 'text/tab-separated-values') return 'tsv';
    if (mime.startsWith('audio/')) return 'audio';
    if (mime.startsWith('video/')) return 'none';
    const extension = /\.(csv|tsv|txt)$/i.exec(value.filename)?.[1]?.toLowerCase();
    if (extension === 'csv' || extension === 'tsv') return extension;
    if (extension === 'txt') return 'text';
    if (mime.startsWith('text/')) return 'text';
    return 'none';
  }
  function unavailableDescription(value: FileMetadata): string {
    if (value.content_state === 'missing_blob') return "The archive has this file's record, but its bytes are missing.";
    if (value.content_state === 'url_only') return "This file is a link, and msgvault didn't download it.";
    return "The import saved this file's name and size, not its contents.";
  }
  function handleKeydown(event: KeyboardEvent): void {
    if (!onStep || !position || appShortcuts.activeScope() !== 'file-viewer' || event.defaultPrevented || event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
    if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return;
    const target = event.target;
    if (target instanceof HTMLElement) {
      if (target.closest('audio, input, textarea, select, [role="menu"], [contenteditable]:not([contenteditable="false"])')) return;
      const preview = target.closest<HTMLElement>('.preview');
      if (preview?.querySelector('pre, table') && [...preview.querySelectorAll<HTMLElement>('.kit-table-wrapper'), preview].some((element) => element.scrollWidth > element.clientWidth)) return;
    }
    const delta = event.key === 'ArrowLeft' ? -1 : 1;
    if (delta === 1 && !canStepForward || position.index + delta < 0 || position.index + delta >= position.total) return;
    event.preventDefault();
    onStep(delta);
  }
  const context = $derived([file.occurred_at ? formatShortDate(file.occurred_at) : '', file.containing_title, file.participant_labels?.join(', ')].filter(Boolean).join(' · '));
  const typeLabel = $derived(fileTypeLabel(metadata?.mime_type ?? file.mime_type ?? '', file.mime_family ?? 'other'));
  const footerMeta = $derived([typeLabel, formatBytes(metadata?.size_bytes ?? file.size_bytes ?? 0), imageSize].filter(Boolean).join(' · '));
  function namedFilename(value: string | undefined): string | undefined {
    return value?.trim() ? value : undefined;
  }
  const displayFilename = $derived(
    namedFilename(metadata?.filename) ?? namedFilename(file.filename) ?? `attachment ${file.id}`,
  );
</script>

<svelte:window onkeydown={handleKeydown} />

<Modal
  title={displayFilename}
  ariaLabel={`View ${displayFilename}`}
  closeOnOverlayClick={false}
  width="min(1280px, calc(100vw - 32px))"
  maxWidth="min(1280px, calc(100vw - 32px))"
  closeLabel="Close file viewer"
  closable
  onclose={() => {
    void close();
  }}
>
  <div class="file-viewer">
    {#if context || position || notice}
      <div class="context-group">
        <div class="context-bar">
          <span class="context" title={context}>{context}</span>
          {#if position}
            <div class="navigation">
              <IconButton size="sm" ariaLabel="Previous file" disabled={position.index === 0} onclick={() => onStep?.(-1)}><ChevronLeft size={14} /></IconButton>
              <span class="data-mono">{(position.index + 1).toLocaleString()} of {position.total.toLocaleString()}</span>
              <IconButton size="sm" ariaLabel="Next file" disabled={position.index >= position.total - 1 || !canStepForward} onclick={() => onStep?.(1)}><ChevronRight size={14} /></IconButton>
            </div>
          {/if}
        </div>
        {#if notice}<p class="notice" role="alert">{notice}</p>{/if}
      </div>
    {/if}
    {#if pdfPages}
      <div class="pdf-controls" role="group" aria-label="PDF reading controls">
        <span class="data-mono">Page {pdfPage} of {pdfPages}</span>
        <IconButton size="sm" ariaLabel="Zoom out" disabled={pdfZoom === zoomSteps[0]} onclick={() => zoomPDF(zoomSteps[zoomSteps.indexOf(pdfZoom) - 1]!)}><Minus size={14} /></IconButton>
        <Button size="sm" surface={pdfZoom === 1 ? 'soft' : 'outline'} label="Fit width" onclick={() => zoomPDF(1)} />
        <IconButton size="sm" ariaLabel="Zoom in" disabled={pdfZoom === zoomSteps.at(-1)} onclick={() => zoomPDF(zoomSteps[zoomSteps.indexOf(pdfZoom) + 1]!)}><Plus size={14} /></IconButton>
      </div>
    {/if}
    <!-- svelte-ignore a11y_no_noninteractive_tabindex (Scrollable preview regions need keyboard access.) -->
    <div class="preview" onscroll={updatePDFPage} class:tabular={!!delimited} class:pdf={metadata && isPDF(metadata)}
      class:centered={metadata && (error || metadata.content_state !== 'local_content' || ['image', 'audio', 'none'].includes(previewKind(metadata)))} role="region" aria-label={`File preview ${displayFilename}`} tabindex="0">
      {#if loading}
        <p role="status">Loading file…</p>
      {:else if metadata}
        {#if metadata.content_state !== 'local_content'}
          <EmptyState title="This file's contents aren't in the archive" description={unavailableDescription(metadata)}>
            {#snippet icon()}<FileX size={48} />{/snippet}
          </EmptyState>
        {:else if error || previewKind(metadata) === 'none'}
          <div role={error ? 'alert' : undefined}>
            <EmptyState title={error || 'No preview for this file type'} description="Download it to open in another app.">
              {#snippet icon()}<FileIcon size={48} />{/snippet}
            </EmptyState>
          </div>
        {:else if isImage(metadata)}
          {#if imageURL}
            <img src={imageURL} alt={`Preview ${displayFilename}`} onerror={imageFailed} onload={(event) => { imageSize = `${(event.currentTarget as HTMLImageElement).naturalWidth} × ${(event.currentTarget as HTMLImageElement).naturalHeight}`; }} />
          {:else}<p role="status">Loading preview…</p>{/if}
        {:else if isPDF(metadata)}
          {#if !pdfPages}<p role="status">Loading preview…</p>{/if}
          <div class="pdf-preview" role="region" aria-label={`PDF preview ${displayFilename}`} bind:this={pdfHost}></div>
        {:else if previewKind(metadata) === 'audio'}
          {#if audioURL}
            <div class="audio-preview">
              <FileVolume size={48} />
              <audio controls preload="metadata" src={audioURL} onerror={() => { error = "This browser can't play this audio."; }}></audio>
            </div>
          {:else}<p role="status">Loading preview…</p>{/if}
        {:else if text === undefined}
          <p role="status">Loading preview…</p>
        {:else if delimited}
          <Table unsorted stickyHeader zebra ariaLabel="File contents">
            {#snippet header()}{#each delimited?.header ?? [] as cell}<TableHeaderCell><span title={cell}>{cell}</span></TableHeaderCell>{/each}{/snippet}
            {#each delimited.rows as row}<tr>{#each row as cell}<td title={cell}>{cell}</td>{/each}</tr>{/each}
          </Table>
          {#if textTruncated || delimited.truncated || delimited.columnsTruncated || delimited.hasErrors}
            <div class="preview-notes">
              {#if textTruncated}<p>Preview stops at the 2 MiB size limit.</p>
              {/if}
              {#if delimited.truncated}<p>Showing the first 200 rows.</p>{/if}
              {#if delimited.columnsTruncated}<p>Preview stops at 100 columns.</p>{/if}
              {#if delimited.hasErrors}<p role="status">Some rows couldn't be read.</p>{/if}
            </div>
          {/if}
        {:else}
          <pre>{text}</pre>
          {#if textTruncated}<p class="truncated">Showing the start of this file.</p>{/if}
        {/if}
      {:else if error}<p class="error" role="alert">{error}</p>{/if}
    </div>
  </div>
  {#snippet footer()}
    <div class="viewer-footer">
      <div class="metadata" title={metadata?.mime_type ?? file.mime_type}>{footerMeta}</div>
      <div class="actions">
        <Menu align="end">
          <MenuTrigger ariaLabel="More file actions" title="More actions">
            {#snippet child({ attachment })}
              <IconButton size="sm" ariaLabel="More file actions" title="More actions" {@attach attachment}><Ellipsis size={16} /></IconButton>
            {/snippet}
          </MenuTrigger>
          <MenuContent ariaLabel="File actions">
            <MenuItem disabled={!metadata && !file.entry_key} onselect={openConversation}>Open conversation</MenuItem>
          </MenuContent>
        </Menu>
        <Button size="sm" tone={!metadata || metadata.content_state === 'local_content' ? 'neutral' : 'info'} surface={!metadata || metadata.content_state === 'local_content' ? 'outline' : 'solid'} label="Open message" disabled={!metadata && !file.entry_key} onclick={openItem} />
        <Button
          size="sm"
          tone="info"
          surface={!metadata || metadata.content_state !== 'local_content' ? 'outline' : 'solid'}
          label="Download"
          disabled={!metadata || metadata.content_state !== 'local_content'}
          ariaLabel={`Download ${displayFilename}`}
          onclick={download}
        />
      </div>
    </div>
  {/snippet}
</Modal>

<style>
  .file-viewer {
    display: flex;
    width: 100%;
    height: 100%;
    min-height: 0;
    flex-direction: column;
    overflow: hidden;
    background: var(--bg-surface);
  }
  p {
    margin: 0;
  }
  .preview {
    min-height: 0;
    flex: 1;
    overflow: auto;
    /* Reserve the scrollbar's width up front so centered PDF pages do not
       shift left when a later page makes the preview scroll. */
    scrollbar-gutter: stable both-edges;
    padding: var(--pane-pad-y) var(--pane-pad-x);
    background: var(--bg-inset);
  }
  .preview > p[role='status']:not(.truncated) {
    display: grid;
    height: 100%;
    place-items: center;
  }
  .preview img {
    display: block;
    max-width: 100%;
    max-height: 100%;
    min-height: 0;
    margin: auto;
    object-fit: contain;
  }
  .preview.pdf {
    padding: 0;
  }
  .preview.pdf:has(:global(.pdf-page)) > p[role='status'] {
    display: none;
  }
  .pdf-preview :global(.pdf-pages) {
    display: grid;
    width: max-content;
    min-width: 100%;
    box-sizing: border-box;
    padding: var(--pane-pad-y) var(--pane-pad-x);
    gap: var(--space-5);
  }
  .pdf-preview :global(.textLayer) {
    color: transparent;
  }
  .pdf-preview :global(.textLayer ::selection) {
    color: transparent;
    background: color-mix(in srgb, var(--accent-blue) 25%, transparent);
  }
  .pdf-preview :global(.pdf-page) {
    position: relative;
    border: var(--space-3) solid var(--bg-surface);
    box-sizing: content-box;
    justify-self: center;
    background: white;
    box-shadow: var(--shadow-md);
    color: var(--text-primary);
  }
  .pdf-preview :global(.pdf-canvas) {
    display: block;
    width: 100%;
    height: 100%;
  }
  .error {
    padding: var(--pane-pad-y) var(--pane-pad-x);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    margin-top: var(--space-3);
    color: var(--accent-red);
  }
  .viewer-footer,
  .metadata,
  .actions {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }
  .viewer-footer {
    width: 100%;
    flex-wrap: wrap;
    justify-content: space-between;
  }
  .metadata {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .actions {
    flex-wrap: wrap;
    justify-content: flex-end;
  }
  :global(.kit-modal-panel:has(.file-viewer)) {
    height: calc(100svh - 64px);
    outline: none;
  }
  :global(:root:root .kit-modal-body:has(.file-viewer)) {
    display: flex;
    flex: 1;
    min-height: 0;
    padding-top: var(--space-3);
    overflow: hidden;
  }
  .context-group {
    margin-bottom: var(--space-3);
  }
  .context-bar {
    display: flex;
    flex-wrap: nowrap;
    align-items: center;
    gap: var(--space-3);
    min-height: 28px;
  }
  .context {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }
  .navigation {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    margin-left: auto;
    margin-right: calc(-1 * var(--space-3));
    flex-shrink: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .tabular {
    display: flex;
    flex-direction: column;
  }
  .tabular :global(.kit-table-wrapper) {
    flex: 0 1 auto;
  }
  .preview-notes {
    position: sticky;
    bottom: 0;
    flex-shrink: 0;
    margin-top: var(--space-3);
    border-top: 1px solid var(--border-muted);
    padding-top: var(--space-2);
    background: var(--bg-inset);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .centered {
    display: grid;
    grid-template-rows: minmax(0, 1fr);
    place-items: center;
  }
  pre {
    margin: 0;
    padding: var(--space-3);
    white-space: pre-wrap;
    font-family: var(--font-mono);
    font-size: var(--font-size-sm);
    line-height: 1.5;
    color: var(--text-primary);
  }
  td, .tabular :global(.kit-th) {
    max-width: 32ch;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .tabular :global(tbody tr:nth-child(odd)) {
    background: var(--bg-surface);
  }
  .notice {
    margin-top: var(--space-1);
    color: var(--accent-red);
    font-size: var(--font-size-sm);
  }
  .pdf-controls {
    display: flex;
    align-items: center;
    justify-content: center;
    gap: var(--space-3);
    padding-bottom: var(--space-3);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }
  @media (max-width: 640px) {
    .viewer-footer {
      display: grid;
      grid-template-columns: minmax(0, 1fr);
      grid-template-rows: auto auto;
      min-height: 48px;
      align-content: start;
    }
    .actions {
      justify-self: end;
      align-self: end;
    }
  }
  .truncated {
    margin-top: var(--space-3);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .audio-preview {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-4);
    width: 100%;
    color: var(--text-muted);
  }
  audio {
    width: min(480px, 100%);
  }
</style>
