<script lang="ts">
  import { Button, DateRangePicker, SelectDropdown, resolveRange, type RangeSelection } from '@kenn-io/kit-ui';
  import { onMount, tick, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { OperationsController } from '../../operations/controller.svelte';
  import type { OperationAction, OperationsURLState } from '../../operations/models';
  import { OPERATION_KIND_LABELS, RELATED_STATUS_LABELS, type RelatedStatus } from '../../operations/labels';
  import { operationFocusAnchor, resolveOperationFocusAnchor, type OperationFocusAnchor } from '../../operations/focus';
  import OperationRelatedStatus from '../operations/OperationRelatedStatus.svelte';
  import OperationRunDetail from '../operations/OperationRunDetail.svelte';
  import OperationRunTable from '../operations/OperationRunTable.svelte';
  import Dialog from '../common/Dialog.svelte';

  let { client, controller, state: local = $bindable(), onclose, onNavigate, onConfigure, onAnnounce = () => undefined }: {
    client: APIClient;
    controller: OperationsController;
    state: OperationsURLState;
    onclose: () => void;
    onNavigate: (target: RelatedStatus) => void;
    onConfigure: () => void;
    onAnnounce?: (message: string) => void;
  } = $props();
  let root = $state<HTMLElement>();
  let anchor: OperationFocusAnchor | undefined;
  let invokingRow: HTMLButtonElement | undefined;
  let relatedFocus: string | undefined;
  const current = $derived(controller.snapshot);
  const kindOptions = [{ value: '', label: 'All kinds' }, { value: 'lane:messages', label: 'All messages' }, { value: 'lane:person_facts', label: 'All people' }, { value: 'lane:contacts', label: 'All contacts' }, { value: 'lane:documents', label: 'All documents' }, { value: 'lane:visual_attachments', label: 'All visual attachments' }, ...Object.entries(OPERATION_KIND_LABELS).map(([value, label]) => ({ value, label }))];
  const stateOptions = [{ value: '', label: 'All states' }, ...['queued', 'running', 'succeeded', 'partial', 'paused', 'failed', 'cancelled'].map(value => ({ value, label: value[0]!.toUpperCase() + value.slice(1) }))];
  const dates = $derived<RangeSelection>({ mode: 'custom', from: local.operationStartedFrom.slice(0, 10), to: local.operationStartedBefore ? dayOffset(local.operationStartedBefore.slice(0, 10), -1).slice(0, 10) : '' });
  const configured = $derived(current.statusLanes.flatMap(lane => lane.kinds).find(lane => lane.related_status === local.operationStatus)?.configured);
  onMount(() => {
    if (controller.snapshot.historyReadable || controller.snapshot.runsError) void controller.refresh();
  });
  $effect(() => {
    const wanted = { ...local };
    untrack(() => { void controller.applyURLState(wanted); });
  });
  function patchFilter(patch: Partial<OperationsURLState>): void {
    local = { ...local, ...patch, operationRunID: null, operationStatus: '' };
  }
  function selectRun(id: string, button: HTMLButtonElement): void {
    anchor = operationFocusAnchor(current.rows, id);
    invokingRow = button;
    local = { ...local, operationRunID: id };
  }
  async function allRuns(): Promise<void> {
    const id = resolveOperationFocusAnchor(current.rows, anchor) ?? local.operationRunID;
    local = { ...local, operationRunID: null };
    await tick();
    const fallback = Array.from(root?.querySelectorAll<HTMLButtonElement>('button[data-run-id]') ?? []).find(button => button.dataset.runId === id);
    (invokingRow?.isConnected ? invokingRow : fallback)?.focus();
  }
  function navigateStatus(target: RelatedStatus, button: HTMLButtonElement): void {
    relatedFocus = button.textContent?.trim();
    onNavigate(target);
  }
  async function closeRelatedStatus(): Promise<void> {
    local = { ...local, operationStatus: '' };
    await tick();
    const buttons = Array.from(root?.querySelectorAll<HTMLButtonElement>('button') ?? []);
    (buttons.find(button => button.textContent?.trim() === relatedFocus) ?? buttons[0])?.focus();
    relatedFocus = undefined;
  }
  const actionSuccess: Record<OperationAction, string> = {
    carddav_sync: 'CardDAV sync request completed; current operation state was refreshed.',
    visual_build: 'Visual index build request completed; current operation state was refreshed.',
    visual_resume: 'Visual index resume request completed; current operation state was refreshed.'
  };
  async function runAction(action: OperationAction): Promise<void> {
    const outcome = await controller.runAction(action);
    if (outcome === 'succeeded') onAnnounce(actionSuccess[action]);
    else if (outcome !== 'discarded') onAnnounce(controller.snapshot.actionConflict ?? controller.snapshot.actionError ?? '');
  }
  async function refresh(event: MouseEvent): Promise<void> {
    const button = event.currentTarget as HTMLButtonElement;
    const focused = document.activeElement === button;
    await controller.refresh();
    await tick();
    if (focused && document.activeElement === document.body && button.isConnected) button.focus();
  }
  function dayOffset(date: string, days: number): string {
    const value = new Date(`${date}T00:00:00Z`);
    value.setUTCDate(value.getUTCDate() + days);
    return value.toISOString().replace('.000Z', 'Z');
  }
  function changeDates(selection: RangeSelection): void {
    if (selection.mode === 'relative' && selection.days === 0) {
      patchFilter({ operationStartedFrom: '', operationStartedBefore: '' });
      return;
    }
    const range = selection.mode === 'custom' ? selection : resolveRange(selection);
    patchFilter({ operationStartedFrom: range.from ? `${range.from}T00:00:00Z` : '', operationStartedBefore: range.to ? dayOffset(range.to, 1) : '' });
  }
</script>

<Dialog title={local.operationStatus ? RELATED_STATUS_LABELS[local.operationStatus] : current.detail && local.operationRunID ? OPERATION_KIND_LABELS[current.detail.kind] : 'Past runs'} {onclose}>
  <div class="history" bind:this={root}>
    {#if local.operationStatus}
      {#if current.statusReadable || current.statusError}
        {#key `${local.operationStatus}:${configured}`}
          <OperationRelatedStatus {client} authority={local.operationStatus} {configured}
            onClose={local.operationRunID ? () => void closeRelatedStatus() : onclose} {onConfigure} />
        {/key}
      {:else}<p class="muted" role="status">Loading status…</p>{/if}
    {:else if local.operationRunID}
      <Button size="sm" surface="soft" label="All runs" onclick={() => void allRuns()} />
      {#if current.detailLoading}<p class="muted" role="status">Loading run…</p>{/if}
      {#if current.detailError}<p class="error" role="alert">{current.detailError}</p>{/if}
      {#if current.detail}
        <OperationRunDetail detail={current.detail} actionPending={current.actionPending}
          onNavigate={navigateStatus} onAction={(action) => void runAction(action)} />
      {/if}
    {:else}
      <div class="filters" aria-label="Operation filters">
        <SelectDropdown title="Kind" value={local.operationKind || (local.operationLane ? `lane:${local.operationLane}` : '')} options={kindOptions} onchange={(value) => patchFilter({ operationLane: value.startsWith('lane:') ? value.slice(5) as OperationsURLState['operationLane'] : '', operationKind: value.startsWith('lane:') ? '' : value as OperationsURLState['operationKind'] })} />
        <SelectDropdown title="State" value={local.operationState} options={stateOptions} onchange={(value) => patchFilter({ operationState: value as OperationsURLState['operationState'] })} />
        <DateRangePicker selection={dates} onSelect={changeDates} maxDate={new Date().toISOString().slice(0, 10)} />
        {#if local.operationStartedFrom || local.operationStartedBefore}
          <Button size="sm" surface="soft" label="Clear operation dates" onclick={() => patchFilter({ operationStartedFrom: '', operationStartedBefore: '' })} />
        {/if}
      </div>
      {#if current.initialLoading || current.backgroundLoading}<p class="muted" role="status">Loading…</p>{/if}
      {#if current.historyReadable && current.rows.length === 0 && !current.runsError && !current.conflict}<p class="muted">No runs match.</p>{/if}
      <div class="runs"><OperationRunTable rows={current.rows} onSelect={selectRun} /></div>
      {#if current.nextCursor}<Button size="sm" surface="soft" label={current.paging ? 'Loading…' : 'Show more'} disabled={current.paging} onclick={() => void controller.loadMore()} />{/if}
    {/if}
    {#if !local.operationStatus}<Button size="sm" surface="soft" label="Refresh" disabled={current.initialLoading || current.backgroundLoading || current.detailLoading} onclick={(event) => void refresh(event)} />{/if}
    {#each current.unavailableKinds as unavailable (unavailable.kind)}<p class="muted" role="status">{OPERATION_KIND_LABELS[unavailable.kind]} history is unavailable.</p>{/each}
    {#if current.statusError}<p class="error" role="alert">{current.statusError}</p>{/if}
    {#if current.runsError}<p class="error" role="alert">{current.runsError}</p>{/if}
    {#if current.conflict}<p class="error" role="alert">{current.conflict}</p>{#if current.restartRequired}<Button size="sm" label="Restart" onclick={() => void controller.restart()} />{/if}{/if}
    {#if current.actionPending}<p class="muted" role="status">Starting…</p>{/if}
    {#if current.actionConflict}<p class="error" role="alert">{current.actionConflict}</p>{/if}
    {#if current.actionError}<p class="error" role="alert">{current.actionError}</p>{/if}
  </div>
</Dialog>

<style>
  .history { display: grid; gap: var(--space-4); min-width: 0; }
  .filters { display: flex; align-items: center; flex-wrap: wrap; gap: var(--control-gap); }
  .runs { max-height: 50vh; overflow: auto; }
  .muted { margin: 0; color: var(--text-muted); font-size: var(--font-size-sm); }
  .error { margin: 0; color: var(--status-error-ink); font-size: var(--font-size-sm); }
</style>
