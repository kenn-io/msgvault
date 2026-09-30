<script lang="ts">
  import { Button, Menu, MenuContent, MenuItem, MenuTrigger } from '@kenn-io/kit-ui';
  import Ellipsis from '@lucide/svelte/icons/ellipsis';

  import type { APIClient } from '../../api/client';
  import type {
    ExplorePreflightResponse as GeneratedExplorePreflightResponse,
    ExploreSelection as GeneratedExploreSelection,
  } from '../../api/generated/models';
  import type { AllMatchingExploreSelection } from '../../explore/models';
  import { preflightReasonLabel } from '../../explore/labels';
  import type { ExploreSelectionState } from '../../explore/state.svelte';
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
    onOpenInSource = undefined,
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
    onOpenInSource?: () => void;
    onReviewDeletion?: (mode: 'explicit' | 'all_matching') => void;
    /** Runs after Clear selection, which removes the bar and its focused button. */
    onClear?: () => void;
  } = $props();

  const exportReason = $derived(preflight?.unavailable_actions.find((item) => item.action === 'export')?.reason);
  const openReason = $derived(preflight?.unavailable_actions.find((item) => item.action === 'open_in_source')?.reason);
  const visible = $derived(selection.mode === 'all_matching' || selection.count > 0);
  const openMenuVisible = $derived(Boolean(openReason) || Boolean(preflight && onOpenInSource));
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

  let meetingOpen = $state(false);
  $effect(() => {
    if (!visible) meetingOpen = false;
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

<!-- The live region outlives the bar so the first "1 selected" is announced. -->
<span class="kit-sr-only" role="status" aria-label="Selection status" aria-live="polite"
  >{visible ? message : ''}</span
>
{#if visible}
  <div class="selection-bar">
    <span class="selection-count" aria-hidden="true">{message}</span>
    {#if allMatching && selection.mode === 'explicit' && selection.count > 0}
      <Button
        size="sm"
        tone="info"
        surface="soft"
        label={`Select all ${totalCount?.toLocaleString() ?? ''} matching items`.replace(
          'all  matching',
          'all matching',
        )}
        onclick={() => selection.selectAllMatching(allMatching)}
      />
    {/if}
    {#if preflight && !exportReason && exportTarget && onExport}
      <Button size="sm" tone="info" surface="soft" label="Export selection" onclick={onExport} />
    {:else if exportReason}
      <span class="action-reason" title={exportReason}>
        Export unavailable: {preflightReasonLabel('export', exportReason)}
      </span>
    {/if}
    {#if client && meetingSelection && canExportMeetings}
      <Button
        size="sm"
        surface="soft"
        label="Meeting context…"
        ariaExpanded={meetingOpen}
        onclick={() => (meetingOpen = !meetingOpen)}
      />
    {/if}
    {#if onReviewDeletion}
      <Button
        size="sm"
        surface="soft"
        label="Review for deletion…"
        onclick={() => onReviewDeletion(selection.mode === 'all_matching' ? 'all_matching' : 'explicit')}
      />
    {/if}
    {#if openMenuVisible}
      <Menu align="end">
        <MenuTrigger ariaLabel="More selection actions" title="More selection actions">
          <Ellipsis size={16} aria-hidden="true" />
        </MenuTrigger>
        <MenuContent ariaLabel="More selection actions">
          <MenuItem
            disabled={Boolean(openReason)}
            textValue="Open selection in source"
            onselect={() => onOpenInSource?.()}
          >
            <span class="menu-item-text">
              Open selection in source
              {#if openReason}
                <span class="menu-reason">{preflightReasonLabel('open_in_source', openReason)}</span>
              {/if}
            </span>
          </MenuItem>
        </MenuContent>
      </Menu>
    {/if}
    <Button
      size="sm"
      surface="soft"
      label="Clear selection"
      onclick={() => {
        selection.clear();
        onClear?.();
      }}
    />
    {#if client && meetingSelection && canExportMeetings && meetingOpen}
      <div class="meeting-row">
        <MeetingContextExport
          {client}
          request={{ selection: meetingSelection }}
          disabledReason={contextDisabledReason}
        />
      </div>
    {/if}
  </div>
{/if}

<style>
  .selection-bar {
    display: flex;
    min-height: 36px;
    flex: 0 0 auto;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-3) var(--space-4);
    padding: var(--space-1) var(--space-4);
    border-top: 1px solid var(--border-default);
    background: var(--bg-surface);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .selection-count {
    margin-right: auto;
    font-weight: 600;
  }

  .meeting-row {
    display: flex;
    flex-basis: 100%;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-3) var(--space-4);
  }

  .menu-item-text {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .menu-reason {
    max-width: 16rem;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    white-space: normal;
  }

  .action-reason {
    max-width: 18rem;
    color: var(--text-muted);
    overflow-wrap: anywhere;
  }
</style>
