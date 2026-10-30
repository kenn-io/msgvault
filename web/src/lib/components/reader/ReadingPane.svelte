<script module lang="ts">
  import type { MeetingExploreScope, MessageDetail } from "../../api/generated/models";
  import type {
    EntryRow,
    ExploreGroupDimension,
    ExplorePredicate
  } from '../../explore/models';

  export type ReadingPaneSelection =
    | { kind: 'entry'; row: EntryRow }
    | { kind: 'archive'; message: MessageDetail }
    | {
        kind: 'group';
        dimension: ExploreGroupDimension;
        key: string;
        label: string;
        count?: number;
        estimatedBytes?: number;
        latestAt?: string;
        /** Authority from this group detail result, never the outer list. */
        meetingScope?: MeetingExploreScope;
      };

  export type ReadingPaneStatus = 'ready' | 'loading' | 'missing' | 'error' | 'unavailable';

</script>

<script lang="ts">
  import { messageTitle } from '../../archive/message-title';
  import { Button, EmptyState, IconButton } from '@kenn-io/kit-ui';
  import X from '@lucide/svelte/icons/x';
  import { onDestroy, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { MeetingRef } from '../../api/generated/models';
  import { createExploreAPI } from '../../explore/api';
  import { entryKindPresentation } from '../../explore/labels';
  import { filtersForGroup } from '../../explore/group-context';
  import type { ExploreCacheUnavailable, ExploreFileFact, ExploreFilter } from '../../explore/models';
  import { isEmailMessageType } from '../../explore/models';
  import { formatBytes } from '../../util/format';
  import IdentityBadge from '../explore/IdentityBadge.svelte';
  import MeetingPanel from '../meetings/MeetingPanel.svelte';
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
    onRetry = undefined,
    conversationAnchorId = undefined,
    conversationStart = undefined,
    conversationEnd = undefined,
    onConversationAnchorChange = undefined,
    onOpenPerson = undefined,
    onOpenSettings = undefined,
    onOpenMeeting = undefined,
    onReloadMeetings = undefined
  }: {
    client: APIClient;
    onOpenPerson?: (participantID: number) => void;
    selection?: ReadingPaneSelection;
    targetKey?: string;
    status?: ReadingPaneStatus;
    statusMessage?: string;
    unavailable?: ExploreCacheUnavailable;
    predicate: ExplorePredicate;
    onClose?: () => void;
    onRetry?: () => void;
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
    /** Opens the exact generated archive reference carried by meeting action evidence. */
    onOpenMeeting?: (meeting: MeetingRef) => void;
    onReloadMeetings?: () => void;
  } = $props();

  const api = createExploreAPI(untrack(() => client));
  let files = $state<ExploreFileFact[]>([]);
  let totalFiles = $state(0);
  let filesLoading = $state(false);
  let filesError = $state('');
  let requestGeneration = 0;
  let requestController: AbortController | undefined;
  const title = $derived(selection
    ? selection.kind === 'entry' ? selection.row.title || '(untitled)' : selection.kind === 'archive' ? messageTitle(selection.message.subject, selection.message.conversation_title) || '(untitled)' : selection.label
    : targetKey || 'Selected result');
  const showFiles = $derived(selection?.kind === 'group' &&
    (selection.dimension === 'participant' || selection.dimension === 'domain'));
  const conversationRow = $derived(selection?.kind === 'entry' && selection.row.conversation_id &&
    selection.row.anchor_message_id ? selection.row : undefined);
  const counterpartParticipantId = $derived(selection?.kind === 'entry' ? selection.row.counterpart_participant_id : undefined);
  const archiveMessage = $derived(selection?.kind === 'archive' ? selection.message : undefined);
  const conversationID = $derived(archiveMessage?.conversation_id ?? conversationRow?.conversation_id);
  // The thread opens immediately at the entry's own anchor; an explicit
  // anchor (in-thread navigation restored from the URL) overrides it.
  const threadAnchorId = $derived(conversationAnchorId ?? archiveMessage?.id ?? conversationRow?.anchor_message_id);
  const metaStrip = $derived.by((): string => {
    if (!selection) return '';
    if (selection.kind === 'archive') return `${entryKindPresentation('', selection.message.message_type).name} · ${formatDate(selection.message.sent_at)}`;
    if (selection.kind === 'entry') {
      const row = selection.row;
      const parts = [entryKindPresentation(row.kind, row.message_type).name, row.source_identifier, formatDate(row.occurred_at)];
      if (row.message_count > 1) parts.push(`${row.message_count.toLocaleString()} items`);
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
    // filtersForGroup appends the group membership filter alongside any
    // existing filter(s) on the same dimension (participant/domain), so a
    // drill-down under an existing filter narrows to their intersection
    // rather than widening to every entry in the drilled-into group alone.
    const filters: ExploreFilter[] =
      filtersForGroup(currentPredicate.filters ?? [], selected.dimension, selected.key) ?? [];
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

  onDestroy(() => { requestGeneration += 1; requestController?.abort(); });

  function formatDate(value: string): string {
    const parsed = new Date(value);
    return Number.isNaN(parsed.valueOf())
      ? value
      : new Intl.DateTimeFormat(undefined, { dateStyle: 'medium' }).format(parsed);
  }

</script>

<aside class="reading-pane" aria-label={`Reading pane: ${title}`}>
  <header class="pane-header">
    <div class="pane-heading">
      <strong class="pane-title">{title}</strong>
      {#if metaStrip}<span class="pane-meta" data-mono>{metaStrip}</span>{/if}
      {#if selection?.kind === 'entry' && isEmailMessageType(selection.row.message_type)}
        <IdentityBadge
          senderIdentities={selection.row.matched_sender_identities}
          recipientIdentities={selection.row.matched_recipient_identities}
        />
      {/if}
    </div>
    <div class="pane-actions">
      {#if onOpenPerson && counterpartParticipantId !== undefined}
        <Button size="sm" surface="outline" label="Open person" onclick={() => { if (counterpartParticipantId !== undefined) onOpenPerson?.(counterpartParticipantId); }} />
      {/if}
      <IconButton size="sm" ariaLabel="Close reading pane" onclick={() => onClose?.()}>
        <X size={14} />
      </IconButton>
    </div>
  </header>

  <div class="pane-body">
    {#if conversationID !== undefined && threadAnchorId !== undefined}
      <ConversationView
        {onOpenPerson}
        {onOpenSettings}
        {onOpenMeeting}
        {client}
        conversationId={conversationID}
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
          <div role={status === 'error' || status === 'missing' ? 'alert' : undefined}>
            <EmptyState
              title="Nothing to read here"
              description={statusMessage || 'The selected result is no longer available in this context.'}
            />
            {#if status === 'error' && onRetry}<Button label="Retry" size="sm" surface="soft" onclick={onRetry} />{/if}
          </div>
        {/if}
      </section>
    {:else if selection.kind === 'entry' || selection.kind === 'archive'}
      <section class="pane-status" aria-label="Entry details">
        <p class="preview">{(selection.kind === 'entry' ? selection.row.preview : selection.message.snippet) || 'No preview is available.'}</p>
      </section>
    {:else}
      <div class="group-scroll" aria-label="Group details">
        {#if showFiles}
          {#if selection.meetingScope}
            <MeetingPanel {client} scope={{ kind: 'explore', explore: selection.meetingScope }}
              onReloadScope={onReloadMeetings} {onOpenMeeting} />
          {:else}
            <p class="pane-status" role="status">Reload this group to load meeting activity with its current search scope.</p>
          {/if}
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
    flex-wrap: wrap;
    justify-content: space-between;
    gap: var(--control-gap) var(--section-gap);
    padding: var(--pane-pad-y) var(--pane-pad-x);
    border-bottom: 1px solid var(--border-muted);
    box-shadow: 0 1px 0 var(--hairline-sheen);
  }

  /* A narrow pane wraps the actions below the heading instead of squeezing the title away. */
  .pane-heading {
    display: flex;
    min-width: 0;
    flex: 1 1 12rem;
    align-items: baseline;
    gap: var(--field-gap);
  }

  .pane-title {
    min-width: min(8rem, 100%);
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .pane-meta {
    min-width: 0;
    flex: 0 1 auto;
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
    gap: var(--control-gap);
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
    gap: var(--field-gap);
    padding: var(--pane-pad-y) var(--pane-pad-x);
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
