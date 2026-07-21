<script module lang="ts">
  import type {
    EntryRow,
    ExploreGroupDimension,
    ExplorePredicate
  } from '../../explore/models';

  export type ReadingPaneSelection =
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

  export type ReadingPaneStatus = 'ready' | 'loading' | 'missing' | 'error' | 'unavailable';
</script>

<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import { onDestroy, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import { createExploreAPI } from '../../explore/api';
  import type { ExploreCacheUnavailable, ExploreFileFact, ExploreFilter } from '../../explore/models';
  import EmptyState from '../common/EmptyState.svelte';
  import TaskLinks from '../tasks/TaskLinks.svelte';
  import AttachmentRail from './AttachmentRail.svelte';
  import ConversationView from './ConversationView.svelte';

  let {
    client,
    selection = undefined,
    targetKey = '',
    status = 'ready',
    statusMessage = '',
    unavailable = undefined,
    predicate,
    onClose = undefined,
    conversationAnchorId = undefined,
    conversationStart = undefined,
    conversationEnd = undefined,
    onConversationAnchorChange = undefined,
    onOpenSettings = undefined,
    onOpenRelationship = undefined
  }: {
    client: APIClient;
    selection?: ReadingPaneSelection;
    targetKey?: string;
    status?: ReadingPaneStatus;
    statusMessage?: string;
    unavailable?: ExploreCacheUnavailable;
    predicate: ExplorePredicate;
    onClose?: () => void;
    /** Overrides the entry's own anchor when the reader navigated within the
     * thread (URL-carried, so Back/Forward restore the same message). */
    conversationAnchorId?: number;
    /** Lower UTC bound (RFC3339, inclusive) restricting the conversation
     * window opened for this selection — e.g. a chat burst's local day. */
    conversationStart?: string;
    /** Upper UTC bound (RFC3339, exclusive) restricting the conversation window. */
    conversationEnd?: string;
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
  let tasksOpen = $state(false);
  let requestGeneration = 0;
  let requestController: AbortController | undefined;
  const title = $derived(selection
    ? selection.kind === 'entry' ? selection.row.title || '(untitled)' : selection.label
    : targetKey || 'Selected result');
  const showFiles = $derived(selection?.kind === 'group' &&
    (selection.dimension === 'participant' || selection.dimension === 'domain'));
  const conversationRow = $derived(selection?.kind === 'entry' && selection.row.conversation_id &&
    selection.row.anchor_message_id ? selection.row : undefined);
  // The thread opens immediately at the entry's own anchor; an explicit
  // anchor (in-thread navigation restored from the URL) overrides it.
  const threadAnchorId = $derived(conversationAnchorId ?? conversationRow?.anchor_message_id);
  // counterpart_participant_id is server-computed (see EntryRow): the
  // smallest non-owner participant on the entry, or absent when the owner
  // set is unknown or every participant is the owner. Unlike
  // participant_ids[0], it never resolves to the archive owner.
  const counterpartParticipantId = $derived(
    selection?.kind === 'entry' ? selection.row.counterpart_participant_id : undefined
  );
  const showTasks = $derived(selection?.kind === 'entry' &&
    selection.row.message_type === 'email' && selection.row.anchor_message_id !== undefined);

  const metaStrip = $derived.by((): string => {
    if (!selection) return '';
    if (selection.kind === 'entry') {
      const row = selection.row;
      const parts = [row.message_type, row.source_identifier, formatDate(row.occurred_at)];
      if (row.message_count > 1) parts.push(`${row.message_count.toLocaleString()} items`);
      if (row.attachment_count > 0) {
        parts.push(`${row.attachment_count.toLocaleString()} ${row.attachment_count === 1 ? 'file' : 'files'}`);
      }
      return parts.filter(Boolean).join(' · ');
    }
    const parts = [`Grouped by ${selection.dimension}`];
    if (selection.count !== undefined) parts.push(`${selection.count.toLocaleString()} items`);
    if (selection.estimatedBytes !== undefined) parts.push(formatBytes(selection.estimatedBytes));
    if (selection.latestAt) parts.push(`latest ${formatDate(selection.latestAt)}`);
    return parts.join(' · ');
  });

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

  // A new selection collapses the previous entry's Tasks disclosure.
  $effect(() => {
    void targetKey;
    void selection;
    tasksOpen = false;
  });

  onDestroy(() => {
    requestGeneration += 1;
    requestController?.abort();
  });

  function handleOpenRelationship(): void {
    if (counterpartParticipantId !== undefined) onOpenRelationship?.(counterpartParticipantId);
  }

  function formatDate(value: string): string {
    const parsed = new Date(value);
    return Number.isNaN(parsed.valueOf())
      ? value
      : new Intl.DateTimeFormat(undefined, { dateStyle: 'medium' }).format(parsed);
  }

  function formatBytes(value: number): string {
    if (value < 1024) return `${value} B`;
    if (value < 1024 * 1024) return `${Math.round(value / 1024)} KB`;
    return `${(value / (1024 * 1024)).toFixed(1)} MB`;
  }
</script>

<aside class="reading-pane" aria-label={`Reading pane: ${title}`}>
  <header class="pane-header">
    <div class="pane-heading">
      <strong class="pane-title">{title}</strong>
      {#if metaStrip}<span class="pane-meta" data-mono>{metaStrip}</span>{/if}
    </div>
    <div class="pane-actions">
      {#if showTasks}
        <details class="tasks-disclosure" bind:open={tasksOpen}>
          <summary aria-label="Tasks for this message">Tasks</summary>
        </details>
      {/if}
      {#if onOpenRelationship && counterpartParticipantId !== undefined}
        <Button
          size="sm"
          surface="outline"
          label="Open relationship"
          ariaLabel="Open relationship"
          onclick={handleOpenRelationship}
        />
      {/if}
      <Button size="sm" surface="outline" label="Close" ariaLabel="Close reading pane" onclick={() => onClose?.()} />
    </div>
  </header>

  {#if showTasks && tasksOpen && selection?.kind === 'entry'}
    <div class="tasks-sheet">
      <TaskLinks
        {client}
        messageId={selection.row.anchor_message_id!}
        title={selection.row.title || '(untitled)'}
        sourceType={selection.row.source_type}
        sourceIdentifier={selection.row.source_identifier}
        onsettings={onOpenSettings}
      />
    </div>
  {/if}

  <div class="pane-body">
    {#if conversationRow && threadAnchorId !== undefined}
      <ConversationView
        {client}
        conversationId={conversationRow.conversation_id!}
        anchorId={threadAnchorId}
        start={conversationStart}
        end={conversationEnd}
        onAnchorChange={(anchorId) => onConversationAnchorChange?.(anchorId)}
      />
    {:else if !selection}
      <section class="pane-status" aria-label="Reading pane status">
        {#if status === 'loading'}
          <p role="status">Restoring selected detail…</p>
        {:else if status === 'unavailable' && unavailable}
          <p role="alert">
            {unavailable.message} Rebuild the cache with <code>{unavailable.recovery_action}</code>.
          </p>
        {:else}
          <EmptyState
            glyph="envelope"
            label="Nothing to read here"
            hint={statusMessage || 'The selected result is no longer available in this context.'}
            role="alert"
          />
        {/if}
      </section>
    {:else if selection.kind === 'entry'}
      <section class="pane-status" aria-label="Entry details">
        <p class="preview">{selection.row.preview || 'No preview is available.'}</p>
      </section>
    {:else}
      <div class="group-scroll" aria-label="Group details" data-scroll>
        {#if showFiles}
          <AttachmentRail files={files} totalCount={totalFiles} loading={filesLoading} error={filesError} />
        {:else}
          <section class="pane-status">
            <p>No further detail is available for this group.</p>
          </section>
        {/if}
      </div>
    {/if}
  </div>
</aside>

<style>
  .reading-pane {
    display: flex;
    min-width: 0;
    min-height: 0;
    height: 100%;
    flex-direction: column;
    overflow: hidden;
    background: var(--bg-surface);
  }

  @media (prefers-reduced-motion: no-preference) {
    .reading-pane {
      animation: pane-rise 160ms ease-out;
    }

    @keyframes pane-rise {
      from {
        opacity: 0;
        transform: translateY(10px);
      }
    }
  }

  /* Machined hairline under the pane header: darker line plus a faint
   * sheen below it. */
  .pane-header {
    display: flex;
    min-height: 40px;
    flex: none;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    padding: var(--space-2) var(--space-4);
    border-bottom: 1px solid var(--border-muted);
    box-shadow: 0 1px 0 var(--hairline-sheen);
  }

  .pane-heading {
    display: flex;
    min-width: 0;
    align-items: baseline;
    gap: var(--space-4);
  }

  .pane-title {
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .pane-meta {
    flex: none;
    overflow: hidden;
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .pane-actions {
    display: flex;
    flex: none;
    align-items: center;
    gap: var(--space-2);
  }

  .tasks-disclosure summary {
    display: inline-flex;
    align-items: center;
    padding: 3px 10px;
    border: 1px solid var(--control-border);
    border-radius: var(--radius-md);
    color: var(--text-secondary);
    cursor: pointer;
    font-size: var(--font-size-xs);
    list-style: none;
  }

  .tasks-disclosure summary::-webkit-details-marker {
    display: none;
  }

  .tasks-disclosure[open] summary,
  .tasks-disclosure summary:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .tasks-sheet {
    flex: none;
    max-height: 40%;
    overflow: auto;
    padding: 0 var(--space-4);
    border-bottom: 1px solid var(--border-muted);
  }

  .pane-body {
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
  }

  .group-scroll {
    min-height: 0;
    flex: 1;
    overflow: auto;
  }

  .pane-status {
    display: grid;
    gap: var(--space-3);
    padding: var(--space-4);
  }

  .pane-status p {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    line-height: 1.5;
  }

  .preview {
    max-width: 70ch;
  }

  code {
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-primary);
    font-family: var(--font-mono);
  }
</style>
