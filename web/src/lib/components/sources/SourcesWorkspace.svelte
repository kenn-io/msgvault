<script lang="ts">
  import {
    listSourceStatus as generatedListSourceStatus,
    triggerSync as generatedTriggerSync,
  } from '../../api/generated/api/api';
  import { Button, Chip, Spinner, Table, TableHeaderCell, type ChipTone } from '@kenn-io/kit-ui';
  import { onDestroy, onMount } from 'svelte';
  import type { APIClient } from '../../api/client';
  import { scheduleSummary } from '../../settings/cron';
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
  function statusLabel(run: SyncRun | null): string {
    if (!run) return 'Never';
    if (run.status === 'completed') return 'Completed';
    if (run.status === 'failed') return 'Failed';
    return run.status.replaceAll('_', ' ');
  }
  function statusTone(run: SyncRun): ChipTone {
    if (run.status === 'completed') return 'success';
    if (run.status === 'failed') return 'danger';
    return 'info';
  }
  function resultTimestamp(run: SyncRun | null | undefined): string | undefined {
    return run?.completed_at ?? run?.started_at;
  }
  function formatTimestamp(value: string | null | undefined): string {
    if (!value) return 'Not available';
    const date = new Date(value);
    if (!Number.isFinite(date.getTime())) return value;
    return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(date);
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
  <header>
    <div>
      <p>Archive workspace</p>
      <h1>Sources</h1>
    </div>
    <div class="header-actions">
      <span>Status and incremental sync</span>
      <Button size="sm" surface="soft" label="View source operations" onclick={onOpenOperations} />
    </div>
  </header>
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
  {:else if awaitingState === 'not_observed'}<p class="notice notice--error" role="status">
      sync_start_not_observed
    </p>{/if}
  {#if loading}<p role="status">Loading source status…</p>
  {:else if sources.length === 0}{#if !statusError}<p class="notice" role="status">No archived sources are available.</p>{/if}
  {:else}
    <Table ariaLabel="Source status" zebra={false} class="source-table">
      {#snippet header()}
        <TableHeaderCell label="Source" />
        <TableHeaderCell label="Schedule" />
        <TableHeaderCell label="Latest result" />
        <TableHeaderCell label="Last successful sync" />
        <TableHeaderCell label="Action" />
      {/snippet}
      {#each sources as source (source.id)}
        <tr>
          <td>
            <div class="source-cell">
              <strong class="source-name">{label(source)}</strong>
              <span>{source.source_type} · {source.identifier}</span>
              <span
                >Updated <time datetime={source.updated_at} title={source.updated_at}
                  >{formatTimestamp(source.updated_at)}</time
                ></span
              >
            </div>
          </td>
          <td>
            <div class="cell-stack">
              {#if source.scheduled}
                {#if source.schedule}
                  <strong title={source.schedule}>{scheduleSummary(source.schedule)}</strong>
                  <span class="schedule-expression" data-mono>{source.schedule}</span>
                {:else}
                  <strong>Schedule unavailable</strong>
                {/if}
                {#if source.next_sync_at}
                  <span
                    >Next <time datetime={source.next_sync_at} title={source.next_sync_at}
                      >{formatTimestamp(source.next_sync_at)}</time
                    ></span
                  >
                {/if}
              {:else if isOnDemandSource(source)}
                <span>On demand · imported through the API</span>
              {:else}<span>Not scheduled</span>{/if}
              {#if source.scheduler_last_error}<span class="error-copy">Scheduler: {source.scheduler_last_error}</span
                >{/if}
            </div>
          </td>
          <td>
            <div class="cell-stack">
              {#if source.active_sync}
                <span class="working"><Spinner size={12} label={`Syncing ${label(source)}`} /> Syncing</span>
                <strong>{source.active_sync.messages_processed.toLocaleString()} processed</strong>
                <span
                  >{source.active_sync.messages_added.toLocaleString()} added · {source.active_sync.errors_count.toLocaleString()}
                  errors</span
                >
              {:else if source.latest_sync}
                <Chip size="sm" tone={statusTone(source.latest_sync)} uppercase={false}
                  >{statusLabel(source.latest_sync)}</Chip
                >
                {@const latestAt = resultTimestamp(source.latest_sync)}
                {#if latestAt}<time datetime={latestAt} title={latestAt}>{formatTimestamp(latestAt)}</time>{/if}
                {#if staleLastResult(source)}<span class="reason">stale_last_result</span>{/if}
                {#if source.latest_sync.error_message}<span class="error-copy">{source.latest_sync.error_message}</span
                  >{/if}
                {#if source.latest_sync.item_errors?.length}
                  <details open>
                    <summary
                      >{source.latest_sync.item_errors.length} item {source.latest_sync.item_errors.length === 1
                        ? 'error'
                        : 'errors'}</summary
                    >
                    {#each source.latest_sync.item_errors as item (`${item.source_message_id}:${item.phase}:${item.created_at}`)}
                      <span class="error-copy">{item.error_message}</span>
                    {/each}
                  </details>
                {/if}
              {:else}<strong>No prior sync result</strong>{/if}
            </div>
          </td>
          <td>
            {#if source.last_successful_sync?.completed_at}
              <time
                datetime={source.last_successful_sync.completed_at}
                title={source.last_successful_sync.completed_at}
              >
                {formatTimestamp(source.last_successful_sync.completed_at)}
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
              <span class="reason">{source.sync_unavailable_reason ?? 'sync_unavailable'}</span>
            {/if}
          </td>
        </tr>
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
    padding: var(--space-5) var(--space-6);
  }
  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
  }
  .header-actions {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }
  header p,
  h1 {
    margin: 0;
  }
  header p {
    color: var(--status-warning-ink);
    font-size: var(--font-size-2xs);
    font-weight: 800;
    letter-spacing: 0.1em;
    text-transform: uppercase;
  }
  header span,
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
  details {
    display: grid;
    min-width: 0;
    gap: var(--space-1);
  }
  .source-name {
    color: var(--text-primary);
    font-size: var(--font-size-md);
  }
  .schedule-expression {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .working {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    color: var(--accent-teal);
  }
  .action-cell {
    text-align: right;
    white-space: nowrap;
  }
  .reason,
  .error-copy {
    color: var(--text-danger);
  }
  .notice {
    padding: var(--space-3);
    border: 1px solid var(--accent-amber);
    border-radius: var(--radius-md);
    background: var(--status-warning-bg);
  }
  .notice--error {
    border-color: var(--accent-red);
    color: var(--text-danger);
  }
</style>
