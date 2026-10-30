<script lang="ts">
  import { onDestroy, tick, untrack } from 'svelte';
  import { SvelteSet } from 'svelte/reactivity';

  import {
    getConversation as generatedGetConversation,
    getMessage as generatedGetMessage,
  } from '../../api/generated/api/api';
  import type { MessageDetail as GeneratedMessageDetail } from '../../api/generated/models';
  import type { APIClient } from '../../api/client';
  import type { ArchiveMessageDetail } from '../../archive/types';
  import type { MessageViewMode } from '../../archive/types';
  import MessageCard from './MessageCard.svelte';
  import { reportError, reportAction, reportWhenVisible } from '../../telemetry/actions';

  type APIMessageDetail = GeneratedMessageDetail;

  let {
    client,
    conversationId,
    anchorId,
    onAnchorChange = undefined,
    start = undefined,
    end = undefined,
  }: {
    client: APIClient;
    conversationId: number;
    anchorId: number;
    onAnchorChange?: (anchorId: number) => void;
    /** Lower UTC bound (RFC3339, inclusive) restricting the window — e.g. a
     * chat burst's local day. Omit for the default unbounded window. */
    start?: string;
    /** Upper UTC bound (RFC3339, exclusive) restricting the window. */
    end?: string;
  } = $props();

  const apiClient = untrack(() => client);
  let scroller = $state<HTMLElement>();
  let messages = $state<APIMessageDetail[]>([]);
  let activeAnchor = $state(0);
  let lastReportedAnchor = 0;
  $effect(() => {
    if (!expanded.has(activeAnchor)) {
      lastReportedAnchor = 0;
      return;
    }
    const message = messages.find((message) => message.id === activeAnchor);
    if (!message || message.body_omitted || loading || error) return;
    let cancelPending = () => {};
    const report = () => {
      cancelPending();
      cancelPending = reportWhenVisible(() => {
        lastReportedAnchor = message.id;
        void reportAction(apiClient, 'message_opened');
      });
    };
    window.addEventListener('focus', report);
    if (message.id !== lastReportedAnchor) untrack(report);
    return () => {
      cancelPending();
      window.removeEventListener('focus', report);
    };
  });
  let loading = $state(true);
  let error = $state('');
  let hasBefore = $state(false);
  let hasAfter = $state(false);
  let total = $state(0);
  let viewModes = $state<Record<number, MessageViewMode>>({});
  const expanded = new SvelteSet<number>();
  const loadingBodies = new SvelteSet<number>();
  let bodyErrors = $state<Record<number, string>>({});
  const reportedBodyErrors = new Set<number>();
  $effect(() => {
    const pending: (() => void)[] = [];
    for (const id of reportedBodyErrors) {
      if (!bodyErrors[id]) reportedBodyErrors.delete(id);
    }
    for (const message of messages) {
      const id = message.id;
      if (!bodyErrors[id] || !expanded.has(id) || reportedBodyErrors.has(id) || loading || error) continue;
      pending.push(reportError(() => apiClient, () => 'message', undefined, () => reportedBodyErrors.add(id)));
    }
    return () => pending.forEach((cancel) => cancel());
  });
  let controller: AbortController | undefined;
  let generation = 0;
  let loadedWindowKey = '';
  const anchorLoaded = $derived(messages.some((message) => message.id === activeAnchor));
  $effect(() => { if (error) return reportError(() => apiClient, () => 'message'); });

  // Expansion is local: a new anchor that is already inside the loaded
  // window expands in place without refetching. A different conversation,
  // different bounds, or an anchor outside the window reloads it.
  $effect(() => {
    const requestedConversation = conversationId;
    const requestedAnchor = anchorId;
    const requestedStart = start;
    const requestedEnd = end;
    const windowKey = `${requestedConversation}\0${requestedStart ?? ''}\0${requestedEnd ?? ''}`;
    if (windowKey === loadedWindowKey && untrack(() => messages.some((message) => message.id === requestedAnchor))) {
      activeAnchor = requestedAnchor;
      untrack(() => {
        expanded.add(requestedAnchor);
        ensureBody(requestedAnchor);
      });
      void revealAnchor(requestedAnchor);
      return;
    }
    activeAnchor = requestedAnchor;
    // The target differs from the loaded window (another conversation, other
    // bounds, or an anchor outside the slice), so the on-screen thread is
    // stale — drop it now so the loading state shows instead of the previous
    // conversation lingering under the new title while the request pends.
    messages = [];
    void loadConversation(windowKey, requestedConversation, requestedAnchor, requestedStart, requestedEnd);
  });

  onDestroy(() => {
    generation += 1;
    controller?.abort();
  });

  function errorMessage(value: unknown, fallback: string): string {
    return typeof value === 'object' && value !== null && 'message' in value ? String(value.message) : fallback;
  }

  async function loadConversation(
    windowKey: string,
    requestedConversation: number,
    requestedAnchor: number,
    requestedStart: string | undefined,
    requestedEnd: string | undefined,
  ): Promise<void> {
    generation += 1;
    const requestGeneration = generation;
    controller?.abort();
    const requestController = new AbortController();
    controller = requestController;
    loading = true;
    error = '';
    try {
      const {
        data,
        error: responseError,
        response,
      } = await generatedGetConversation(
        { id: requestedConversation },
        {
          anchor: requestedAnchor,
          before: 25,
          after: 25,
          ...(requestedStart ? { start: requestedStart } : {}),
          ...(requestedEnd ? { end: requestedEnd } : {}),
        },
        { ...apiClient, signal: requestController.signal },
      );
      if (requestGeneration !== generation || requestController.signal.aborted) return;
      loading = false;
      if (!data) {
        messages = [];
        loadedWindowKey = '';
        error = errorMessage(responseError, `Could not load conversation (${response.status})`);
        return;
      }
      messages = data.messages ?? [];
      activeAnchor = data.anchor_id;
      hasBefore = data.has_before;
      hasAfter = data.has_after;
      total = data.total;
      loadedWindowKey = windowKey;
      expanded.clear();
      expanded.add(data.anchor_id);
      loadingBodies.clear();
      bodyErrors = {};
      ensureBody(data.anchor_id);
      void revealAnchor(data.anchor_id);
    } catch (requestError) {
      if (
        requestGeneration !== generation ||
        requestController.signal.aborted ||
        (requestError instanceof DOMException && requestError.name === 'AbortError')
      )
        return;
      loading = false;
      messages = [];
      loadedWindowKey = '';
      const detail = requestError instanceof Error && requestError.message ? `: ${requestError.message}` : '';
      error = `Conversation network error${detail}`;
    }
  }

  async function revealAnchor(id: number): Promise<void> {
    await tick();
    const card = scroller?.querySelector<HTMLElement>(`[data-message-id="${id}"]`);
    if (!card || !scroller) return;
    scroller.scrollTop = Math.max(0, card.offsetTop - 8);
  }

  function toggleMessage(id: number): void {
    if (expanded.has(id)) {
      expanded.delete(id);
      return;
    }
    expanded.add(id);
    ensureBody(id);
    if (id !== activeAnchor) {
      activeAnchor = id;
      onAnchorChange?.(id);
    }
  }

  // Bodies past the server's inline budget arrive flagged body_omitted with
  // only a snippet; the full body loads on demand through the single-message
  // endpoint when the card expands.
  function ensureBody(id: number): void {
    const message = messages.find((entry) => entry.id === id);
    if (!message?.body_omitted || loadingBodies.has(id)) return;
    void loadBody(id);
  }

  async function loadBody(id: number): Promise<void> {
    const requestGeneration = generation;
    loadingBodies.add(id);
    if (id in bodyErrors) {
      const { [id]: _cleared, ...rest } = bodyErrors;
      bodyErrors = rest;
    }
    try {
      const { data, error: responseError, response } = await generatedGetMessage({ id }, apiClient);
      if (requestGeneration !== generation) return;
      if (!data) {
        bodyErrors = {
          ...bodyErrors,
          [id]: errorMessage(responseError, `Could not load message body (${response.status})`),
        };
        return;
      }
      messages = messages.map((entry) =>
        entry.id === id ? { ...entry, body: data.body, body_html: data.body_html, body_omitted: false } : entry,
      );
    } catch (requestError) {
      if (requestGeneration !== generation) return;
      const detail = requestError instanceof Error && requestError.message ? `: ${requestError.message}` : '';
      bodyErrors = { ...bodyErrors, [id]: `Could not load message body${detail}` };
    } finally {
      loadingBodies.delete(id);
    }
  }

  function changeViewMode(id: number, mode: MessageViewMode): void {
    viewModes = { ...viewModes, [id]: mode };
  }

  function archiveDetail(message: APIMessageDetail): ArchiveMessageDetail {
    return {
      id: message.id,
      conversationId: message.conversation_id ?? conversationId,
      subject: message.subject,
      sender: message.is_from_me ? 'You' : message.from,
      recipients: message.to ?? [],
      sentAt: message.sent_at,
      snippet: message.snippet,
      messageType: message.message_type,
      relatedMessageId: message.related_message_id,
      body: message.body,
      bodyHtml: message.body_html,
      attachments: (message.attachments ?? []).map((attachment) => ({
        id: attachment.id,
        stored: Boolean(attachment.content_hash),
        filename: attachment.filename,
        mimeType: attachment.mime_type,
        sizeBytes: attachment.size_bytes,
      })),
    };
  }
</script>

<!-- svelte-ignore a11y_no_noninteractive_tabindex -- the thread scroller is
     a keyboard-scrollable reading surface and the focus target Escape
     returns to from inside an archived content frame. -->
<section class="conversation-view" aria-label="Conversation thread" tabindex="0" bind:this={scroller}>
  {#if loading && messages.length === 0}
    <p role="status">Loading conversation…</p>
  {:else if error}
    <p role="alert">{error}</p>
  {:else}
    {#if hasBefore}
      <p class="bounded" role="status">
        Earlier messages are outside this view{total > 0 ? ` — showing ${messages.length} of ${total}` : ''}.
      </p>
    {/if}
    <div class="message-thread">
      {#each messages as message (message.id)}
        <MessageCard
          client={apiClient}
          message={archiveDetail(message)}
          expanded={expanded.has(message.id)}
          anchor={message.id === activeAnchor}
          viewMode={viewModes[message.id] ?? 'html'}
          bodyPending={loadingBodies.has(message.id)}
          bodyError={bodyErrors[message.id] ?? ''}
          onToggle={toggleMessage}
          onViewModeChange={changeViewMode}
        />
      {/each}
    </div>
    {#if hasAfter}<p class="bounded" role="status">Later messages are outside this view.</p>{/if}
    {#if !anchorLoaded}
      <p role="alert">The selected message is not available in this conversation window.</p>
    {/if}
  {/if}
</section>

<style>
  .conversation-view {
    position: relative;
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    overflow: auto;
    outline: none;
  }

  .conversation-view:focus-visible {
    outline: var(--focus-ring);
    outline-offset: -2px;
  }

  .message-thread {
    display: flex;
    flex-direction: column;
  }

  .message-thread > :global(.message-card:last-child) {
    border-bottom: 0;
  }

  .bounded,
  .conversation-view > [role='status'],
  .conversation-view > [role='alert'] {
    margin: 0;
    padding: var(--pane-pad-y) var(--pane-pad-x);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
</style>
