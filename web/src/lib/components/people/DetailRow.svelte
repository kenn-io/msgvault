<script lang="ts">
  import type { Snippet } from 'svelte';

  interface Props {
    label?: string;
    children: Snippet;
    /** Buttons for the row. They show on hover and keyboard focus. */
    actions?: Snippet;
  }

  let { label = undefined, children, actions = undefined }: Props = $props();
</script>

<div class="detail-row">
  {#if label !== undefined}<span class="label">{label}</span>{/if}
  <div class="value">{@render children()}</div>
  {#if actions}<div class="actions">{@render actions()}</div>{/if}
</div>

<style>
  .detail-row {
    display: flex;
    min-height: var(--control-h-lg);
    align-items: center;
    gap: var(--field-gap);
    padding: var(--space-2) 0;
    border-bottom: 1px solid var(--border-muted);
  }

  .label {
    width: 120px;
    flex: none;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .value {
    min-width: 0;
    flex: 1;
    overflow-wrap: anywhere;
    color: var(--text-primary);
    font-size: var(--font-size-md);
  }

  .actions {
    display: flex;
    flex: none;
    gap: var(--space-2);
    opacity: 0;
  }

  .detail-row:hover .actions,
  .detail-row:focus-within .actions {
    opacity: 1;
  }

  @media (hover: none) {
    .actions {
      opacity: 1;
    }
  }

  @media (max-width: 640px) {
    .label {
      width: 88px;
    }
  }
</style>
