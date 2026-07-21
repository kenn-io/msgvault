<script module lang="ts">
  import type {
    EntryRow,
    ExploreGroupDimension,
    ExplorePredicate
  } from '../../explore/models';

  export type InspectorSelection =
    | { kind: 'entry'; row: EntryRow }
    | {
        kind: 'group';
        dimension: ExploreGroupDimension;
        key: string;
        label: string;
        count?: number;
        estimatedBytes?: number;
        latestAt?: string;
      };

  export type InspectorStatus = 'ready' | 'loading' | 'missing' | 'error' | 'unavailable';
</script>

<script lang="ts">
  import { Button, SplitResizeHandle, type SplitResizeEvent } from '@kenn-io/kit-ui';
  import { onDestroy, tick, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import { createExploreAPI } from '../../explore/api';
  import type { ExploreCacheUnavailable, ExploreFileFact, ExploreFilter } from '../../explore/models';
  import ConversationView from '../reader/ConversationView.svelte';
  import TaskLinks from '../tasks/TaskLinks.svelte';
  import AttachmentRail from './AttachmentRail.svelte';

  let {
    client,
    selection = undefined,
    targetKey = '',
    status = 'ready',
    statusMessage = '',
    unavailable = undefined,
    predicate,
    width,
    onClose = undefined,
    onWidthChange = undefined,
    onContentFocusChange = undefined,
    conversationAnchorId = undefined,
    conversationStart = undefined,
    conversationEnd = undefined,
    onViewConversation = undefined,
    onBackConversation = undefined,
    onConversationAnchorChange = undefined,
    onOpenSettings = undefined,
    onOpenRelationship = undefined
  }: {
    client: APIClient;
    selection?: InspectorSelection;
    targetKey?: string;
    status?: InspectorStatus;
    statusMessage?: string;
    unavailable?: ExploreCacheUnavailable;
    predicate: ExplorePredicate;
    width: number;
    onClose?: () => void;
    onWidthChange?: (width: number) => void;
    onContentFocusChange?: (focused: boolean) => void;
    conversationAnchorId?: number;
    /** Lower UTC bound (RFC3339, inclusive) restricting the conversation
     * window opened for this selection — e.g. a chat burst's local day. */
    conversationStart?: string;
    /** Upper UTC bound (RFC3339, exclusive) restricting the conversation window. */
    conversationEnd?: string;
    onViewConversation?: (conversationId: number, anchorId: number) => void;
    onBackConversation?: () => void;
    onConversationAnchorChange?: (anchorId: number) => void;
    onOpenSettings?: () => void;
    /** Header action for entry selections that carry a participant, jumping
     * to that participant's cluster in the Relationships hub. Omitted (and
     * the button hidden) where the caller has no such destination — e.g. the
     * Relationships hub's own reading pane, whose timeline rows don't carry
     * participant IDs. */
    onOpenRelationship?: (participantID: number) => void;
  } = $props();

  const api = createExploreAPI(untrack(() => client));
  let files = $state<ExploreFileFact[]>([]);
  let totalFiles = $state(0);
  let filesLoading = $state(false);
  let filesError = $state('');
  let requestGeneration = 0;
  let requestController: AbortController | undefined;
  let resizeStartWidth = 0;
  let contentFocused = $state(false);
  let contentElement: HTMLDivElement | undefined;
  let contentFocusButton = $state<HTMLButtonElement>();
  const title = $derived(selection
    ? selection.kind === 'entry' ? selection.row.title || '(untitled)' : selection.label
    : targetKey || 'Selected result');
  const showFiles = $derived(selection?.kind === 'group' &&
    (selection.dimension === 'participant' || selection.dimension === 'domain'));
  const conversationRow = $derived(selection?.kind === 'entry' && selection.row.conversation_id &&
    selection.row.anchor_message_id ? selection.row : undefined);
  // counterpart_participant_id is server-computed (see EntryRow): the
  // smallest non-owner participant on the entry, or absent when the owner
  // set is unknown or every participant is the owner. Unlike
  // participant_ids[0], it never resolves to the archive owner.
  const counterpartParticipantId = $derived(
    selection?.kind === 'entry' ? selection.row.counterpart_participant_id : undefined
  );

  const filesPredicateFingerprint = $derived(JSON.stringify(predicate));

  $effect(() => {
    const selected = selection;
    void filesPredicateFingerprint;
    const currentPredicate = untrack(() => predicate);
    requestGeneration += 1;
    requestController?.abort();
    requestController = undefined;
    files = [];
    totalFiles = 0;
    filesError = '';
    if (!selected || selected.kind !== 'group' ||
      (selected.dimension !== 'participant' && selected.dimension !== 'domain')) {
      filesLoading = false;
      return;
    }
    const generation = requestGeneration;
    const controller = new AbortController();
    requestController = controller;
    filesLoading = true;
    const filters: ExploreFilter[] = [
      ...(currentPredicate.filters ?? []).filter((filter) => filter.dimension !== selected.dimension),
      { dimension: selected.dimension, values: [selected.key] }
    ];
    const filePredicate: ExplorePredicate = {
      ...currentPredicate,
      cursor: undefined,
      candidate_snapshot_id: undefined,
      grouping: undefined,
      filters,
      presentation: 'table'
    };
    void api.files(filePredicate, controller.signal)
      .then((loaded) => {
        if (generation !== requestGeneration || controller.signal.aborted) return;
        if (loaded.status === 'unavailable') {
          filesError = loaded.unavailable.message;
          return;
        }
        files = loaded.result.files;
        totalFiles = loaded.result.totalCount;
      })
      .catch((cause: unknown) => {
        if (generation !== requestGeneration || controller.signal.aborted) return;
        filesError = cause instanceof Error ? cause.message : 'Could not load file facts.';
      })
      .finally(() => {
        if (generation === requestGeneration) filesLoading = false;
      });
  });

  onDestroy(() => {
    requestGeneration += 1;
    requestController?.abort();
    if (contentFocused) onContentFocusChange?.(false);
  });

  async function focusContent(event: MouseEvent): Promise<void> {
    contentFocusButton = event.currentTarget as HTMLButtonElement;
    if (!contentFocused) {
      contentFocused = true;
      onContentFocusChange?.(true);
    }
    await tick();
    contentElement?.focus();
  }

  function exitContentFocus(): void {
    if (contentFocused) {
      contentFocused = false;
      onContentFocusChange?.(false);
    }
    contentFocusButton?.focus();
  }

  function handleContentKeydown(event: KeyboardEvent): void {
    if (event.key !== 'Escape' || !contentFocused) return;
    event.preventDefault();
    event.stopPropagation();
    exitContentFocus();
  }

  function beginResize(): void {
    resizeStartWidth = width;
  }

  function resize(event: SplitResizeEvent): void {
    onWidthChange?.(Math.max(320, Math.min(720, resizeStartWidth - event.deltaX)));
  }

  function handleOpenRelationship(): void {
    if (counterpartParticipantId !== undefined) onOpenRelationship?.(counterpartParticipantId);
  }

  function formatDate(value: string): string {
    const parsed = new Date(value);
    return Number.isNaN(parsed.valueOf()) ? value : parsed.toLocaleString();
  }

  function formatBytes(value: number): string {
    if (value < 1024) return `${value} B`;
    if (value < 1024 * 1024) return `${Math.round(value / 1024)} KB`;
    return `${(value / (1024 * 1024)).toFixed(1)} MB`;
  }
</script>

{#snippet header()}
  <div class="inspector-header">
    <div><span class="eyebrow">Inspector</span><strong>{title}</strong></div>
    <div class="inspector-actions">
      {#if onOpenRelationship && counterpartParticipantId !== undefined}
        <Button
          size="sm"
          surface="outline"
          label="Open relationship"
          ariaLabel="Open relationship"
          onclick={handleOpenRelationship}
        />
      {/if}
      <Button
        size="sm"
        surface="outline"
        label="Focus content"
        ariaLabel="Focus inspector content"
        onclick={(event) => { void focusContent(event); }}
      />
      <Button size="sm" surface="outline" label="Close" ariaLabel="Close inspector" onclick={() => onClose?.()} />
    </div>
  </div>
{/snippet}

{#snippet contents()}
  <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
  <div
    bind:this={contentElement}
    class="inspector-content"
    class:inspector-content--focused={contentFocused}
    role="region"
    aria-label="Inspector content"
    tabindex="-1"
    onkeydown={handleContentKeydown}
  >
    {#if conversationAnchorId !== undefined && conversationRow}
      <ConversationView
        {client}
        conversationId={conversationRow.conversation_id!}
        anchorId={conversationAnchorId}
        start={conversationStart}
        end={conversationEnd}
        onBack={() => onBackConversation?.()}
        onAnchorChange={(anchorId) => onConversationAnchorChange?.(anchorId)}
        {onContentFocusChange}
      />
    {:else if !selection}
      <section class="summary" aria-label="Inspector status">
        {#if status === 'loading'}
          <p role="status">Restoring selected detail…</p>
        {:else if status === 'unavailable' && unavailable}
          <p role="alert">
            {unavailable.message} Rebuild the cache with <code>{unavailable.recovery_action}</code>.
          </p>
        {:else}
          <p role="alert">{statusMessage || 'The selected result is no longer available in this context.'}</p>
        {/if}
      </section>
    {:else if selection.kind === 'entry'}
      <section class="summary" aria-label="Entry details">
        <p class="preview">{selection.row.preview || 'No preview is available.'}</p>
        <dl>
          <div><dt>Kind</dt><dd>{selection.row.message_type}</dd></div>
          <div><dt>Source</dt><dd>{selection.row.source_identifier}</dd></div>
          <div><dt>Occurred</dt><dd>{formatDate(selection.row.occurred_at)}</dd></div>
          <div><dt>Items</dt><dd>{selection.row.message_count.toLocaleString()}</dd></div>
          <div><dt>Files</dt><dd>{selection.row.attachment_count.toLocaleString()}</dd></div>
        </dl>
        {#if conversationRow}
          <span data-conversation-return>
            <Button
              size="sm"
              surface="soft"
              label="View conversation"
              ariaLabel="View conversation"
              onclick={() => onViewConversation?.(
                conversationRow.conversation_id!, conversationRow.anchor_message_id!
              )}
            />
          </span>
        {/if}
      </section>
      {#if selection.row.message_type === 'email' && selection.row.anchor_message_id}
        <TaskLinks
          {client}
          messageId={selection.row.anchor_message_id}
          title={selection.row.title || '(untitled)'}
          sourceType={selection.row.source_type}
          sourceIdentifier={selection.row.source_identifier}
          onsettings={onOpenSettings}
        />
      {/if}
    {:else}
      <section class="summary" aria-label="Group details">
        <p class="group-kind">Grouped by {selection.dimension}</p>
        <dl>
          {#if selection.count !== undefined}<div><dt>Items</dt><dd>{selection.count.toLocaleString()} items</dd></div>{/if}
          {#if selection.estimatedBytes !== undefined}<div><dt>Estimated</dt><dd>{formatBytes(selection.estimatedBytes)}</dd></div>{/if}
          {#if selection.latestAt}<div><dt>Latest</dt><dd>{formatDate(selection.latestAt)}</dd></div>{/if}
        </dl>
      </section>
    {/if}
    {#if showFiles}
      <AttachmentRail files={files} totalCount={totalFiles} loading={filesLoading} error={filesError} />
    {/if}
  </div>
{/snippet}

<div class="pinned-inspector">
  <SplitResizeHandle ariaLabel="Resize inspector" onResizeStart={beginResize} onResize={resize} />
  <aside aria-label={`Inspect ${title}`} style:width={`${width}px`}>
    {@render header()}
    <div class="pinned-body">{@render contents()}</div>
  </aside>
</div>

<style>
  .pinned-inspector {
    display: flex;
    min-height: 0;
    height: 100%;
    flex: none;
  }

  aside {
    display: flex;
    min-width: 320px;
    max-width: 720px;
    flex-direction: column;
    overflow: hidden;
    border: 1px solid var(--border-default);
    border-left: 0;
    border-radius: 0 var(--radius-md) var(--radius-md) 0;
    background: var(--bg-surface);
  }

  .inspector-header {
    display: flex;
    width: 100%;
    min-width: 0;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
  }

  .inspector-header > div:first-child {
    display: grid;
    min-width: 0;
    gap: var(--space-1);
  }

  .inspector-header strong {
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .eyebrow,
  .group-kind {
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    letter-spacing: 0.06em;
    text-transform: uppercase;
  }

  .inspector-actions {
    display: flex;
    flex: none;
    gap: var(--space-2);
  }

  .pinned-body {
    min-height: 0;
    flex: 1;
    overflow: auto;
  }

  .inspector-content {
    display: flex;
    min-height: 100%;
    flex-direction: column;
  }

  .inspector-content--focused:focus {
    outline: 2px solid var(--accent-blue);
    outline-offset: -2px;
  }

  .summary {
    display: grid;
    gap: var(--space-4);
    padding: var(--space-5);
  }

  .summary p,
  dl {
    margin: 0;
  }

  .preview {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    line-height: 1.5;
  }

  dl {
    display: grid;
    gap: var(--space-2);
  }

  dl div {
    display: grid;
    grid-template-columns: 88px minmax(0, 1fr);
    gap: var(--space-3);
    font-size: var(--font-size-xs);
  }

  dt {
    color: var(--text-muted);
  }

  dd {
    min-width: 0;
    margin: 0;
    overflow-wrap: anywhere;
    color: var(--text-primary);
  }

  @media (max-width: 760px) {
    .pinned-inspector {
      width: 100%;
      background: var(--bg-surface);
    }

    .pinned-inspector > :global(.kit-split-resize-handle) {
      display: none;
    }

    aside {
      width: 100% !important;
      max-width: none;
      border: 0;
      border-radius: 0;
    }
  }
</style>
