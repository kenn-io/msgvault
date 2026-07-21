<script lang="ts">
  import type { APIClient } from '../../api/client';
  import type { ArchiveMessageDetail, MessageViewMode } from '../../archive/types';
  import ContentFrame from './ContentFrame.svelte';

  interface Props {
    message: ArchiveMessageDetail;
    expanded: boolean;
    /** The thread's current anchor — announced via aria-current and used by
     * the thread to scroll the card into view. */
    anchor?: boolean;
    viewMode?: MessageViewMode;
    sanitizationFailed?: boolean;
    onToggle?: (id: number) => void;
    onViewModeChange?: (id: number, mode: MessageViewMode) => void;
    client?: APIClient;
  }

  let {
    message,
    expanded,
    anchor = false,
    viewMode = 'html',
    sanitizationFailed = false,
    onToggle,
    onViewModeChange,
    client = undefined
  }: Props = $props();

  let menuOpen = $state(false);

  const hasRenderableHTML = $derived(
    typeof message.bodyHtml === 'string' && message.bodyHtml.trim() !== '' && !sanitizationFailed
  );
  const renderAsHTML = $derived(hasRenderableHTML && viewMode === 'html');

  function formatDate(value: string): string {
    const parsed = new Date(value);
    if (Number.isNaN(parsed.valueOf())) return value;
    return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(parsed);
  }

  function selectMode(mode: MessageViewMode): void {
    menuOpen = false;
    onViewModeChange?.(message.id, mode);
  }
</script>

{#if expanded}
  <article
    class="message-card message-card--expanded"
    aria-label={`Message ${message.id}`}
    aria-current={anchor ? 'true' : undefined}
    data-message-id={message.id}
  >
    <div class="card-header">
      <button
        type="button"
        class="collapse-target"
        aria-label={`Collapse message ${message.id} from ${message.sender || 'unknown sender'}`}
        aria-expanded="true"
        onclick={() => onToggle?.(message.id)}
      >
        <span class="header-line">
          <strong class="sender">{message.sender || 'Unknown sender'}</strong>
          <time datetime={message.sentAt}>{formatDate(message.sentAt)}</time>
        </span>
        {#if message.recipients.length > 0}
          <span class="recipients">to {message.recipients.join(', ')}</span>
        {/if}
        {#if message.subject}
          <span class="subject">{message.subject}</span>
        {/if}
      </button>
      {#if hasRenderableHTML}
        <details class="card-menu" bind:open={menuOpen}>
          <summary aria-label={`Message ${message.id} display options`}>⋯</summary>
          <div class="menu-sheet kit-popover-card">
            {#if viewMode === 'html'}
              <button type="button" onclick={() => selectMode('text')}>Show plain text</button>
            {:else}
              <button type="button" onclick={() => selectMode('html')}>Show formatted HTML</button>
            {/if}
          </div>
        </details>
      {/if}
    </div>

    {#if sanitizationFailed}
      <p class="sanitize-notice" role="alert">Could not render HTML formatting. Showing plain text.</p>
    {/if}

    <section class="card-body" aria-label="Message body">
      {#if renderAsHTML}
        <ContentFrame
          {client}
          messageId={message.id}
          html={message.bodyHtml ?? ''}
          title="Message body"
        />
      {:else}
        <pre>{message.body}</pre>
      {/if}
    </section>
  </article>
{:else}
  <button
    type="button"
    class="message-card message-card--collapsed"
    aria-label={`Expand message ${message.id} from ${message.sender || 'unknown sender'}`}
    aria-expanded="false"
    data-message-id={message.id}
    onclick={() => onToggle?.(message.id)}
  >
    <span class="collapsed-sender">{message.sender || 'Unknown sender'}</span>
    <span class="collapsed-snippet">{message.snippet || message.subject}</span>
    <time datetime={message.sentAt}>{formatDate(message.sentAt)}</time>
  </button>
{/if}

<style>
  .message-card {
    min-width: 0;
    border-bottom: 1px solid var(--border-muted);
  }

  .message-card--collapsed {
    display: flex;
    width: 100%;
    align-items: baseline;
    gap: var(--space-4);
    padding: var(--space-3) var(--space-4);
    border: 0;
    border-bottom: 1px solid var(--border-muted);
    background: none;
    color: var(--text-secondary);
    cursor: pointer;
    font: inherit;
    text-align: left;
  }

  .message-card--collapsed:hover {
    background: var(--bg-surface-hover);
  }

  .collapsed-sender {
    flex: none;
    max-width: 220px;
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    font-weight: 600;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .collapsed-snippet {
    min-width: 0;
    flex: 1;
    overflow: hidden;
    font-size: var(--font-size-sm);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .message-card time {
    flex: none;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }

  .card-header {
    display: flex;
    align-items: flex-start;
    gap: var(--space-3);
    padding: var(--space-3) var(--space-4) 0;
  }

  .collapse-target {
    display: grid;
    min-width: 0;
    flex: 1;
    gap: 2px;
    padding: 0;
    border: 0;
    background: none;
    cursor: pointer;
    font: inherit;
    text-align: left;
  }

  .header-line {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-4);
  }

  .sender {
    min-width: 0;
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .recipients,
  .subject {
    overflow: hidden;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .subject {
    color: var(--text-secondary);
  }

  .card-menu {
    position: relative;
    flex: none;
  }

  .card-menu summary {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 24px;
    height: 24px;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    cursor: pointer;
    list-style: none;
  }

  .card-menu summary::-webkit-details-marker {
    display: none;
  }

  .card-menu summary:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .menu-sheet {
    position: absolute;
    z-index: var(--z-popover);
    top: 26px;
    right: 0;
    min-width: 160px;
    padding: var(--space-2);
  }

  .menu-sheet button {
    display: block;
    width: 100%;
    padding: var(--space-2) var(--space-3);
    border: 0;
    border-radius: var(--radius-sm);
    background: none;
    color: var(--text-primary);
    cursor: pointer;
    font: inherit;
    font-size: var(--font-size-xs);
    text-align: left;
    white-space: nowrap;
  }

  .menu-sheet button:hover {
    background: var(--bg-surface-hover);
  }

  .sanitize-notice {
    margin: var(--space-2) var(--space-4) 0;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }

  .card-body {
    padding: var(--space-3) var(--space-4) var(--space-4);
  }

  /* Plain text renders on the theme surface with theme text. */
  pre {
    margin: 0;
    overflow-wrap: anywhere;
    color: var(--text-primary);
    font-family: var(--font-mono);
    font-size: var(--font-size-sm);
    line-height: 1.55;
    white-space: pre-wrap;
  }
</style>
