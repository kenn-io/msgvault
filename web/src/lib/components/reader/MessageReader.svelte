<script lang="ts">
  import type { APIClient } from '../../api/client';
  import type { ArchiveMessageDetail, MessageViewMode } from '../../archive/types';
  import ContentFrame from './ContentFrame.svelte';

  interface Props {
    message: ArchiveMessageDetail | null;
    loading?: boolean;
    error?: string | null;
    viewMode?: MessageViewMode;
    sanitizationFailed?: boolean;
    onViewModeChange?: (id: number, mode: MessageViewMode) => void;
    onContentFocusChange?: (focused: boolean) => void;
    client?: APIClient;
  }

  let {
    message,
    loading = false,
    error = null,
    viewMode = 'html',
    sanitizationFailed = false,
    onViewModeChange,
    onContentFocusChange,
    client = undefined
  }: Props = $props();

  const hasRenderableHTML = $derived(
    message !== null &&
      typeof message.bodyHtml === 'string' &&
      message.bodyHtml.trim() !== '' &&
      !sanitizationFailed
  );
  const renderAsHTML = $derived(hasRenderableHTML && viewMode === 'html');

  function selectMode(mode: MessageViewMode): void {
    if (message === null) return;
    onViewModeChange?.(message.id, mode);
  }
</script>

<article class="message-reader" aria-label="Message reader">
  {#if loading}
    <p aria-label="Loading message">Loading message…</p>
  {:else if error}
    <p role="alert">{error}</p>
  {:else if message === null}
    <p>Select a message to read it.</p>
  {:else}
    <header>
      <h1>{message.subject}</h1>
      <p>{message.sender}</p>
      {#if hasRenderableHTML}
        <div class="view-mode" role="group" aria-label="Message view">
          <button
            type="button"
            aria-pressed={viewMode === 'html'}
            onclick={() => selectMode('html')}>HTML</button
          >
          <button
            type="button"
            aria-pressed={viewMode === 'text'}
            onclick={() => selectMode('text')}>Text</button
          >
        </div>
      {/if}
    </header>

    {#if sanitizationFailed}
      <p role="alert">Could not render HTML formatting. Showing plain text.</p>
    {/if}

    <section aria-label="Message body">
      {#if renderAsHTML}
        <ContentFrame
          {client}
          messageId={message.id}
          html={message.bodyHtml ?? ''}
          title="Message body"
          {onContentFocusChange}
        />
      {:else}
        <pre>{message.body}</pre>
      {/if}
    </section>
  {/if}
</article>

<style>
  .message-reader {
    min-width: 0;
  }

  header {
    position: relative;
  }

  h1 {
    margin: 0;
    font-size: var(--font-size-lg);
  }

  .view-mode {
    display: flex;
    gap: 4px;
  }

  pre {
    box-sizing: border-box;
    width: 100%;
    min-height: 240px;
  }

  pre {
    margin: 0;
    white-space: pre-wrap;
  }
</style>
