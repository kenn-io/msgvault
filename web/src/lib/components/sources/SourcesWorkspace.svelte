<script lang="ts">
  import {
    listSourceStatus as generatedListSourceStatus,
    triggerSync as generatedTriggerSync,
  } from '../../api/generated/api/api';
  import { Button, Chip, IconButton, Notice, Table, TableHeaderCell } from '@kenn-io/kit-ui';
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
  const IDLE_POLL_MS = 30_000;
  const IDLE_GRACE_POLLS = 3;
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
  let idlePollAt: number | undefined;
  let idleGracePolls = IDLE_GRACE_POLLS;
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
      !sources.some((source) => source.active_sync || source.scheduler_queued || (schedulerHoldsSyncLock(source) && lockHoldPolls < maxLockHoldPolls)) &&
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
  function scheduleIdlePoll(): void {
    const scheduled = sources.filter((source) => source.scheduled);
    if (scheduled.length === 0) return;
    const nextAt = Math.min(...scheduled.map((source) => Date.parse(source.next_sync_at ?? '')).filter(Number.isFinite));
    let delay = IDLE_POLL_MS;
    // Keep a few checks after the observed tick, even if the scheduler has
    // already advanced next_sync_at while waiting for the shared sync lock.
    if (idlePollAt !== undefined && idlePollAt <= Date.now() && idleGracePolls > 0) {
      idleGracePolls -= 1;
    } else if (Number.isFinite(nextAt) && nextAt > Date.now()) {
      idlePollAt = nextAt;
      idleGracePolls = IDLE_GRACE_POLLS;
      delay = Math.min(2_147_483_647, Math.max(MIN_POLL_MS, nextAt - Date.now()));
    } else {
      if (idleGracePolls === 0) return;
      idleGracePolls -= 1;
    }
    schedulePoll(delay, true);
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
      if (hasActiveSync || next.some((source) => source.scheduler_queued)) {
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
      } else {
        scheduleIdlePoll();
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
        !sources.some((source) => source.active_sync || source.scheduler_queued) && awaitingSourceID === undefined) {
        lockStatusStale = true;
        if (sources.some((source) => source.scheduled)) schedulePoll(IDLE_POLL_MS, true);
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
          !sources.some((source) => source.active_sync || source.scheduler_queued));
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
  <div class="sources__content">
    {#if statusError || triggerError || lockStatusStale || awaitingState !== 'idle'}
      <div class="notices">
        {#if statusError}<Notice tone="error" message={statusError} actionLabel="Retry" onaction={refresh} />{/if}
        {#if triggerError}<Notice tone="error" message={triggerError} actionLabel="Refresh" onaction={refresh} />{/if}
        {#if lockStatusStale}
          <Notice
            tone="warning"
            message={sources.some((source) => source.scheduled) ? 'Source status may be stale.' : 'Automatic refresh paused. Source status may be stale.'}
            actionLabel="Refresh"
            onaction={refresh}
          />
        {/if}
        {#if awaitingState === 'awaiting'}<Notice message="Awaiting accepted sync run…" />
        {:else if awaitingState === 'not_observed'}
          <Notice
            message="The sync was requested, but it hasn't started yet. Refresh to check again."
            actionLabel="Refresh"
            onaction={refresh}
          />
        {/if}
      </div>
    {/if}
    {#if loading}<p class="placeholder" role="status">Loading source status…</p>
    {:else if sources.length === 0}{#if !statusError}<p class="placeholder" role="status">No archived sources are available.</p>{/if}
    {:else}
      <Table ariaLabel="Source status" zebra={false} stickyHeader={false} class="source-table">
        {#snippet header()}
          <TableHeaderCell label="Source" />
          <TableHeaderCell label="Schedule" />
          <TableHeaderCell label="Status" />
          <TableHeaderCell label="Last successful sync" />
          <TableHeaderCell label="Action" class="source-table__action-head" />
        {/snippet}
        {#each sources as source (source.id)}
          {@const detailID = `source-${source.id}-details`}
          {@const chip = syncStatusChip(source)}
          {@const latestAt = resultTimestamp(source.latest_sync)}
          {@const open = expanded.has(source.id)}
          <tr class="source-row" class:source-row--open={open}>
            <td class="field field--source">
              <div class="source-identity">
                <span class="disclosure">
                  {#if hasDetails(source)}
                    <IconButton
                      size="sm"
                      ariaLabel={`Show details for ${label(source)}`}
                      ariaExpanded={open}
                      ariaControls={open ? detailID : undefined}
                      onclick={() => toggleDetails(source.id)}
                    >
                      {#if open}<ChevronDown size={14} aria-hidden="true" />{:else}<ChevronRight
                          size={14}
                          aria-hidden="true"
                        />{/if}
                    </IconButton>
                  {/if}
                </span>
                <div class="field__stack">
                  <strong class="field__primary source-name">{label(source)}</strong>
                  <span class="field__meta">{[sourceTypeLabel(source.source_type), source.identifier].filter(Boolean).join(' · ')}</span>
                  <span class="field__meta"
                    >Updated <time datetime={source.updated_at} title={formatDateTime(source.updated_at, 'long')}
                      >{formatDateTime(source.updated_at)}</time
                    ></span
                  >
                </div>
              </div>
            </td>
            <td class="field">
              <span class="field__label">Schedule</span>
              <div class="field__stack">
                {#if source.scheduled}
                  {#if source.schedule}
                    <strong class="field__primary" title={source.schedule}>{scheduleSummary(source.schedule)}</strong>
                  {:else}
                    <strong class="field__primary">Schedule unavailable</strong>
                  {/if}
                  {#if source.next_sync_at}
                    <span class="field__meta"
                      >Next <time datetime={source.next_sync_at} title={formatDateTime(source.next_sync_at, 'long')}
                        >{formatDateTime(source.next_sync_at)}</time
                      ></span
                    >
                  {/if}
                {:else if isOnDemandSource(source)}
                  <span class="field__value">On demand · imported through the API</span>
                {:else}<span class="field__value">Not scheduled</span>{/if}
              </div>
            </td>
            <td class="field">
              <span class="field__label">Status</span>
              <div class="field__stack">
                <div class="chips">
                  <Chip size="sm" tone={chip.tone} uppercase={false}>{chip.label}</Chip>
                  {#if source.scheduler_queued && chip.label !== 'Queued'}<Chip size="sm" tone="info" uppercase={false}>Queued</Chip>{/if}
                </div>
                {#if source.active_sync}
                  <strong class="field__primary">{source.active_sync.messages_processed.toLocaleString()} processed</strong>
                  <span class="field__meta"
                    >{source.active_sync.messages_added.toLocaleString()} added · {source.active_sync.errors_count.toLocaleString()}
                    errors</span
                  >
                {:else if latestAt}
                  <span class="field__meta"
                    ><time datetime={latestAt} title={formatDateTime(latestAt, 'long')}>{formatDateTime(latestAt)}</time></span
                  >
                  {#if staleLastResult(source)}<span class="field__meta stale">This result may be out of date.</span>{/if}
                {/if}
                {#if source.scheduler_queued && source.scheduler_queued_at && !source.active_sync}
                  <span class="field__meta">
                    Queued since
                    <time datetime={source.scheduler_queued_at} title={formatDateTime(source.scheduler_queued_at, 'long')}
                      >{formatDateTime(source.scheduler_queued_at)}</time
                    >
                  </span>
                {/if}
              </div>
            </td>
            <td class="field">
              <span class="field__label">Last successful sync</span>
              {#if source.last_successful_sync?.completed_at}
                <time
                  class="field__value"
                  datetime={source.last_successful_sync.completed_at}
                  title={formatDateTime(source.last_successful_sync.completed_at, 'long')}
                >
                  {formatDateTime(source.last_successful_sync.completed_at)}
                </time>
              {:else}
                <span class="field__meta">No successful sync result</span>
              {/if}
            </td>
            <td class="field field--action">
              {#if source.can_sync}
                <Button
                  size="sm"
                  tone="info"
                  surface="soft"
                  label="Sync now"
                  ariaLabel={`Sync now ${label(source)}`}
                  disabled={Boolean(triggering) || awaitingSourceID === source.id}
                  onclick={() => void syncNow(source)}
                />
              {:else if isOnDemandSource(source)}
                <span class="field__meta">On-demand API source</span>
              {:else}
                {@const reason = source.sync_unavailable_reason ?? 'sync_unavailable'}
                <span class="field__meta reason" title={reason}>{source.scheduler_queued && !source.active_sync ? 'Sync queued' : syncUnavailableLabel(reason)}</span>
              {/if}
            </td>
          </tr>
          {#if open && hasDetails(source)}
            <tr class="detail-row" id={detailID}>
              <td colspan="5">
                <div class="details">
                  {#if source.latest_sync?.error_message}<p class="error-copy">{source.latest_sync.error_message}</p>{/if}
                  {#if source.scheduler_last_error}<p class="error-copy">Scheduler: {source.scheduler_last_error}</p>{/if}
                  {#if source.latest_sync?.item_errors?.length}
                    {@const count = source.latest_sync.item_errors.length}
                    <p class="details__label">{count} item {count === 1 ? 'error' : 'errors'}</p>
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
  </div>
</main>

<style>
  .sources {
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    gap: var(--space-4);
    padding: var(--space-5) var(--page-gutter) var(--space-4);
    container-type: inline-size;
  }

  /* PageHeader's band reaches 4px past the default gap; this keeps the first
   * notice or the table a full 16px clear of it. */
  .sources__content {
    display: flex;
    min-height: 0;
    flex-direction: column;
    gap: var(--space-5);
    padding-top: var(--space-5);
  }

  .notices {
    display: grid;
    gap: var(--space-4);
  }

  .placeholder {
    margin: 0;
    padding: var(--space-6);
    border: 1px dashed var(--border-default);
    border-radius: var(--radius-md);
    color: var(--text-muted);
    font-size: var(--font-size-sm);
    text-align: center;
  }

  :global(.source-table) {
    flex: 0 1 auto;
    overflow: auto;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
  }

  :global(.source-table .kit-table) {
    background: var(--bg-surface);
    table-layout: fixed;
    min-width: 760px;
  }

  /* kit-ui's own cell rules carry the table's scoped class, so these selectors
   * need the extra .kit-table step to win. */
  :global(.source-table .kit-table thead th) {
    padding: var(--space-4) var(--space-5);
  }

  :global(.source-table thead th:nth-child(1)) {
    width: 30%;
  }

  :global(.source-table thead th:nth-child(2)) {
    width: 20%;
  }

  :global(.source-table thead th:nth-child(3)) {
    width: 20%;
  }

  :global(.source-table thead th:nth-child(4)) {
    width: 16%;
  }

  :global(.source-table .kit-table thead th.source-table__action-head) {
    width: 14%;
    text-align: right;
  }

  :global(.source-table .kit-table tbody tr > td) {
    padding: var(--space-5);
    vertical-align: top;
  }

  :global(.source-table .kit-table tbody tr.detail-row > td) {
    padding: 0 var(--space-5) var(--space-5);
  }

  :global(.source-table .kit-table tbody tr:last-child > td) {
    border-bottom: 0;
  }

  :global(.source-table .kit-table tbody tr.source-row--open > td) {
    border-bottom-color: transparent;
  }

  /* Rows are not clickable, so they keep a steady surface on hover. */
  :global(.source-table .kit-table tbody tr:is(.source-row, .detail-row):hover) {
    background: transparent;
  }

  /* Every column reads as one field: a primary value, then muted metadata. */
  .field__stack {
    display: grid;
    min-width: 0;
    justify-items: start;
    gap: var(--space-2);
  }

  .field__primary,
  .field__value {
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    line-height: 1.4;
  }

  .field__primary {
    font-weight: 600;
  }

  .field__value {
    display: block;
    padding-top: 1px;
  }

  .field__meta {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    line-height: 1.4;
    overflow-wrap: anywhere;
  }

  .source-identity {
    display: grid;
    grid-template-columns: 24px minmax(0, 1fr);
    align-items: start;
    gap: var(--space-3);
  }

  .disclosure {
    display: flex;
    justify-content: center;
    margin-top: -2px;
  }

  .source-name {
    font-size: var(--font-size-md);
    line-height: 1.3;
  }

  .chips {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
  }

  .field--action {
    text-align: right;
  }

  .stale {
    color: var(--status-warning-ink);
  }

  /* Card labels; desktop rows rely on the column headers instead. */
  .field__label {
    display: none;
  }

  .details {
    display: grid;
    gap: var(--space-2);
    margin-left: calc(24px + var(--space-3));
    padding: var(--space-4) var(--space-5);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
  }

  .details p,
  .details ul {
    margin: 0;
  }

  .details ul {
    display: grid;
    gap: var(--space-1);
    padding-left: var(--space-6);
  }

  .details__label {
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    font-weight: 600;
  }

  .error-copy {
    color: var(--text-danger);
    font-size: var(--font-size-sm);
  }

  /* Narrow panes: each source becomes a card of labelled fields. */
  @container (max-width: 760px) {
    :global(.source-table .kit-table) {
      min-width: 0;
    }

    /* Each card field carries its own visible label instead. */
    :global(.source-table thead) {
      display: none;
    }

    :global(.source-table tbody),
    :global(.source-table tbody tr) {
      display: block;
    }

    .source-row {
      display: grid;
      grid-template-columns: repeat(2, minmax(0, 1fr));
      gap: var(--space-5) var(--space-6);
      padding: var(--space-5);
      border-bottom: 1px solid var(--border-muted);
    }

    .source-row--open {
      border-bottom-color: transparent;
    }

    :global(.source-table .kit-table tbody tr:last-child) {
      border-bottom: 0;
    }

    :global(.source-table .kit-table tbody tr > td) {
      display: block;
      padding: 0;
      border-bottom: 0;
    }

    .field--source {
      grid-row: 1;
      grid-column: 1;
    }

    .field__label {
      display: block;
      margin-bottom: var(--space-2);
      color: var(--text-muted);
      font-size: var(--font-size-xs);
      font-weight: 600;
    }

    .field--action {
      grid-row: 1;
      grid-column: 2;
    }

    .details {
      margin-left: 0;
    }
  }

  @container (max-width: 420px) {
    .source-row {
      grid-template-columns: minmax(0, 1fr);
    }

    .field--action {
      grid-row: auto;
      grid-column: 1;
      text-align: left;
    }
  }
</style>
