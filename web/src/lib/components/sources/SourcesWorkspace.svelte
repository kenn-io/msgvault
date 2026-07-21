<script lang="ts">
  import { Button, Spinner } from '@kenn-io/kit-ui';
  import { onDestroy, onMount } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { components } from '../../api/generated/schema';

  type SyncRun = components['schemas']['SyncRunStatus'];
  type Source = components['schemas']['SourceStatus'];

  const MIN_POLL_MS = 500;
  const MAX_POLL_MS = 8_000;
  // A terminal result older than 24 hours is explicitly called stale.
  const STALE_LAST_RESULT_MS = 24 * 60 * 60 * 1_000;

  let {
    client, maxAwaitingPolls = 6, now = () => new Date()
  }: { client: APIClient; maxAwaitingPolls?: number; now?: () => Date } = $props();
  let sources = $state<Source[]>([]);
  let loading = $state(true);
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
  let awaitingState = $state<'idle' | 'awaiting' | 'not_observed'>('idle');
  let disposed = false;

  onMount(() => {
    const visibilityChanged = (): void => {
      if (document.hidden) {
        stopPolling();
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

  // allowIdle bypasses the active-sync gate below for error-path retries:
  // a status-load failure must be able to reschedule itself even when no
  // source is actively syncing and no accepted run is being awaited, or an
  // initial/inactive-source failure would be unrecoverable until remount.
  function schedulePoll(delay: number, allowIdle = false): void {
    if (disposed || document.hidden) return;
    if (!allowIdle && !sources.some((source) => source.active_sync) && awaitingSourceID === undefined) return;
    if (timer !== undefined) clearTimeout(timer);
    timer = setTimeout(() => {
      timer = undefined;
      void load();
    }, delay);
  }

  async function load(): Promise<void> {
    if (disposed || document.hidden) return;
    const requestGeneration = ++generation;
    controller?.abort();
    const requestController = new AbortController();
    controller = requestController;
    try {
      const { data, error: responseError } = await client.GET('/api/v1/sources/status', {
        signal: requestController.signal
      });
      if (requestGeneration !== generation || disposed) return;
      if (!data) throw new Error(messageFor(responseError, 'Unable to load source status.'));
      const next = data.sources ?? [];
      let nextDelay: number | undefined;
      let advanced = false;
      const nextProgress = new Map<number, number>();
      for (const source of next) {
        if (!source.active_sync) continue;
        const processed = source.active_sync.messages_processed;
        nextProgress.set(source.id, processed);
        if (processed > (progress.get(source.id) ?? -1)) advanced = true;
      }
      progress = nextProgress;
      const awaitedSource = awaitingSourceID === undefined
        ? undefined
        : next.find((source) => source.id === awaitingSourceID);
      const acceptedRunObserved = Boolean(awaitedSource?.active_sync)
        || (awaitedSource?.latest_sync != null && awaitedSource.latest_sync.id !== awaitingBaselineRunID);
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
      if (next.some((source) => source.active_sync)) {
        pollDelay = advanced ? MIN_POLL_MS : Math.min(MAX_POLL_MS, pollDelay * 2);
        nextDelay = pollDelay;
      }
      sources = next;
      statusError = '';
      if (nextDelay !== undefined) schedulePoll(nextDelay);
    } catch (cause) {
      if (requestController.signal.aborted || requestGeneration !== generation || disposed) return;
      statusError = cause instanceof Error ? cause.message : 'Unable to load source status.';
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
        schedulePoll(nextDelay, true);
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
    try {
      const { data, error: responseError, response } = await client.POST('/api/v1/sync/{account}', {
        params: { path: { account: source.identifier } }
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
      triggerError = cause instanceof Error ? cause.message : `Unable to start sync for ${source.identifier}.`;
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

  function staleLastResult(source: Source): boolean {
    const resultAt = source.latest_sync?.completed_at ?? source.latest_sync?.started_at;
    if (!resultAt) return false;
    const timestamp = Date.parse(resultAt);
    return Number.isFinite(timestamp) && now().getTime() - timestamp > STALE_LAST_RESULT_MS;
  }

  function messageFor(value: unknown, fallback: string): string {
    return typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string'
      ? value.message : fallback;
  }
</script>

<main class="sources" aria-label="Sources">
  <header><div><p>Archive workspace</p><h1>Sources</h1></div><span>Status and incremental sync</span></header>
  {#if statusError}<p class="notice notice--error" role="alert">{statusError}</p>{/if}
  {#if triggerError}<p class="notice notice--error" role="alert">{triggerError}</p>{/if}
  {#if awaitingState === 'awaiting'}<p class="notice" role="status">Awaiting accepted sync run…</p>
  {:else if awaitingState === 'not_observed'}<p class="notice notice--error" role="status">sync_start_not_observed</p>{/if}
  {#if loading}<p role="status">Loading source status…</p>
  {:else if sources.length === 0}<p class="notice" role="status">No archived sources are available.</p>
  {:else}
    <section class="source-list" aria-label="Source status list">
      {#each sources as source (source.id)}
        <article>
          <div class="identity">
            <h2>{label(source)}</h2>
            <span>{source.source_type} · {source.identifier}</span>
            <span>Updated {source.updated_at}</span>
            {#if source.scheduled}
              <span>Scheduled · {source.schedule ?? 'schedule unavailable'}</span>
              {#if source.next_sync_at}<span>Next {source.next_sync_at}</span>{/if}
            {:else}<span>Not scheduled</span>{/if}
            {#if source.scheduler_last_error}<span class="error-copy">Scheduler: {source.scheduler_last_error}</span>{/if}
          </div>
          <div class="run-state">
            {#if source.active_sync}
              <span class="working"><Spinner size={12} label={`Syncing ${label(source)}`} /> Syncing</span>
              <strong>{source.active_sync.messages_processed.toLocaleString()} processed</strong>
              <span>{source.active_sync.messages_added.toLocaleString()} added · {source.active_sync.errors_count.toLocaleString()} errors</span>
            {:else}
              <span>Latest result</span>
              {#if source.latest_sync}
                <strong>{statusLabel(source.latest_sync)}</strong>
                {#if staleLastResult(source)}<span class="reason">stale_last_result</span>{/if}
                {#if source.latest_sync.error_message}<span class="error-copy">{source.latest_sync.error_message}</span>{/if}
                {#if source.latest_sync.item_errors?.length}
                  <details open>
                    <summary>{source.latest_sync.item_errors.length} item {source.latest_sync.item_errors.length === 1 ? 'error' : 'errors'}</summary>
                    {#each source.latest_sync.item_errors as item (`${item.source_message_id}:${item.phase}:${item.created_at}`)}
                      <span class="error-copy">{item.error_message}</span>
                    {/each}
                  </details>
                {/if}
              {:else}<strong>No prior sync result</strong>{/if}
            {/if}
          </div>
          <div class="last-success">
            <span>Last successful sync</span>
            <strong>{source.last_successful_sync?.completed_at ?? 'No successful sync result'}</strong>
          </div>
          <div class="action">
            {#if source.can_sync}
              <Button size="sm" tone="info" surface="soft" label={`Sync now ${label(source)}`} disabled={Boolean(triggering) || awaitingSourceID === source.id} onclick={() => void syncNow(source)} />
            {:else}
              <span class="reason">{source.sync_unavailable_reason ?? 'sync_unavailable'}</span>
            {/if}
          </div>
        </article>
      {/each}
    </section>
  {/if}
</main>

<style>
  .sources { display: flex; min-height: 0; flex: 1; flex-direction: column; gap: var(--space-4); padding: var(--space-5) var(--space-6); }
  header, article { display: flex; align-items: center; justify-content: space-between; gap: var(--space-4); }
  header p, h1, h2 { margin: 0; }
  header p { color: var(--accent-amber); font-size: var(--font-size-2xs); font-weight: 800; letter-spacing: .1em; text-transform: uppercase; }
  header span, article span { color: var(--text-muted); font-size: var(--font-size-xs); }
  .source-list { display: grid; border-top: 1px solid var(--border-muted); }
  article { min-height: 72px; padding: var(--space-3); border-bottom: 1px solid var(--border-muted); }
  .identity, .run-state, .last-success, details { display: grid; min-width: 0; gap: var(--space-1); }
  .identity { flex: 1; }
  .run-state, .last-success { width: min(22vw, 240px); }
  .working { display: flex; align-items: center; gap: var(--space-2); color: var(--accent-teal); }
  .reason, .error-copy { color: var(--text-danger); }
  .notice { padding: var(--space-3); border-left: 3px solid var(--accent-amber); background: var(--bg-subtle); }
  .notice--error { border-color: var(--accent-red); color: var(--text-danger); }
  @media (max-width: 760px) { article { align-items: stretch; flex-direction: column; } .run-state, .last-success { width: auto; } }
</style>
