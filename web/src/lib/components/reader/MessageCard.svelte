<script lang="ts">
  import MessageMenu from './MessageMenu.svelte';
  import PersonLink from './PersonLink.svelte';
  import MessageTasks from '../tasks/MessageTasks.svelte';
  import MeetingActions from '../meetings/MeetingActions.svelte';
  import type { MeetingRef } from '../../api/generated/models';

  import type { APIClient } from '../../api/client';
  import type { ArchiveMessageDetail, MessageViewMode } from '../../archive/types';
  import IdentityAvatar from '../common/IdentityAvatar.svelte';
  import ContentFrame from './ContentFrame.svelte';
  import MessageRecordings from './MessageRecordings.svelte';

  interface Props {
    message: ArchiveMessageDetail;
    expanded: boolean;
    /** The thread's current anchor — announced via aria-current and used by
     * the thread to scroll the card into view. */
    anchor?: boolean;
    viewMode?: MessageViewMode;
    sanitizationFailed?: boolean;
    /** The body was omitted from the window response and is being fetched. */
    bodyPending?: boolean;
    /** The on-demand body fetch failed; shown in place of the body. */
    bodyError?: string;
    onToggle?: (id: number) => void;
    onViewModeChange?: (id: number, mode: MessageViewMode) => void;
    client?: APIClient;
    onOpenPerson?: (address: string) => void;
    onOpenSettings?: () => void;
    onOpenMeeting?: (meeting: MeetingRef) => void;
  }

  let {
    message,
    expanded,
    anchor = false,
    viewMode = 'html',
    sanitizationFailed = false,
    bodyPending = false,
    bodyError = '',
    onToggle,
    onViewModeChange,
    onOpenPerson = undefined,
    onOpenSettings = undefined,
    onOpenMeeting = undefined,
    client = undefined
  }: Props = $props();

  let tasksRefresh = $state(0);
  let menuError = $state('');

  const hasRenderableHTML = $derived(
    typeof message.bodyHtml === 'string' && message.bodyHtml.trim() !== '' && !sanitizationFailed
  );
  const renderAsHTML = $derived(hasRenderableHTML && viewMode === 'html');

  function formatDate(value: string): string {
    const parsed = new Date(value);
    if (Number.isNaN(parsed.valueOf())) return value;
    return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(parsed);
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
      <IdentityAvatar label={message.sender || '?'} size={24} />
      <div class="header-content">
        <span class="header-line">
          <strong class="sender"><PersonLink name={message.sender || 'Unknown sender'} address={message.senderAddress} {onOpenPerson} /></strong>
          <time datetime={message.sentAt} data-mono>{formatDate(message.sentAt)}</time>
        </span>
        {#if message.recipients.length > 0}
          <span class="recipients">to {#each message.recipients as recipient, index}{#if index > 0}, {/if}<PersonLink name={recipient} {onOpenPerson} />{/each}</span>
        {/if}
        <button type="button" class="collapse-target" aria-label={`Collapse message ${message.id} from ${message.sender || 'unknown sender'}`} aria-expanded="true" onclick={() => onToggle?.(message.id)}>{message.subject || 'Collapse'}</button>
      </div>
      <MessageMenu {client} {message} {hasRenderableHTML} {viewMode} {onViewModeChange} {onOpenSettings} onTasksChange={() => tasksRefresh++} onError={(error) => menuError = error} />
    </div>

    {#if message.relatedMessageId}
      <a class="related-message" href={`/messages/${message.relatedMessageId}`}>
        {message.messageType === 'meeting_transcript' ? 'Open conversation' : 'Open meeting'}
      </a>
    {/if}
    {#if menuError}<p class="sanitize-notice" role="alert">{menuError}</p>{/if}

    {#if sanitizationFailed}
      <p class="sanitize-notice" role="alert">Could not render HTML formatting. Showing plain text.</p>
    {/if}

    <div class="body-reveal">
      <section class="card-body" aria-label="Message body">
        {#if bodyError}
          <p class="body-state" role="alert">{bodyError}</p>
        {:else if bodyPending}
          <p class="body-state" role="status">Loading message…</p>
        {:else if renderAsHTML}
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
    </div>

    {#if client && !bodyPending && !bodyError}
      {#if message.messageType === 'meeting_transcript'}<MeetingActions compact {client} request={{ scope: { message_ids: [message.id] }, limit: 200 }} {onOpenMeeting} />{/if}
      <MessageTasks {client} messageId={message.id} title={message.subject} email={!message.messageType || message.messageType === 'email'} refresh={tasksRefresh} onsettings={onOpenSettings} />
    {/if}
    {#if message.attachments.length > 0}
      <ul class="attachments" aria-label="Attachments">
        {#each message.attachments as attachment (attachment.id)}
          <li>
            {#if attachment.stored}
              <a href={`/api/v1/files/${attachment.id}/content`} download={attachment.filename}>
                {attachment.filename || '(unnamed file)'}
              </a>
            {:else}
              <span>{attachment.filename || '(unnamed file)'} (not downloaded)</span>
            {/if}
          </li>
        {/each}
      </ul>
    {/if}
    {#if client && message.attachments.length > 0}
      <MessageRecordings {client} {message} messageId={message.id} />
    {/if}
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
    <IdentityAvatar label={message.sender || '?'} size={24} />
    <span class="collapsed-sender">{message.sender || 'Unknown sender'}</span>
    <span class="collapsed-snippet">{message.snippet || message.subject}</span>
    <time datetime={message.sentAt} data-mono>{formatDate(message.sentAt)}</time>
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
    align-items: center;
    gap: var(--space-4);
    padding: var(--space-3) var(--pane-pad-x);
    border: 0;
    border-bottom: 1px solid var(--border-muted);
    background: none;
    color: var(--text-secondary);
    cursor: pointer;
    font: inherit;
    text-align: left;
    transition: background-color 80ms ease-out;
  }

  .message-card--collapsed:hover {
    background: var(--bg-surface-hover);
  }

  .body-reveal {
    display: grid;
    grid-template-rows: 1fr;
  }

  .body-reveal > .card-body {
    min-height: 0;
    overflow: hidden;
  }

  @media (prefers-reduced-motion: no-preference) {
    .body-reveal {
      animation: body-expand 180ms ease-out;
    }

    @keyframes body-expand {
      from {
        grid-template-rows: 0fr;
        opacity: 0;
      }

      to {
        grid-template-rows: 1fr;
        opacity: 1;
      }
    }
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

  .related-message {
    display: inline-block;
    margin: var(--space-3) var(--pane-pad-x) 0;
  }

  .card-header {
    display: flex;
    align-items: flex-start;
    gap: var(--space-3);
    padding: var(--space-3) var(--pane-pad-x) 0;
  }

  .header-content { display: grid; min-width: 0; flex: 1; gap: 2px; }

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

  .recipients {
    overflow: hidden;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    text-overflow: ellipsis;
    white-space: nowrap;
  }


  .sanitize-notice {
    margin: var(--space-2) var(--pane-pad-x) 0;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }

  .body-state {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .card-body {
    padding: var(--space-3) var(--pane-pad-x) var(--space-4);
  }

  .attachments {
    margin: 0;
    padding: 0 var(--pane-pad-x) var(--space-4);
    font-size: var(--font-size-sm);
    list-style: none;
  }

  /* Plain text renders on the theme surface with theme text, in the same
   * reading type and measure as themed HTML mail. */
  pre {
    max-width: 680px;
    margin: 0;
    overflow-wrap: break-word;
    color: var(--text-primary);
    font-family: var(--font-sans);
    font-size: 14px;
    line-height: 1.55;
    white-space: pre-wrap;
  }
</style>
