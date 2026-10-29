<script lang="ts">
  import { Button, DateRangePicker, SelectDropdown, resolveRange, type RangeSelection } from '@kenn-io/kit-ui';
  import { tick, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { OperationsController } from '../../operations/controller.svelte';
  import type { OperationsURLState } from '../../operations/models';
  import { OPERATION_KIND_LABELS, STATUS_AUTHORITY_BY_KIND, type RelatedStatus } from '../../operations/labels';
  import { operationFocusAnchor, resolveOperationFocusAnchor, type OperationFocusAnchor } from '../../operations/focus';
  import OperationRelatedStatus from '../operations/OperationRelatedStatus.svelte';
  import OperationRunDetail from '../operations/OperationRunDetail.svelte';
  import OperationRunTable from '../operations/OperationRunTable.svelte';
  import Dialog from '../common/Dialog.svelte';

  let { client, controller, state: local = $bindable(), archiveContextKey = '', onclose, onNavigate, onConfigure }: {
    client: APIClient;
    controller: OperationsController;
    state: OperationsURLState;
    archiveContextKey?: string;
    onclose: () => void;
    onNavigate: (target: RelatedStatus) => void;
    onConfigure: () => void;
  } = $props();
  let root = $state<HTMLElement>();
  let anchor: OperationFocusAnchor | undefined;
  let invokingRow: HTMLButtonElement | undefined;
  let relatedFocus: string | undefined;
  const current = $derived(controller.snapshot);
  const kindOptions = [{ value: '', label: 'All kinds' }, ...Object.entries(OPERATION_KIND_LABELS).map(([value, label]) => ({ value, label }))];
  const stateOptions = [{ value: '', label: 'All states' }, ...['queued', 'running', 'succeeded', 'partial', 'paused', 'failed', 'cancelled'].map(value => ({ value, label: value[0]!.toUpperCase() + value.slice(1) }))];
  const dates = $derived<RangeSelection>({ mode: 'custom', from: local.operationStartedFrom.slice(0, 10), to: local.operationStartedBefore ? dayOffset(local.operationStartedBefore.slice(0, 10), -1).slice(0, 10) : '' });
  const configured = $derived(current.statusLanes.flatMap(lane => lane.kinds).find(lane => STATUS_AUTHORITY_BY_KIND[lane.kind as keyof typeof STATUS_AUTHORITY_BY_KIND] === local.operationStatus)?.configured);
  $effect(() => {
    const wanted = { ...local };
    const context = archiveContextKey;
    untrack(() => { void controller.applyURLState(wanted, context); });
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
  function dayOffset(date: string, days: number): string {
    const value = new Date(`${date}T00:00:00Z`);
    value.setUTCDate(value.getUTCDate() + days);
    return value.toISOString().replace('.000Z', 'Z');
  }
  function changeDates(selection: RangeSelection): void {
    const range = selection.mode === 'custom' ? selection : resolveRange(selection);
    patchFilter({ operationStartedFrom: range.from ? `${range.from}T00:00:00Z` : '', operationStartedBefore: range.to ? dayOffset(range.to, 1) : '' });
  }
</script>

<Dialog title={current.detail && local.operationRunID ? OPERATION_KIND_LABELS[current.detail.kind] : 'Past runs'} {onclose}>
  <div class="history" bind:this={root}>
    {#if local.operationStatus}
      {#key local.operationStatus}
        <OperationRelatedStatus {client} authority={local.operationStatus} {configured}
          onClose={() => void closeRelatedStatus()} {onConfigure} />
      {/key}
    {:else if local.operationRunID}
      <Button size="sm" surface="soft" label="All runs" onclick={() => void allRuns()} />
      {#if current.detailLoading}<p class="muted" role="status">Loading run…</p>{/if}
      {#if current.detailError}<p class="error" role="alert">{current.detailError}</p>{/if}
      {#if current.detail}
        <OperationRunDetail detail={current.detail} compact showClose={false} actionPending={current.actionPending}
          onNavigate={navigateStatus} onAction={(action) => void controller.runAction(action)} />
      {/if}
    {:else}
      <div class="filters" aria-label="Operation filters">
        <SelectDropdown title="Kind" value={local.operationKind} options={kindOptions} onchange={(value) => patchFilter({ operationKind: value as OperationsURLState['operationKind'] })} />
        <SelectDropdown title="State" value={local.operationState} options={stateOptions} onchange={(value) => patchFilter({ operationState: value as OperationsURLState['operationState'] })} />
        <DateRangePicker selection={dates} onSelect={changeDates} maxDate={new Date().toISOString().slice(0, 10)} />
      </div>
      {#if current.initialLoading || current.backgroundLoading}<p class="muted" role="status">Loading…</p>{/if}
      {#if current.historyReadable && current.rows.length === 0 && !current.runsError && !current.conflict}<p class="muted">No runs match.</p>{/if}
      <div class="runs"><OperationRunTable rows={current.rows} compact onSelect={selectRun} /></div>
      {#if current.nextCursor}<Button size="sm" surface="soft" label={current.paging ? 'Loading…' : 'Show more'} disabled={current.paging} onclick={() => void controller.loadMore()} />{/if}
    {/if}
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
