<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import { onDestroy, untrack } from 'svelte';

  import type { components } from '../../api/generated/schema';
  import type { APIClient } from '../../api/client';
  import type { ArchiveMessageDetail } from '../../archive/types';
  import type { MessageViewMode } from '../../archive/types';
  import MessageReader from './MessageReader.svelte';

  type APIMessageDetail = components['schemas']['MessageDetail'];

  let {
    client,
    conversationId,
    anchorId,
    onBack,
    onAnchorChange = undefined,
    onContentFocusChange = undefined,
    start = undefined,
    end = undefined
  }: {
    client: APIClient;
    conversationId: number;
    anchorId: number;
    onBack: () => void;
    onAnchorChange?: (anchorId: number) => void;
    onContentFocusChange?: (focused: boolean) => void;
    /** Lower UTC bound (RFC3339, inclusive) restricting the window — e.g. a
     * chat burst's local day. Omit for the default unbounded window. */
    start?: string;
    /** Upper UTC bound (RFC3339, exclusive) restricting the window. */
    end?: string;
  } = $props();

  const apiClient = untrack(() => client);
  let messages = $state<APIMessageDetail[]>([]);
  let activeAnchor = $state(0);
  let loading = $state(true);
  let error = $state('');
  let hasBefore = $state(false);
  let hasAfter = $state(false);
  let total = $state(0);
  let viewModes = $state<Record<number, MessageViewMode>>({});
  let controller: AbortController | undefined;
  let generation = 0;
  const selected = $derived(messages.find((message) => message.id === activeAnchor));

  $effect(() => {
    const requestedConversation = conversationId;
    const requestedAnchor = anchorId;
    const requestedStart = start;
    const requestedEnd = end;
    activeAnchor = requestedAnchor;
    void loadConversation(requestedConversation, requestedAnchor, requestedStart, requestedEnd);
  });

  onDestroy(() => {
    generation += 1;
    controller?.abort();
  });

  function errorMessage(value: unknown, status: number): string {
    return typeof value === 'object' && value !== null && 'message' in value
      ? String(value.message)
      : `Could not load conversation (${status})`;
  }

  async function loadConversation(
    requestedConversation: number,
    requestedAnchor: number,
    requestedStart: string | undefined,
    requestedEnd: string | undefined
  ): Promise<void> {
    generation += 1;
    const requestGeneration = generation;
    controller?.abort();
    const requestController = new AbortController();
    controller = requestController;
    loading = true;
    error = '';
    try {
      const { data, error: responseError, response } = await apiClient.GET('/api/v1/conversations/{id}', {
        params: {
          path: { id: requestedConversation },
          query: {
            anchor: requestedAnchor, before: 25, after: 25,
            ...(requestedStart ? { start: requestedStart } : {}),
            ...(requestedEnd ? { end: requestedEnd } : {})
          }
        },
        signal: requestController.signal
      });
      if (requestGeneration !== generation || requestController.signal.aborted) return;
      loading = false;
      if (!data) {
        messages = [];
        error = errorMessage(responseError, response.status);
        return;
      }
      messages = data.messages ?? [];
      activeAnchor = data.anchor_id;
      hasBefore = data.has_before;
      hasAfter = data.has_after;
      total = data.total;
    } catch (requestError) {
      if (requestGeneration !== generation || requestController.signal.aborted ||
        (requestError instanceof DOMException && requestError.name === 'AbortError')) return;
      loading = false;
      messages = [];
      const detail = requestError instanceof Error && requestError.message
        ? `: ${requestError.message}`
        : '';
      error = `Conversation network error${detail}`;
    }
  }

  function openMessage(id: number): void {
    if (id === activeAnchor) return;
    onAnchorChange?.(id);
  }

  function changeViewMode(id: number, mode: MessageViewMode): void {
    viewModes = { ...viewModes, [id]: mode };
  }

  function archiveDetail(message: APIMessageDetail): ArchiveMessageDetail {
    return {
      id: message.id,
      conversationId: message.conversation_id ?? conversationId,
      subject: message.subject,
      sender: message.from,
      recipients: message.to ?? [],
      sentAt: message.sent_at,
      snippet: message.snippet,
      body: message.body,
      bodyHtml: message.body_html,
      attachments: (message.attachments ?? []).map((attachment) => ({
        filename: attachment.filename,
        mimeType: attachment.mime_type,
        sizeBytes: attachment.size_bytes
      }))
    };
  }
</script>

<section class="conversation-view" aria-label="Containing conversation">
  <header class="conversation-toolbar">
    <Button
      size="sm"
      surface="outline"
      label="Back"
      ariaLabel="Back from conversation"
      onclick={onBack}
    />
    <h2>Conversation</h2>
    {#if total > 0}<span>{messages.length} of {total}</span>{/if}
  </header>

  {#if loading && messages.length === 0}
    <p role="status">Loading conversation…</p>
  {:else if error}
    <p role="alert">{error}</p>
  {:else}
    {#if hasBefore}<p class="bounded" role="status">Earlier messages are outside this bounded window.</p>{/if}
    <div class="message-stack">
      {#each messages as message (message.id)}
        {#if message.id === activeAnchor}
          <article aria-label={`Selected message ${message.id}`} aria-current="true" class="selected-message">
            <MessageReader
              client={apiClient}
              message={archiveDetail(message)}
              viewMode={viewModes[message.id] ?? 'html'}
              onViewModeChange={changeViewMode}
              onContentFocusChange={onContentFocusChange}
            />
          </article>
        {:else}
          <Button
            size="md"
            surface="soft"
            label={`${message.from || 'Unknown sender'} · ${message.subject || '(untitled)'}`}
            ariaLabel={`Open message ${message.id} from ${message.from || 'unknown sender'}`}
            onclick={() => openMessage(message.id)}
          />
        {/if}
      {/each}
    </div>
    {#if hasAfter}<p class="bounded" role="status">Later messages are outside this bounded window.</p>{/if}
    {#if !selected}<p role="alert">The selected message is not available in this conversation window.</p>{/if}
  {/if}
</section>

<style>
  .conversation-view {
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    overflow: auto;
  }

  .conversation-toolbar {
    position: sticky;
    z-index: 2;
    top: 0;
    display: flex;
    align-items: center;
    gap: var(--space-3);
    padding: var(--space-3);
    border-bottom: 1px solid var(--border-default);
    background: var(--bg-surface);
  }

  .conversation-toolbar h2 {
    min-width: 0;
    flex: 1;
    margin: 0;
    font-size: var(--font-size-md);
  }

  .conversation-toolbar span,
  .bounded {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .message-stack {
    display: grid;
    gap: var(--space-3);
    padding: var(--space-3);
  }

  .message-stack > :global(.kit-button) {
    width: 100%;
    justify-content: flex-start;
  }

  .selected-message {
    padding: var(--space-3);
    border: 2px solid var(--accent-blue);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
  }

  .bounded,
  .conversation-view > [role='status'],
  .conversation-view > [role='alert'] {
    margin: 0;
    padding: var(--space-3);
  }
</style>
