<script lang="ts">
  import { reportError } from '../../telemetry/actions';
  import {
    listCardDAVConnections as generatedListCardDAVConnections,
    listSourceStatus as generatedListSourceStatus,
    triggerSync as generatedTriggerSync,
  } from '../../api/generated/api/api';
  import { Button, Chip, Table, TableHeaderCell } from '@kenn-io/kit-ui';
  import { onDestroy, onMount } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { CardDAVConnectionResponse, SourceStatus } from '../../api/generated/models';
  import { CardDAVController } from '../../carddav/controller.svelte';
  import { SvelteMap } from 'svelte/reactivity';
  import { contactCanSync, contactLabel, contactLastGood, contactRunState } from '../../sources/contacts';
  import { sourceRunState, sourceTypeLabel, syncRunning, syncUnavailableLabel } from '../../sources/labels';
  import { formatDateTime } from '../../util/format';
  import Dialog from '../common/Dialog.svelte';
  import { EmptyState } from '@kenn-io/kit-ui';
  import { operationStateChip } from '../../operations/labels';
  import PageHeader from '../shell/PageHeader.svelte';
  import CommandLine from './CommandLine.svelte';
  import ContactPanel from './ContactPanel.svelte';
  import SourcePanel from './SourcePanel.svelte';

  type Source = SourceStatus;
  const MIN_POLL_MS = 500;
  const MAX_POLL_MS = 8000;
  const ADD_SOURCE_COMMAND = 'msgvault add-account user@example.com';

  let {
    client,
    requestTimeoutMs = 20_000,
    maxAwaitingPolls = 6,
    maxLockHoldPolls = 8,
    onOpenOperations = () => undefined,
    onStatusChange = () => undefined,
    onOpenSettings = () => undefined,
  }: {
    client: APIClient;
    onOpenOperations?: () => void;
    onStatusChange?: () => void;
    requestTimeoutMs?: number;
    maxAwaitingPolls?: number;
    maxLockHoldPolls?: number;
    /** Opens a Settings category, such as `carddav` for the contacts account. */
    onOpenSettings?: (categoryID: string) => void;
  } = $props();

  let sources = $state<Source[]>([]);
  let connections = $state<CardDAVConnectionResponse[]>([]);
  const contactControllers = new SvelteMap<string, CardDAVController>();
  const contacts = $derived(connections.map(connection => ({
    ...connection,
    status: contactControllers.get(connection.connection)?.status ?? connection.status,
  })));
  let contactsLoading = $state(true);
  let contactsRequest: AbortController | undefined;
  let selected = $state<string>();
  let adding = $state(false);
  let loading = $state(true);
  const initialLoading = $derived((loading || contactsLoading) && !sources.length && !contacts.length);
  let lockStatusStale = $state(false);
  let awaitingState = $state<'idle' | 'awaiting' | 'not_observed'>('idle');
  let statusError = $state('');
  let contactsError = $state('');
  let triggerError = $state('');
  let triggering = $state<string>();
  $effect(() => {
    if (triggerError) return reportError(() => client, () => 'sync');
  });
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
  let disposed = false;

  const selectedSource = $derived(
    selected?.startsWith('source:') ? sources.find((source) => `source:${source.id}` === selected) : undefined,
  );
  const selectedContact = $derived(
    selected?.startsWith('contacts:')
      ? contacts.find((connection) => `contacts:${connection.connection}` === selected)
      : undefined,
  );

  onMount(() => {
    const visibilityChanged = (): void => {
      if (document.hidden) {
        stopPolling();
        loading = false;
        contactsRequest?.abort();
        contactsLoading = false;
      } else {
        pollDelay = MIN_POLL_MS;
        refresh();
      }
    };
    document.addEventListener('visibilitychange', visibilityChanged);
    refresh();
    return () => document.removeEventListener('visibilitychange', visibilityChanged);
  });
  onDestroy(() => {
    disposed = true;
    stopPolling();
    contactsRequest?.abort();
    for (const controller of contactControllers.values()) controller.destroy();
  });

  async function loadContacts(): Promise<void> {
    if (disposed || document.hidden) {
      contactsLoading = false;
      return;
    }
    if (!connections.length) contactsLoading = true;
    contactsRequest?.abort();
    const request = contactsRequest = new AbortController();
    const signal = AbortSignal.any([request.signal, AbortSignal.timeout(requestTimeoutMs)]);
    try {
      const { data } = await generatedListCardDAVConnections({ ...client, signal });
      if (signal.aborted || disposed) return;
      if (!data) throw new Error();
      connections = data.connections.filter((connection) => !connection.orphaned);
      const names = new Set(connections.map(connection => connection.connection));
      for (const [name, controller] of contactControllers) {
        if (!names.has(name)) {
          controller.destroy();
          contactControllers.delete(name);
        }
      }
      for (const connection of connections) {
        let controller = contactControllers.get(connection.connection);
        if (!controller) {
          controller = new CardDAVController(client, document, connection.connection);
          controller.status = connection.status;
          contactControllers.set(connection.connection, controller);
        }
        void controller.retryStatus();
      }
      contactsError = '';
    } catch {
      if (!request.signal.aborted && !disposed) contactsError = "Can't load contacts.";
    } finally {
      if (contactsRequest === request) {
        contactsRequest = undefined;
        contactsLoading = false;
      }
    }
  }
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
      !sources.some((source) => syncRunning(source) || (schedulerHoldsSyncLock(source) && lockHoldPolls < maxLockHoldPolls)) &&
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
    if (!sources.length) loading = true;
    const requestGeneration = ++generation;
    controller?.abort();
    const requestController = (controller = new AbortController());
    const signal = AbortSignal.any([requestController.signal, AbortSignal.timeout(requestTimeoutMs)]);
    try {
      const { data, error: responseError } = await generatedListSourceStatus(undefined, { ...client, signal });
      if (requestGeneration !== generation || disposed) return;
      if (!data) throw new Error(messageFor(responseError, 'Unable to load source status.'));
      const next = data.sources ?? [];
      sources = next;
      if (next.length) loading = false;
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
      const hasActiveSync = next.some(syncRunning);
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
        !sources.some(syncRunning) && awaitingSourceID === undefined) {
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
          !sources.some(syncRunning));
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
      if (disposed) return;
      triggerError = signal.aborted
        ? 'Starting sync timed out. Check source status before trying again.'
        : cause instanceof Error ? cause.message : `Unable to start sync for ${source.identifier}.`;
      schedulePoll(MIN_POLL_MS);
    } finally {
      triggering = undefined;
      onStatusChange();
    }
  }
  async function syncContacts(connection: CardDAVConnectionResponse): Promise<void> {
    await contactControllers.get(connection.connection)?.sync();
    if (!disposed) onStatusChange();
  }
  function staleLastResult(source: Source): boolean {
    const resultAt = source.latest_sync?.completed_at ?? source.latest_sync?.started_at;
    if (!resultAt) return false;
    const timestamp = Date.parse(resultAt);
    return Number.isFinite(timestamp) && Date.now() - timestamp > 24 * 60 * 60 * 1000;
  }
  function label(source: Source): string {
    return source.display_name || source.identifier;
  }
  function messageFor(value: unknown, fallback: string): string {
    return typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string'
      ? value.message
      : fallback;
  }
  export function refresh(): void {
    stopPolling(false);
    lockHoldPolls = 0;
    lockStatusStale = false;
    statusError = '';
    pollDelay = MIN_POLL_MS;
    void load();
    void loadContacts();
  }
  function toggle(key: string): void {
    const next = selected === key ? undefined : key;
    selected = next;
  }
  function syncing(source: Source): boolean {
    return (
      triggering === source.identifier ||
      awaitingSourceID === source.id ||
      syncRunning(source) ||
      source.scheduler_queued === true ||
      source.sync_unavailable_reason === 'sync_already_running'
    );
  }
</script>

{#snippet lastGood(value: string | null | undefined)}
  {#if value}
    <time datetime={value} title={formatDateTime(value, 'long')}>{formatDateTime(value)}</time>
  {:else}
    <span class="muted">Never</span>
  {/if}
{/snippet}

<main class="sources" aria-label="Sources">
  <PageHeader title="Sources">
    {#snippet actions()}
      <Button size="sm" label="Sync history" onclick={onOpenOperations} />
      <Button size="sm" tone="info" surface="solid" label="Add source" onclick={() => (adding = true)} />
    {/snippet}
  </PageHeader>
  {#if statusError}<div class="notice notice--error" role="alert">
      <span>{statusError}</span>
      <Button size="sm" surface="soft" label="Retry" onclick={refresh} />
    </div>{/if}
  {#if contactsError}<div class="notice notice--error" role="alert"><span>{contactsError}</span><Button size="sm" label="Retry" onclick={refresh} /></div>{/if}
  {#if lockStatusStale}<div class="notice" role="status"><span>Automatic refresh paused. Source status may be stale.</span><Button size="sm" label="Refresh" onclick={refresh} /></div>{/if}
  {#if awaitingState === 'awaiting'}<p role="status">Awaiting accepted sync run…</p>{:else if awaitingState === 'not_observed'}<div class="notice" role="status"><span>The sync was requested, but it hasn't started yet.</span><Button size="sm" label="Refresh" onclick={refresh} /></div>{/if}
  {#if triggerError}<div class="notice notice--error" role="alert">
      <span>{triggerError}</span>
      <Button size="sm" surface="soft" label="Refresh" onclick={refresh} />
    </div>{/if}
  {#if initialLoading}<p class="muted" role="status">Loading…</p>
  {:else if sources.length === 0 && contacts.length === 0}
    {#if !loading && !contactsLoading && !statusError && !contactsError}<EmptyState title="No sources yet" /><CommandLine command={ADD_SOURCE_COMMAND} />{/if}
  {:else}
    <Table ariaLabel="Sources" zebra={false} class="source-table" unsorted>
      {#snippet header()}
        <TableHeaderCell label="Source" />
        <TableHeaderCell label="Status" />
        <TableHeaderCell label="Last good sync" />
        <TableHeaderCell label="Sync now" />
      {/snippet}
      {#each sources as source (source.id)}
        {@const key = `source:${source.id}`}
        {@const chip = operationStateChip(sourceRunState(source) ?? 'Never synced')}
        <tr class:selected={selected === key} aria-selected={selected === key} onclick={() => toggle(key)}>
          <td>
            <div class="name">
              <button
                type="button"
                class="name__button"
                aria-label={`Details for ${label(source)}`}
                onclick={(event) => {
                  event.stopPropagation();
                  toggle(key);
                }}>{label(source)}</button
              >
              <span class="muted">{sourceTypeLabel(source.source_type)}</span>
            </div>
          </td>
          <td><div class="name"><Chip size="sm" tone={chip.tone} dot uppercase={false}>{chip.label}</Chip>{#if source.active_sync}<span class="muted">{source.active_sync.messages_processed.toLocaleString()} processed</span><span class="muted">{source.active_sync.messages_added.toLocaleString()} added · {source.active_sync.errors_count.toLocaleString()} errors</span>{:else if staleLastResult(source)}<span class="muted">This result may be out of date.</span>{/if}</div></td>
          <td>{@render lastGood(source.last_successful_sync?.completed_at)}</td>
          <td class="action">
            {#if source.can_sync || syncing(source)}
              <Button
                size="sm"
                tone="info"
                surface="soft"
                label={syncing(source) ? 'Syncing…' : 'Sync now'}
                ariaLabel={`Sync now ${label(source)}`}
                disabled={Boolean(triggering) || syncing(source)}
                onclick={(event) => {
                  event.stopPropagation();
                  void syncNow(source);
                }}
              />
            {:else}
              <span class="muted">{isOnDemandSource(source) ? 'On demand' : syncUnavailableLabel(source.sync_unavailable_reason)}</span>
            {/if}
          </td>
        </tr>
      {/each}
      {#if contacts.length > 0}
        <tr class="group"><th scope="rowgroup" colspan="4">Contacts</th></tr>
        {#each contacts as connection (connection.connection)}
          {@const key = `contacts:${connection.connection}`}
          {@const controller = contactControllers.get(connection.connection)!}
          {@const state = contactRunState(connection.status)}
          {@const chip = operationStateChip(state ?? 'Never synced')}
          <tr class:selected={selected === key} aria-selected={selected === key} onclick={() => toggle(key)}>
            <td>
              <div class="name">
                <button
                  type="button"
                  class="name__button"
                  aria-label={`Details for ${contactLabel(connection)}`}
                  onclick={(event) => {
                    event.stopPropagation();
                    toggle(key);
                  }}>{contactLabel(connection)}</button
                >
                <span class="muted">Contacts</span>
              </div>
            </td>
            <td><div class="name"><Chip size="sm" tone={chip.tone} dot uppercase={false}>{chip.label}</Chip>
              {#if controller.statusError}<span class="muted" role="alert">{controller.statusError}</span><Button size="sm" label="Retry" onclick={() => void controller.retryStatus()} />{/if}
              {#if controller.syncError}<span class="muted" role="alert">{controller.syncError}</span>{/if}
              {#if controller.syncStatus}<span class="muted" role="status">{controller.syncStatus}</span>{/if}
            </div></td>
            <td>{@render lastGood(contactLastGood(connection.status))}</td>
            <td class="action">
              {#if contactCanSync(connection.status) || controller.syncPending}
                <Button
                  size="sm"
                  tone="info"
                  surface="soft"
                  label={controller.syncPending ? 'Syncing…' : 'Sync now'}
                  ariaLabel={`Sync now ${contactLabel(connection)}`}
                  disabled={!controller.canSync}
                  onclick={(event) => {
                    event.stopPropagation();
                    void syncContacts(connection);
                  }}
                />
              {:else}
                <span class="muted">{connection.status.active ? 'Sync in progress' : connection.status.available ? 'Account needs attention' : 'Contacts unavailable'}</span>
              {/if}
            </td>
          </tr>
        {/each}
      {/if}
    </Table>
  {/if}
</main>

{#if selectedSource}
  {#key selectedSource.id}
    <SourcePanel
      {client}
      source={selectedSource}
      syncing={syncing(selectedSource)}
      onSync={() => void syncNow(selectedSource)}
      onclose={() => (selected = undefined)}
      onOpenSettings={() => onOpenSettings('sources')}
    />
  {/key}
{:else if selectedContact}
  <ContactPanel
    connection={selectedContact}
    controller={contactControllers.get(selectedContact.connection)!}
    onSync={() => void syncContacts(selectedContact)}
    onclose={() => (selected = undefined)}
    onOpenSettings={() => onOpenSettings('carddav')}
  />
{/if}

{#if adding}
  <Dialog title="Add source" onclose={() => (adding = false)}>
    <CommandLine command={ADD_SOURCE_COMMAND} />
  </Dialog>
{/if}

<style>
  .sources > :global(.page-header) {
    margin-bottom: calc(var(--space-5) - var(--section-gap));
  }

  .sources {
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    gap: var(--section-gap);
    padding: var(--space-5) var(--page-gutter) var(--space-4);
  }
  .muted {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  :global(.kit-table-wrapper.source-table) {
    flex: none;
    overflow: auto;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
  }
  :global(.source-table .kit-table) {
    background: var(--bg-surface);
  }
  :global(.source-table tbody td) {
    padding-inline: var(--pane-pad-x);
    vertical-align: middle;
  }
  :global(.source-table thead th) {
    padding-inline: var(--pane-pad-x);
  }
  :global(.source-table tbody tr:not(.group)) {
    cursor: pointer;
  }
  tr.selected {
    background: var(--bg-surface-hover);
  }
  .group th {
    padding: var(--pane-pad-y) var(--pane-pad-x);
    border-bottom: 1px solid var(--border-muted);
    background: var(--bg-inset);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-semibold);
    text-align: left;
  }
  .name {
    display: grid;
    min-width: 0;
    gap: var(--label-gap);
  }
  .name :global(.kit-chip) {
    justify-self: start;
  }
  .name__button {
    justify-self: start;
    padding: 0;
    border: 0;
    background: none;
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-medium);
    cursor: pointer;
    text-align: left;
  }
  .name__button:focus-visible {
    outline: var(--focus-ring);
    outline-offset: 2px;
  }
  .action {
    text-align: right;
    white-space: nowrap;
  }
  .notice {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--control-gap);
    padding: var(--pane-pad-y) var(--pane-pad-x);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-subtle);
  }
  .notice--error {
    border-color: var(--accent-red);
    color: var(--text-danger);
  }
</style>
