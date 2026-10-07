<script lang="ts">
  import {
    listSourceStatus as generatedListSourceStatus,
    triggerSync as generatedTriggerSync,
  } from '../../api/generated/api/api';
  import { Button, Chip, IconButton, Table, TableHeaderCell } from '@kenn-io/kit-ui';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import { onDestroy, onMount } from 'svelte';
  import { SvelteSet } from 'svelte/reactivity';
  import type { APIClient } from '../../api/client';
  import { scheduleSummary } from '../../settings/cron';
  import { sourceTypeLabel, syncStatusChip, syncUnavailableLabel } from '../../sources/labels';
  import { formatDateTime } from '../../util/format';
  import PageHeader from '../shell/PageHeader.svelte';
  import type {
    SourceStatus as GeneratedSourceStatus,
    SyncRunStatus as GeneratedSyncRunStatus,
  } from '../../api/generated/models';
  type SyncRun = GeneratedSyncRunStatus;
  type Source = GeneratedSourceStatus;
  const MIN_POLL_MS = 500;
  const MAX_POLL_MS = 8000;
  // A terminal result older than 24 hours is explicitly called stale.
  const STALE_LAST_RESULT_MS = 24 * 60 * 60 * 1000;
  let {
    client,
    requestTimeoutMs = 20_000,
    maxAwaitingPolls = 6,
    maxLockHoldPolls = 8,
    now = () => new Date(),
    onOpenOperations = () => undefined,
  }: {
    client: APIClient;
    requestTimeoutMs?: number;
    maxAwaitingPolls?: number;
    maxLockHoldPolls?: number;
    now?: () => Date;
    onOpenOperations?: () => void;
  } = $props();
  let sources = $state<Source[]>([]);
  const expanded = new SvelteSet<number>();
  let loading = $state(true);
  let lockStatusStale = $state(false);
  let statusError = $state('');
  let triggerError = $state('');
  let triggering = $state<string>();
  let controller: AbortController | undefined;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let generation = 0;
  let pollDelay = MIN_POLL_MS;
  let progress = new Map<number, number>();
  let awaitingSourceID = $state<number>();
  let awaitingBaselineRunID: number | undefined;
  let awaitingAttempts = 0;
  let lockHoldPolls = 0;
  let hasLoadedStatus = false;
  let awaitingState = $state<'idle' | 'awaiting' | 'not_observed'>('idle');
  let disposed = false;
  onMount(() => {
    const visibilityChanged = (): void => {
      if (document.hidden) {
        stopPolling();
        loading = false;
      } else {
        pollDelay = MIN_POLL_MS;
        void load();
      }
    };
    document.addEventListener('visibilitychange', visibilityChanged);
    void load();
    return () => document.removeEventListener('visibilitychange', visibilityChanged);
  });
  onDestroy(() => {
    disposed = true;
    stopPolling();
  });
  function stopPolling(clearAwaiting = true): void {
    generation += 1;
    controller?.abort();
    controller = undefined;
    if (timer !== undefined) clearTimeout(timer);
    timer = undefined;
    if (clearAwaiting) {
      awaitingSourceID = undefined;
      awaitingBaselineRunID = undefined;
      awaitingAttempts = 0;
      awaitingState = 'idle';
    }
  }
  // A completed run can disappear from active_sync while the scheduler still
  // holds the sync lock (e.g. cache rebuild after the run): the source then
  // reports sync_unavailable_reason 'sync_already_running' with no active
  // sync. Polling must continue through that window or "Sync now" would stay
  // unavailable until a remount.
  function schedulerHoldsSyncLock(source: Source): boolean {
    return source.sync_unavailable_reason === 'sync_already_running';
  }
  function isOnDemandSource(source: Source): boolean {
    return source.source_type === 'meeting_import';
  }
  // Later status failures may retry in the background. A failed first load
  // waits for the user's Retry action so its error stays visible.
  function schedulePoll(delay: number, allowIdle = false, lockHold = false): void {
    if (disposed || document.hidden) return;
    if (
      !allowIdle &&
      !sources.some((source) => source.active_sync || (schedulerHoldsSyncLock(source) && lockHoldPolls < maxLockHoldPolls)) &&
      awaitingSourceID === undefined
    )
      return;
    if (timer !== undefined) clearTimeout(timer);
    timer = setTimeout(() => {
      timer = undefined;
      if (lockHold) lockHoldPolls += 1;
      void load();
    }, delay);
  }
  async function load(): Promise<void> {
    if (disposed) return;
    if (document.hidden) {
      loading = false;
      return;
    }
    if (sources.length === 0) loading = true;
    const requestGeneration = ++generation;
    controller?.abort();
    const requestController = new AbortController();
    controller = requestController;
    const signal = AbortSignal.any([requestController.signal, AbortSignal.timeout(requestTimeoutMs)]);
    try {
      const { data, error: responseError } = await generatedListSourceStatus(undefined, {
        ...client,
        signal,
      });
      if (requestGeneration !== generation || disposed) return;
      if (!data) throw new Error(messageFor(responseError, 'Unable to load source status.'));
      const next = data.sources ?? [];
      let nextDelay: number | undefined;
      let lockHoldPoll = false;
      let advanced = false;
      const nextProgress = new Map<number, number>();
      for (const source of next) {
        if (!source.active_sync) continue;
        const processed = source.active_sync.messages_processed;
        nextProgress.set(source.id, processed);
        if (processed > (progress.get(source.id) ?? -1)) advanced = true;
      }
      progress = nextProgress;
      const awaitedSource =
        awaitingSourceID === undefined ? undefined : next.find((source) => source.id === awaitingSourceID);
      const acceptedRunObserved =
        Boolean(awaitedSource?.active_sync) ||
        (awaitedSource?.latest_sync != null && awaitedSource.latest_sync.id !== awaitingBaselineRunID);
      if (acceptedRunObserved) {
        awaitingSourceID = undefined;
        awaitingBaselineRunID = undefined;
        awaitingAttempts = 0;
        awaitingState = 'idle';
        pollDelay = MIN_POLL_MS;
      } else if (awaitingSourceID !== undefined) {
        awaitingAttempts += 1;
        if (awaitingAttempts >= maxAwaitingPolls) {
          awaitingSourceID = undefined;
          awaitingState = 'not_observed';
        } else {
          awaitingState = 'awaiting';
          nextDelay = pollDelay;
          pollDelay = Math.min(MAX_POLL_MS, pollDelay * 2);
        }
      }
      const hasActiveSync = next.some((source) => source.active_sync);
      const hasLockHold = next.some(schedulerHoldsSyncLock);
      if (hasActiveSync) {
        lockHoldPolls = 0;
        lockStatusStale = false;
        pollDelay = advanced ? MIN_POLL_MS : Math.min(MAX_POLL_MS, pollDelay * 2);
        nextDelay = pollDelay;
      } else if (hasLockHold && lockHoldPolls < maxLockHoldPolls) {
        lockStatusStale = false;
        pollDelay = Math.min(MAX_POLL_MS, pollDelay * 2);
        nextDelay = pollDelay;
        lockHoldPoll = true;
      } else if (hasLockHold) {
        lockStatusStale = true;
      } else {
        lockHoldPolls = 0;
        lockStatusStale = false;
      }
      sources = next;
      statusError = '';
      hasLoadedStatus = true;
      if (nextDelay !== undefined) {
        schedulePoll(nextDelay, false, lockHoldPoll);
      }
    } catch (cause) {
      if (requestGeneration !== generation || disposed || requestController.signal.aborted) return;
      if (signal.aborted && signal.reason?.name === 'TimeoutError') {
        statusError = 'Loading source status timed out. The server may be busy; retry in a moment.';
      } else {
        statusError = cause instanceof Error ? cause.message : 'Unable to load source status.';
      }
      if (!hasLoadedStatus) return;
      if (lockHoldPolls >= maxLockHoldPolls && sources.some(schedulerHoldsSyncLock) &&
        !sources.some((source) => source.active_sync) && awaitingSourceID === undefined) {
        lockStatusStale = true;
        return;
      }
      if (awaitingSourceID !== undefined) {
        awaitingAttempts += 1;
        if (awaitingAttempts >= maxAwaitingPolls) {
          awaitingSourceID = undefined;
          awaitingState = 'not_observed';
        } else {
          awaitingState = 'awaiting';
          const nextDelay = pollDelay;
          pollDelay = Math.min(MAX_POLL_MS, pollDelay * 2);
          schedulePoll(nextDelay, true);
        }
      } else {
        const nextDelay = pollDelay;
        pollDelay = Math.min(MAX_POLL_MS, pollDelay * 2);
        schedulePoll(nextDelay, true, sources.some(schedulerHoldsSyncLock) &&
          !sources.some((source) => source.active_sync));
      }
    } finally {
      if (requestGeneration === generation) {
        controller = undefined;
        loading = false;
      }
    }
  }
  async function syncNow(source: Source): Promise<void> {
    if (!source.can_sync || triggering) return;
    triggering = source.identifier;
    triggerError = '';
    const signal = AbortSignal.timeout(requestTimeoutMs);
    try {
      const {
        data,
        error: responseError,
        response,
      } = await generatedTriggerSync({ account: source.identifier }, { source_type: source.source_type }, {
        ...client,
        signal,
      });
      if (response.status !== 202 || !data) {
        throw new Error(messageFor(responseError, `Unable to start sync for ${source.identifier}.`));
      }
      stopPolling();
      pollDelay = MIN_POLL_MS;
      awaitingSourceID = source.id;
      awaitingBaselineRunID = source.latest_sync?.id;
      awaitingAttempts = 0;
      awaitingState = 'awaiting';
      await load();
    } catch (cause) {
      triggerError = signal.aborted
        ? 'Starting sync timed out. Check source status before trying again.'
        : cause instanceof Error ? cause.message : `Unable to start sync for ${source.identifier}.`;
      schedulePoll(MIN_POLL_MS);
    } finally {
      triggering = undefined;
    }
  }
  function label(source: Source): string {
    return source.display_name || source.identifier;
  }
  function resultTimestamp(run: SyncRun | null | undefined): string | undefined {
    return run?.completed_at ?? run?.started_at;
  }
  function hasDetails(source: Source): boolean {
    return Boolean(
      source.latest_sync?.error_message || source.latest_sync?.item_errors?.length || source.scheduler_last_error
    );
  }
  function toggleDetails(id: number): void {
    if (expanded.has(id)) expanded.delete(id);
    else expanded.add(id);
  }
  function staleLastResult(source: Source): boolean {
    const resultAt = source.latest_sync?.completed_at ?? source.latest_sync?.started_at;
    if (!resultAt) return false;
    const timestamp = Date.parse(resultAt);
    return Number.isFinite(timestamp) && now().getTime() - timestamp > STALE_LAST_RESULT_MS;
  }
  function messageFor(value: unknown, fallback: string): string {
    return typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string'
      ? value.message
      : fallback;
  }
  function refresh(): void {
    stopPolling(false);
    lockHoldPolls = 0;
    lockStatusStale = false;
    statusError = '';
    pollDelay = MIN_POLL_MS;
    void load();
  }
</script>

<main class="sources" aria-label="Sources">
  <PageHeader title="Sources" description="Accounts and imports in your archive, and when they last synced.">
    {#snippet actions()}
      <Button size="sm" surface="soft" label="Sync history" onclick={onOpenOperations} />
    {/snippet}
  </PageHeader>
  {#if statusError}<div class="notice notice--error" role="alert">
      <span>{statusError}</span>
      <Button size="sm" surface="soft" label="Retry" onclick={refresh} />
    </div>{/if}
  {#if triggerError}<div class="notice notice--error" role="alert">
      <span>{triggerError}</span>
      <Button size="sm" surface="soft" label="Refresh" onclick={refresh} />
    </div>{/if}
  {#if lockStatusStale}<div class="notice" role="status">
      <span>Automatic refresh paused. Source status may be stale.</span>
      <Button size="sm" surface="soft" label="Refresh" onclick={refresh} />
    </div>{/if}
  {#if awaitingState === 'awaiting'}<p class="notice" role="status">Awaiting accepted sync run…</p>
  {:else if awaitingState === 'not_observed'}<div class="notice" role="status">
      <span>The sync was requested, but it hasn't started yet. Refresh to check again.</span>
      <Button size="sm" surface="soft" label="Refresh" onclick={refresh} />
    </div>{/if}
  {#if loading}<p role="status">Loading source status…</p>
  {:else if sources.length === 0}{#if !statusError}<p class="notice" role="status">No archived sources are available.</p>{/if}
  {:else}
    <Table ariaLabel="Source status" zebra={false} class="source-table">
      {#snippet header()}
        <TableHeaderCell label="Source" />
        <TableHeaderCell label="Schedule" />
        <TableHeaderCell label="Status" />
        <TableHeaderCell label="Last successful sync" />
        <TableHeaderCell label="Action" />
      {/snippet}
      {#each sources as source (source.id)}
        {@const detailID = `source-${source.id}-details`}
        {@const chip = syncStatusChip(source)}
        {@const latestAt = resultTimestamp(source.latest_sync)}
        <tr>
          <td>
            <div class="source-cell">
              <div class="source-title">
                {#if hasDetails(source)}
                  <IconButton
                    size="sm"
                    ariaLabel={`Show details for ${label(source)}`}
                    ariaExpanded={expanded.has(source.id)}
                    ariaControls={expanded.has(source.id) ? detailID : undefined}
                    onclick={() => toggleDetails(source.id)}
                  >
                    {#if expanded.has(source.id)}<ChevronDown size={14} aria-hidden="true" />{:else}<ChevronRight
                        size={14}
                        aria-hidden="true"
                      />{/if}
                  </IconButton>
                {/if}
                <strong class="source-name">{label(source)}</strong>
              </div>
              <span>{[sourceTypeLabel(source.source_type), source.identifier].filter(Boolean).join(' · ')}</span>
              <span
                >Updated <time datetime={source.updated_at} title={formatDateTime(source.updated_at, 'long')}
                  >{formatDateTime(source.updated_at)}</time
                ></span
              >
            </div>
          </td>
          <td>
            <div class="cell-stack">
              {#if source.scheduled}
                {#if source.schedule}
                  <strong title={source.schedule}>{scheduleSummary(source.schedule)}</strong>
                {:else}
                  <strong>Schedule unavailable</strong>
                {/if}
                {#if source.next_sync_at}
                  <span
                    >Next <time datetime={source.next_sync_at} title={formatDateTime(source.next_sync_at, 'long')}
                      >{formatDateTime(source.next_sync_at)}</time
                    ></span
                  >
                {/if}
              {:else if isOnDemandSource(source)}
                <span>On demand · imported through the API</span>
              {:else}<span>Not scheduled</span>{/if}
            </div>
          </td>
          <td>
            <div class="cell-stack">
              <Chip size="sm" tone={chip.tone} uppercase={false}>{chip.label}</Chip>
              {#if source.active_sync}
                <strong>{source.active_sync.messages_processed.toLocaleString()} processed</strong>
                <span
                  >{source.active_sync.messages_added.toLocaleString()} added · {source.active_sync.errors_count.toLocaleString()}
                  errors</span
                >
              {:else if latestAt}
                <time datetime={latestAt} title={formatDateTime(latestAt, 'long')}>{formatDateTime(latestAt)}</time>
                {#if staleLastResult(source)}<span class="stale">This result may be out of date.</span>{/if}
              {/if}
            </div>
          </td>
          <td>
            {#if source.last_successful_sync?.completed_at}
              <time
                datetime={source.last_successful_sync.completed_at}
                title={formatDateTime(source.last_successful_sync.completed_at, 'long')}
              >
                {formatDateTime(source.last_successful_sync.completed_at)}
              </time>
            {:else}
              <span>No successful sync result</span>
            {/if}
          </td>
          <td class="action-cell">
            {#if source.can_sync}
              <Button
                size="sm"
                tone="info"
                surface="soft"
                label={`Sync now ${label(source)}`}
                disabled={Boolean(triggering) || awaitingSourceID === source.id}
                onclick={() => void syncNow(source)}
              />
            {:else if isOnDemandSource(source)}
              <span>On-demand API source</span>
            {:else}
              {@const reason = source.sync_unavailable_reason ?? 'sync_unavailable'}
              <span class="reason" title={reason}>{syncUnavailableLabel(reason)}</span>
            {/if}
          </td>
        </tr>
        {#if expanded.has(source.id) && hasDetails(source)}
          <tr class="detail-row" id={detailID}>
            <td colspan="5">
              <div class="details">
                {#if source.latest_sync?.error_message}<p class="error-copy">{source.latest_sync.error_message}</p>{/if}
                {#if source.scheduler_last_error}<p class="error-copy">Scheduler: {source.scheduler_last_error}</p>{/if}
                {#if source.latest_sync?.item_errors?.length}
                  {@const count = source.latest_sync.item_errors.length}
                  <p>{count} item {count === 1 ? 'error' : 'errors'}</p>
                  <ul>
                    {#each source.latest_sync.item_errors as item (`${item.source_message_id}:${item.phase}:${item.created_at}`)}
                      <li class="error-copy">{item.error_message}</li>
                    {/each}
                  </ul>
                {/if}
              </div>
            </td>
          </tr>
        {/if}
      {/each}
    </Table>
  {/if}
</main>

<style>
  .sources {
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    gap: var(--space-4);
    padding: var(--space-5) var(--page-gutter) var(--space-4);
  }
  td span,
  td time {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  :global(.source-table) {
    overflow: auto;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
  }
  :global(.source-table .kit-table) {
    min-width: 980px;
    background: var(--bg-surface);
  }
  :global(.source-table tbody td) {
    vertical-align: middle;
  }
  :global(.source-table tbody td:first-child) {
    width: 30%;
  }
  :global(.source-table tbody td:nth-child(2)) {
    width: 20%;
  }
  :global(.source-table tbody td:nth-child(3)) {
    width: 18%;
  }
  :global(.source-table tbody td:nth-child(4)) {
    width: 18%;
  }
  .source-cell,
  .cell-stack,
  .details {
    display: grid;
    min-width: 0;
    gap: var(--space-1);
  }
  .source-name {
    color: var(--text-primary);
    font-size: var(--font-size-md);
  }
  .source-title {
    display: flex;
    min-width: 0;
    align-items: center;
    gap: var(--space-1);
  }
  .details {
    padding: var(--space-2) var(--space-3);
  }
  .details p,
  .details ul {
    margin: 0;
  }
  .details ul {
    padding-left: var(--space-5);
  }
  .action-cell {
    text-align: right;
    white-space: nowrap;
  }
  td .reason {
    color: var(--text-muted);
  }
  td .stale {
    color: var(--status-warning-ink);
  }
  .error-copy {
    color: var(--text-danger);
    font-size: var(--font-size-sm);
  }
  .notice {
    padding: var(--space-3);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-subtle);
  }
  .notice--error {
    border-color: var(--accent-red);
    color: var(--text-danger);
  }
</style>
