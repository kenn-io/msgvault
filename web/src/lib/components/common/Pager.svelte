<script lang="ts">
  import ChevronLeft from '@lucide/svelte/icons/chevron-left';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import { IconButton } from '@kenn-io/kit-ui';
  let { offset, count, hasNext, onprevious, onnext, disabled = false, label = 'Pages' }: {
    offset: number; count: number; hasNext: boolean; onprevious: () => void; onnext: () => void;
    disabled?: boolean; label?: string;
  } = $props();
</script>

{#if offset > 0 || hasNext}
  <nav aria-label={label}>
    {#if count}<span>{offset + 1}–{offset + count}</span>{/if}
    <IconButton ariaLabel="Previous page" disabled={disabled || offset === 0} onclick={onprevious}><ChevronLeft size={14} aria-hidden="true" /></IconButton>
    <IconButton ariaLabel="Next page" disabled={disabled || !hasNext} onclick={onnext}><ChevronRight size={14} aria-hidden="true" /></IconButton>
  </nav>
{/if}

<style>
  /* 16 below the last row, at the right: the count (Small), then the 28 page arrows, 8 apart. */
  nav { display: flex; justify-content: flex-end; align-items: center; gap: var(--space-group); margin-top: var(--space-pane); }
  span { color: var(--text-muted); font-size: var(--type-small-size); line-height: var(--type-small-line); }
</style>
