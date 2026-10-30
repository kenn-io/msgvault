<script lang="ts">
  import { DetailDrawer, IconButton } from '@kenn-io/kit-ui';
  import X from '@lucide/svelte/icons/x';
  import { onMount, type Snippet } from 'svelte';
  import { claimDialogEscape } from './dialog-escape';

  interface Props {
    title: string;
    /** Runs on Escape and the close button. The row that opened the panel calls it again to toggle. */
    onclose: () => void;
    children: Snippet;
    footer?: Snippet;
  }

  const id = $props.id();
  onMount(() => claimDialogEscape(`source-drawer:${id}`, onclose));

  let { title, onclose, children, footer = undefined }: Props = $props();
</script>

<!-- The scrim lets clicks through so the row that opened the panel can close it. -->
<div class="side-panel">
  <DetailDrawer ariaLabel={title} width="min(440px, 100vw)" closeOnOverlayClick={false} {onclose} {footer}>
    {#snippet header()}
      <span class="side-panel__title">{title}</span>
      <IconButton size="sm" ariaLabel="Close" title="Close (Esc)" onclick={onclose}>
        <X size={14} strokeWidth={2} aria-hidden="true" />
      </IconButton>
    {/snippet}
    {@render children()}
  </DetailDrawer>
</div>

<style>
  .side-panel :global(.kit-detail-drawer-overlay) {
    background: transparent;
    backdrop-filter: none;
    pointer-events: none;
  }

  .side-panel :global(.kit-detail-drawer) {
    pointer-events: auto;
  }

  .side-panel__title {
    min-width: 0;
    flex: 1;
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: 600;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
</style>
