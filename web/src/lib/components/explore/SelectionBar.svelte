<script lang="ts">
  import { Button, KbdBadge } from '@kenn-io/kit-ui';

  import type { components } from '../../api/generated/schema';
  import type { AllMatchingExploreSelection } from '../../explore/models';
  import type { ExploreSelectionState } from '../../explore/state.svelte';

  type Preflight = components['schemas']['ExplorePreflightResponse'];

  let {
    selection,
    totalCount,
    allMatching = undefined,
    preflight = undefined,
    onExport = undefined,
    onOpenInSource = undefined
  }: {
    selection: ExploreSelectionState;
    totalCount?: number;
    allMatching?: AllMatchingExploreSelection;
    preflight?: Preflight;
    onExport?: () => void;
    onOpenInSource?: () => void;
  } = $props();

  const exportReason = $derived(preflight?.unavailable_actions.find((item) => item.action === 'export')?.reason);
  const openReason = $derived(preflight?.unavailable_actions.find((item) => item.action === 'open_in_source')?.reason);
  const exportTarget = $derived(preflight?.action_targets?.find((item) => item.action === 'export'));

  const message = $derived.by(() => {
    if (selection.mode === 'all_matching') {
      const total = totalCount === undefined ? 'matching' : totalCount.toLocaleString();
      const except = selection.exclusions.size;
      return `All ${total} matching items selected${except > 0 ? `, except ${except}` : ''}`;
    }
    if (selection.count === 0) return 'No items selected';
    return `${selection.count.toLocaleString()} selected`;
  });
</script>

<div class="selection-bar" class:selection-bar--active={selection.mode === 'all_matching' || selection.count > 0}>
  <span role="status" aria-live="polite">{message}</span>
  <span class="shortcut"><KbdBadge keys={['Space']} /> toggle</span>
  <span class="shortcut"><KbdBadge keys={['A']} /> visible</span>
  {#if allMatching && selection.mode === 'explicit' && selection.count > 0}
    <Button
      size="sm"
      tone="info"
      surface="soft"
      label={`Select all ${totalCount?.toLocaleString() ?? ''} matching items`.replace('all  matching', 'all matching')}
      onclick={() => selection.selectAllMatching(allMatching)}
    />
  {/if}
  {#if preflight && !exportReason && exportTarget && onExport}
    <Button size="sm" tone="info" surface="soft" label="Export selection" onclick={onExport} />
  {:else if exportReason}
    <span class="action-reason">Export: {exportReason}</span>
  {/if}
  {#if preflight && !openReason && onOpenInSource}
    <Button size="sm" tone="info" surface="soft" label="Open selection in source" onclick={onOpenInSource} />
  {:else if openReason}
    <span class="action-reason">Open in source: {openReason}</span>
  {/if}
  <Button
    size="sm"
    surface="soft"
    label="Clear selection"
    disabled={selection.mode === 'explicit' && selection.count === 0}
    onclick={() => selection.clear()}
  />
</div>

<style>
  .selection-bar {
    display: flex;
    min-height: 32px;
    align-items: center;
    gap: var(--space-5);
    padding: 0 var(--space-4);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .selection-bar--active {
    border-color: color-mix(in srgb, var(--accent-teal) 45%, var(--border-default));
    background: color-mix(in srgb, var(--accent-teal) 8%, var(--bg-surface));
    color: var(--text-primary);
  }

  [role='status'] {
    margin-right: auto;
    font-weight: 600;
  }

  .shortcut {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    white-space: nowrap;
  }

  .action-reason {
    max-width: 18rem;
    color: var(--text-muted);
    overflow-wrap: anywhere;
  }

  @media (max-width: 760px) {
    .shortcut {
      display: none;
    }
  }
</style>
