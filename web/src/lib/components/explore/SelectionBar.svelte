<script lang="ts">
  import {
    autoReposition, Button, dismissable, floatingPopoverStyle,
    Tooltip,
  } from '@kenn-io/kit-ui';
  import type { Snippet } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type {
    ExplorePreflightResponse as GeneratedExplorePreflightResponse,
    ExploreSelection as GeneratedExploreSelection,
  } from '../../api/generated/models';
  import type { AllMatchingExploreSelection } from '../../explore/models';
  import { preflightReasonLabel } from '../../explore/labels';
  import type { ExploreSelectionState } from '../../explore/state.svelte';
  import { formatBytes } from '../../util/format';
  import MeetingContextExport from '../meetings/MeetingContextExport.svelte';

  type Preflight = GeneratedExplorePreflightResponse;

  let {
    selection,
    totalCount,
    allMatching = undefined,
    preflight = undefined,
    client = undefined,
    meetingSelection = undefined,
    canExportMeetings = false,
    onExport = undefined,
    onReviewDeletion = undefined,
    onClear = undefined,
  }: {
    selection: ExploreSelectionState;
    totalCount?: number;
    allMatching?: AllMatchingExploreSelection;
    preflight?: Preflight;
    client?: APIClient;
    meetingSelection?: GeneratedExploreSelection;
    canExportMeetings?: boolean;
    onExport?: () => void;
    onReviewDeletion?: (mode: 'explicit' | 'all_matching') => void;
    /** Runs after Clear selection, which removes the bar and its focused button. */
    onClear?: () => void;
  } = $props();

  const exportReason = $derived(preflight?.unavailable_actions.find((item) => item.action === 'export')?.reason);
  const stageReason = $derived(
    preflight?.unavailable_actions.find((item) => item.action === 'stage_deletion')?.reason,
  );
  const visible = $derived(selection.mode === 'all_matching' || selection.count > 0);
  const exportTarget = $derived(preflight?.action_targets?.find((item) => item.action === 'export'));
  const contextSelectionCount = $derived(
    meetingSelection?.mode === 'explicit'
      ? (meetingSelection.row_keys?.length ?? 0)
      : totalCount === undefined
        ? undefined
        : Math.max(0, totalCount - (meetingSelection?.exclusions?.length ?? 0)),
  );
  const contextDisabledReason = $derived(
    contextSelectionCount !== undefined && contextSelectionCount > 100
      ? 'Meeting context accepts at most 100 meetings.'
      : '',
  );

  const meetingEligible = $derived(Boolean(client && meetingSelection && canExportMeetings));
  let meetingOpen = $state(false);
  let meetingTrigger = $state<HTMLSpanElement>();
  let meetingPanel = $state<HTMLDivElement>();
  let meetingStyle = $state('');

  function position(): void {
    if (!meetingTrigger || !meetingPanel) return;
    meetingStyle = floatingPopoverStyle({
      trigger: meetingTrigger.getBoundingClientRect(),
      viewportWidth: window.innerWidth,
      viewportHeight: window.innerHeight,
      popoverWidth: meetingPanel.offsetWidth,
      popoverHeight: meetingPanel.offsetHeight,
      align: 'start',
    });
  }

  function hideOnFocusOutside(event: FocusEvent): void {
    const target = event.relatedTarget;
    if (target instanceof Node && !meetingTrigger?.contains(target) && !meetingPanel?.contains(target)) {
      meetingOpen = false;
    }
  }

  $effect(() => {
    if (!meetingOpen) return;
    position();
    const stopPositioning = autoReposition(() => [meetingPanel, meetingTrigger], position);
    const stopDismissal = dismissable({
      owners: () => [meetingPanel, meetingTrigger],
      dismiss: () => (meetingOpen = false),
      escapeFocus: () => meetingTrigger?.querySelector('button'),
    });
    return () => {
      stopPositioning();
      stopDismissal();
    };
  });
  $effect(() => {
    if (!visible || !meetingEligible) meetingOpen = false;
  });
  const message = $derived.by(() => {
    if (selection.mode === 'all_matching') {
      const total = totalCount === undefined ? 'matching' : totalCount.toLocaleString();
      const except = selection.exclusions.size;
      return `All ${total} matching items selected${except > 0 ? `, except ${except}` : ''}`;
    }
    return `${selection.count.toLocaleString()} selected`;
  });
</script>

{#snippet gated(reason: string | undefined, control: Snippet)}
  {#if reason}
    <Tooltip focusable text={reason}>{@render control()}</Tooltip>
  {:else}
    {@render control()}
  {/if}
{/snippet}

<!-- The live region outlives the bar so the first "1 selected" is announced. -->
<span class="kit-sr-only" role="status" aria-label="Selection status" aria-live="polite"
  >{visible ? message : ''}</span
>
{#if visible}
  <div class="selection-bar">
    <span class="selection-count">{message.replace(' matching items', '')}{#if preflight}<span class="selection-size">{' · '}{formatBytes(preflight.estimated_bytes)}</span>{/if}</span>
    {#if allMatching && selection.mode === 'explicit' && selection.count > 0}
      <Button
        size="sm"
        tone="info"
        surface="soft"
        label={`Select all ${totalCount?.toLocaleString() ?? ''} matching`.replace(
          'all  matching',
          'all matching',
        )}
        onclick={() => selection.selectAllMatching(allMatching)}
      />
    {/if}
    {#if preflight && onExport && (exportTarget || exportReason)}
      {#snippet exportControl()}
        <Button size="sm" tone="info" surface="soft" label="Export" ariaLabel="Export selection" disabled={Boolean(exportReason)} onclick={onExport} />
      {/snippet}
      {@render gated(exportReason ? preflightReasonLabel('export', exportReason) : undefined, exportControl)}
    {/if}
    {#if meetingEligible && client && meetingSelection}
      <span bind:this={meetingTrigger} onfocusout={hideOnFocusOutside}>
        <Button
          size="sm"
          surface="soft"
          label="Meeting context…"
          ariaExpanded={meetingOpen}
          onclick={() => (meetingOpen = !meetingOpen)}
        />
      </span>
      <div class="meeting-panel kit-popover-card" role="dialog" aria-label="Meeting context" bind:this={meetingPanel} style={meetingStyle} hidden={!meetingOpen} onfocusout={hideOnFocusOutside}>
        <MeetingContextExport {client} request={{ selection: meetingSelection }} open={meetingOpen} disabledReason={contextDisabledReason} />
      </div>
    {/if}
    {#if onReviewDeletion}
      {#snippet deletionControl()}
        <Button
          size="sm"
          surface="soft"
          label="Review deletion…"
          disabled={Boolean(stageReason)}
          onclick={() => onReviewDeletion?.(selection.mode === 'all_matching' ? 'all_matching' : 'explicit')}
        />
      {/snippet}
      {@render gated(stageReason ? preflightReasonLabel('stage_deletion', stageReason) : undefined, deletionControl)}
    {/if}
    <Button
      size="sm"
      surface="soft"
      label="Clear"
      ariaLabel="Clear selection"
      onclick={() => {
        selection.clear();
        onClear?.();
      }}
    />
  </div>
{/if}

<style>
  .selection-bar {
    display: flex;
    flex: 0 0 auto;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--control-gap) var(--field-gap);
    padding: var(--pane-pad-y) var(--pane-pad-x);
    border-top: 1px solid var(--border-default);
    background: var(--bg-surface);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .selection-count {
    flex: 1 1 8rem;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-weight: 600;
  }

  .selection-size {
    font-weight: 400;
    color: var(--text-muted);
  }

  .meeting-panel {
    position: fixed;
    z-index: var(--z-popover);
    width: 320px;
    max-width: calc(100vw - 2 * var(--space-4));
    padding: var(--pane-pad-x);
  }
</style>
