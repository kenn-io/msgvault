<script lang="ts">
  import type { Snippet } from 'svelte';

  interface Props {
    label?: string;
    /** A muted second line under the value, such as dates or a role. */
    description?: string;
    children: Snippet;
    /** Buttons for the row. They show on hover and keyboard focus. */
    actions?: Snippet;
  }

  let { label = undefined, description = undefined, children, actions = undefined }: Props = $props();
</script>

<div class="detail-row">
  {#if label !== undefined}<span class="label">{label}</span>{/if}
  <div class="value">{@render children()}{#if description}<span class="description">{description}</span>{/if}</div>
  {#if actions}<div class="actions">{@render actions()}</div>{/if}
</div>

<style>
  .detail-row {
    display: flex;
    align-items: baseline;
    gap: var(--field-gap);
    padding: var(--space-3) 0;
    border-bottom: 1px solid var(--border-muted);
  }

  .label {
    width: var(--detail-label-width);
    flex: none;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .value {
    min-width: 0;
    line-height: 1.4;
    flex: 1;
    overflow-wrap: anywhere;
    color: var(--text-primary);
    font-size: var(--font-size-md);
  }

  .description {
    display: block;
    margin-top: var(--space-1);
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .actions {
    display: flex;
    flex: none;
    align-self: center;
    margin: calc(var(--space-1) * -1) 0;
    gap: var(--space-2);
    opacity: 0;
  }

  .detail-row:hover .actions,
  .detail-row:focus-within .actions {
    opacity: 1;
  }

  @media (max-width: 640px) {
    .detail-row:has(.actions) { flex-wrap: wrap; }
    .detail-row:has(.actions) .value { flex-basis: 100%; }
    .actions { flex-wrap: wrap; max-width: 100%; }
  }

  @media (hover: none) {
    .actions {
      opacity: 1;
    }
  }
</style>
