<script lang="ts">
  import {
    Button,
    DateRangePicker,
    RefreshControl,
    SelectDropdown,
    resolveRange,
    type RangeSelection
  } from '@kenn-io/kit-ui';
  import { onMount, tick } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { OperationStatusAuthority } from '../../explore/models';
  import type { OperationsController } from '../../operations/controller.svelte';
  import {
    operationFocusAnchor,
    resolveOperationFocusAnchor,
    type OperationFocusAnchor
  } from '../../operations/focus';
  import type { OperationAction, OperationKind, OperationsURLState } from '../../operations/models';
  import {
    OPERATION_KIND_LABELS,
    OPERATION_LANE_LABELS,
    RELATED_STATUS_LABELS,
    type OperationSettingsTarget,
    type RelatedStatus
  } from '../../operations/labels';
  import OperationRelatedStatus from './OperationRelatedStatus.svelte';
  import OperationRunDetail from './OperationRunDetail.svelte';
  import OperationRunTable from './OperationRunTable.svelte';
  import OperationStatusList from './OperationStatusList.svelte';
  import PageHeader from '../shell/PageHeader.svelte';

  type Controller = Pick<
    OperationsController, 'snapshot' | 'refresh' | 'refreshStatus' | 'loadMore' | 'restart' | 'runAction'
  >;

  let {
    controller,
    client = undefined,
    state: urlState,
    onStateChange = () => undefined,
    onNavigate = () => undefined,
    onAnnounce = () => undefined,
    onConfigure = () => undefined,
    onSetUp = () => undefined
  }: {
    controller: Controller;
    client?: APIClient;
    state: OperationsURLState;
    onStateChange?: (patch: Partial<OperationsURLState>) => void;
    onNavigate?: (target: RelatedStatus) => void;
    onAnnounce?: (message: string) => void;
    onConfigure?: () => void;
    onSetUp?: (target: OperationSettingsTarget) => void;
  } = $props();

  let root = $state<HTMLElement>();
  let narrow = $state(false);
  let invokingRow = $state<HTMLButtonElement>();
  let invokingAnchor = $state<OperationFocusAnchor>();
  let statusFocus = $state<{ target: OperationStatusAuthority; ordinal: number; slot: number }>();
  let priorOperationStatus = $state<OperationsURLState['operationStatus']>('');
  const current = $derived(controller.snapshot);
  const dateSelection = $derived<RangeSelection>(dateSelectionFromState(urlState));
  const relatedStatusConfigured = $derived(configuredRelatedStatus(urlState.operationStatus));

  const laneOptions = [
    { value: '', label: 'All lanes' },
    ...Object.entries(OPERATION_LANE_LABELS).map(([value, label]) => ({ value, label }))
  ];
  const kindOptions = [
    { value: '', label: 'All kinds' },
    ...Object.entries(OPERATION_KIND_LABELS).map(([value, label]) => ({ value, label }))
  ];
  const stateOptions = [
    { value: '', label: 'All states' },
    { value: 'queued', label: 'Queued' },
    { value: 'running', label: 'Running' },
    { value: 'succeeded', label: 'Succeeded' },
    { value: 'partial', label: 'Partial' },
    { value: 'paused', label: 'Paused' },
    { value: 'failed', label: 'Failed' },
    { value: 'cancelled', label: 'Cancelled' }
  ];
  const actionSuccess: Record<OperationAction, string> = {
    carddav_sync: 'CardDAV sync request completed; current operation state was refreshed.',
    visual_build: 'Visual index build request completed; current operation state was refreshed.',
    visual_resume: 'Visual index resume request completed; current operation state was refreshed.'
  };

  onMount(() => {
    const mediaQuery = window.matchMedia?.('(max-width: 760px)');
    const update = () => { narrow = mediaQuery?.matches ?? false; };
    update();
    mediaQuery?.addEventListener('change', update);
    const keydown = (event: KeyboardEvent) => {
      if (event.key !== 'Escape' || urlState.operationRunID === null) return;
      const target = event.target;
      if (target instanceof Element && target.closest('input, textarea, select, [contenteditable]:not([contenteditable="false"])')) return;
      event.preventDefault();
      void closeDetail();
    };
    window.addEventListener('keydown', keydown);
    return () => {
      mediaQuery?.removeEventListener('change', update);
      window.removeEventListener('keydown', keydown);
    };
  });

  function patchFilter(patch: Partial<OperationsURLState>): void {
    onStateChange({ ...patch, operationRunID: null, operationStatus: '' });
  }

  function configuredRelatedStatus(authority: OperationsURLState['operationStatus']): boolean | undefined {
    const kindByAuthority: Partial<Record<OperationStatusAuthority, OperationKind>> = {
      getDocumentIndexStatus: 'document_extraction',
      getDocumentVectorStatus: 'document_embedding',
      getVisualAttachmentStatus: 'visual_embedding'
    };
    const kind = authority ? kindByAuthority[authority] : undefined;
    if (!kind) return undefined;
    for (const lane of current.statusLanes) {
      const status = lane.kinds.find((candidate) => candidate.kind === kind);
      if (status) return status.configured;
    }
    return undefined;
  }

  function relatedStatusButton(target: OperationStatusAuthority): string {
    return `Open ${RELATED_STATUS_LABELS[target]}`;
  }

  function navigateStatus(target: RelatedStatus, button: HTMLButtonElement): void {
    if (target === 'getDocumentIndexStatus' || target === 'getDocumentVectorStatus' ||
      target === 'getVisualAttachmentStatus') {
      const buttons = Array.from(root?.querySelectorAll<HTMLButtonElement>('button') ?? []);
      const targetButtons = buttons.filter((candidate) => candidate.ariaLabel === relatedStatusButton(target));
      statusFocus = {
        target,
        ordinal: Math.max(0, targetButtons.indexOf(button)),
        slot: Math.max(0, buttons.indexOf(button))
      };
    }
    onNavigate(target);
  }

  async function restoreStatusFocus(): Promise<void> {
    const focus = statusFocus;
    if (!focus) return;
    await tick();
    await new Promise<void>((resolve) => setTimeout(resolve, 0));
    const buttons = Array.from(root?.querySelectorAll<HTMLButtonElement>('button') ?? []);
    const targetButtons = buttons.filter((button) => button.ariaLabel === relatedStatusButton(focus.target));
    (targetButtons[focus.ordinal] ?? buttons[focus.slot])?.focus();
    statusFocus = undefined;
  }

  $effect(() => {
    const next = urlState.operationStatus;
    if (priorOperationStatus && !next) void restoreStatusFocus();
    priorOperationStatus = next;
  });

  function selectRun(id: string, button: HTMLButtonElement): void {
    invokingRow = button;
    invokingAnchor = operationFocusAnchor(current.rows, id);
    onStateChange({ operationRunID: id });
  }

  async function closeDetail(): Promise<void> {
    const selectedID = resolveOperationFocusAnchor(current.rows, invokingAnchor) ?? urlState.operationRunID;
    onStateChange({ operationRunID: null });
    await tick();
    await new Promise<void>((resolve) => setTimeout(resolve, 0));
    const fallback = selectedID
      ? Array.from(root?.querySelectorAll<HTMLButtonElement>('button[data-run-id]') ?? [])
          .find((button) => button.dataset.runId === selectedID)
      : undefined;
    (invokingRow?.isConnected ? invokingRow : fallback)?.focus();
    invokingRow = undefined;
    invokingAnchor = undefined;
  }

  function changeDates(selection: RangeSelection): void {
    const range = selection.mode === 'custom' ? selection : resolveRange(selection);
    patchFilter({
      operationStartedFrom: range.from ? `${range.from}T00:00:00Z` : '',
      operationStartedBefore: range.to ? nextDayUTC(range.to) : ''
    });
  }

  async function refreshStatus(): Promise<void> {
    const control = root?.querySelector<HTMLElement>('.kit-refresh-control');
    const hadFocus = Boolean(control?.contains(document.activeElement));
    await controller.refreshStatus();
    // Kit disables the button while busy, and a disabled button drops focus.
    // Restore it only if the person has not moved focus elsewhere meanwhile.
    if (!hadFocus) return;
    await tick();
    const active = document.activeElement;
    if (active && active !== document.body && !control?.contains(active)) return;
    control?.querySelector<HTMLButtonElement>('button')?.focus();
  }

  async function runAction(action: OperationAction): Promise<void> {
    const outcome = await controller.runAction(action);
    if (outcome === 'discarded') return;
    const after = controller.snapshot;
    if (outcome === 'succeeded') onAnnounce(actionSuccess[action]);
    else if (after.actionConflict) onAnnounce(after.actionConflict);
    else if (after.actionError) onAnnounce(after.actionError);
  }

  function dateSelectionFromState(value: OperationsURLState): RangeSelection {
    const from = value.operationStartedFrom.slice(0, 10);
    const before = value.operationStartedBefore.slice(0, 10);
    return {
      mode: 'custom',
      from,
      to: before ? previousDayUTC(before) : ''
    };
  }

  function nextDayUTC(date: string): string {
    const value = new Date(`${date}T00:00:00Z`);
    value.setUTCDate(value.getUTCDate() + 1);
    return value.toISOString().replace('.000Z', 'Z');
  }

  function previousDayUTC(date: string): string {
    const value = new Date(`${date}T00:00:00Z`);
    value.setUTCDate(value.getUTCDate() - 1);
    return value.toISOString().slice(0, 10);
  }
</script>

{#snippet operationNotices()}
  {#if current.initialLoading}
    <p class="notice" role="status" aria-label="Operations loading">Loading operations…</p>
  {/if}
  {#if current.backgroundLoading}
    <p class="notice" role="status" aria-label="Operations refresh">Refreshing operations in the background…</p>
  {/if}
  {#if current.statusError}<p class="notice notice--error" role="alert" aria-label="Operation status failure">{current.statusError}</p>{/if}
  {#if current.runsError}<p class="notice notice--error" role="alert" aria-label="Operation history failure">{current.runsError}</p>{/if}
  {#if current.unavailableKinds.length > 0}
    <div class="notice" role="status" aria-label="Unavailable operation history">
      {#each current.unavailableKinds as unavailable (unavailable.kind)}
        <span>{OPERATION_KIND_LABELS[unavailable.kind]} history is unavailable.</span>
      {/each}
    </div>
  {/if}
  {#if current.conflict}
    <div class="notice notice--error" role="alert" aria-label="Operation history conflict">
      <span>{current.conflict}</span>
      {#if current.restartRequired}<Button size="sm" label="Restart operation history" onclick={() => void controller.restart()} />{/if}
    </div>
  {/if}
  {#if current.actionPending}
    <p class="notice" role="status" aria-label="Operation action progress">Starting the advertised operation…</p>
  {/if}
  {#if current.actionConflict}<p class="notice notice--error" role="alert" aria-label="Operation action conflict">{current.actionConflict}</p>{/if}
  {#if current.actionError}<p class="notice notice--error" role="alert" aria-label="Operation action failure">{current.actionError}</p>{/if}
{/snippet}

<main class="operations-workspace" bind:this={root} aria-label="Operations">
  {#if urlState.operationStatus && client}
    {#key urlState.operationStatus}
      <OperationRelatedStatus
        {client}
        authority={urlState.operationStatus}
        configured={relatedStatusConfigured}
        onClose={() => onStateChange({ operationStatus: '' })}
        {onConfigure}
      />
    {/key}
  {:else if narrow && urlState.operationRunID !== null}
    <section class="focused-detail" aria-label="Operation detail focused content">
      <PageHeader title="Operation detail">
        {#snippet actions()}
          <Button size="sm" surface="soft" label="Back to operation history" onclick={() => void closeDetail()} />
        {/snippet}
      </PageHeader>
      {@render operationNotices()}
      {#if current.detailLoading}
        <p role="status" aria-label="Operation detail loading">Loading operation detail…</p>
      {:else if current.detailError}
        <p class="notice notice--error" role="alert" aria-label="Operation detail failure">{current.detailError}</p>
      {:else if current.detail}
        <OperationRunDetail
          detail={current.detail}
          actionPending={current.actionPending}
          showClose={false}
          onClose={() => void closeDetail()}
          onNavigate={navigateStatus}
          onAction={(action) => void runAction(action)}
        />
      {/if}
    </section>
  {:else}
    <PageHeader title="Operations" description="Background work and its history.">
      {#snippet actions()}
        <RefreshControl
          label="Refresh operation status"
          lastUpdatedAt={current.statusUpdatedAt}
          busy={current.statusRefreshing}
          onRefresh={() => void refreshStatus()}
        />
      {/snippet}
    </PageHeader>

    {#if current.statusReadable}
      <OperationStatusList
        lanes={current.statusLanes}
        actionPending={current.actionPending}
        onNavigate={navigateStatus}
        onAction={(action) => void runAction(action)}
        {onSetUp}
      />
    {/if}

    <section class="filters" aria-label="Operation filters">
      <SelectDropdown title="Lane" value={urlState.operationLane} options={laneOptions}
        onchange={(operationLane) => patchFilter({ operationLane: operationLane as OperationsURLState['operationLane'] })} />
      <SelectDropdown title="Kind" value={urlState.operationKind} options={kindOptions}
        onchange={(operationKind) => patchFilter({ operationKind: operationKind as OperationsURLState['operationKind'] })} />
      <SelectDropdown title="State" value={urlState.operationState} options={stateOptions}
        onchange={(operationState) => patchFilter({ operationState: operationState as OperationsURLState['operationState'] })} />
      <DateRangePicker selection={dateSelection} onSelect={changeDates} maxDate={new Date().toISOString().slice(0, 10)} />
      {#if urlState.operationStartedFrom || urlState.operationStartedBefore}
        <Button size="sm" surface="soft" label="Clear operation dates" onclick={() => patchFilter({ operationStartedFrom: '', operationStartedBefore: '' })} />
      {/if}
      <Button size="sm" surface="soft" label="Reload run history" disabled={current.backgroundLoading} onclick={() => void controller.refresh()} />
    </section>

    {@render operationNotices()}

    {#if current.historyReadable && current.rows.length === 0 && current.unavailableKinds.length === 0 && !current.runsError && !current.conflict}
      <p class="notice" role="status" aria-label="Operation history state">No operation runs match the current filters.</p>
    {/if}

    <div class="content" class:has-detail={urlState.operationRunID !== null}>
      <section class="history" aria-label="Operation run list">
        {#if current.rows.length > 0}
          <OperationRunTable
            rows={current.rows}
            selectedID={urlState.operationRunID}
            {narrow}
            onSelect={selectRun}
          />
        {/if}
        {#if current.nextCursor}
          <Button label={current.paging ? 'Loading more operation history…' : 'Load more operation history'} disabled={current.paging} onclick={() => void controller.loadMore()} />
        {/if}
      </section>
      {#if urlState.operationRunID !== null && !narrow}
        <aside class="detail-pane" aria-label="Selected operation detail">
          {#if current.detailLoading}
            <p role="status" aria-label="Operation detail loading">Loading operation detail…</p>
          {:else if current.detailError}
            <p class="notice notice--error" role="alert">{current.detailError}</p>
          {:else if current.detail}
            <OperationRunDetail
              detail={current.detail}
              actionPending={current.actionPending}
              onClose={() => void closeDetail()}
              onNavigate={navigateStatus}
              onAction={(action) => void runAction(action)}
            />
          {/if}
        </aside>
      {/if}
    </div>
  {/if}
</main>

<style>
  .operations-workspace { display: grid; align-content: start; gap: var(--space-4); min-width: 0; min-height: 0; padding: var(--space-5) var(--page-gutter) var(--space-4); overflow: auto; }
  .notice { margin: 0; }
  .filters { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-2); }
  .content { display: grid; min-width: 0; min-height: 0; }
  .content.has-detail { grid-template-columns: minmax(520px, 1fr) minmax(340px, .6fr); gap: var(--space-4); }
  .history { display: grid; align-content: start; gap: var(--space-3); min-width: 0; }
  .detail-pane { min-width: 0; border-left: 1px solid var(--border-default); background: var(--bg-surface); }
  .notice { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: var(--space-2); padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); }
  .notice--error { border-color: var(--status-error-ink); background: var(--status-error-bg); color: var(--status-error-ink); }
  .focused-detail { min-height: 0; background: var(--bg-surface); }

  @media (max-width: 760px) {
    .filters { align-items: stretch; flex-direction: column; }
    .filters :global(.kit-select-dropdown), .filters :global(.kit-date-range) { width: 100%; }
  }
</style>
