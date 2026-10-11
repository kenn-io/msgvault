<script lang="ts">
  import type { Snippet } from 'svelte';

  interface Props {
    title: string;
    description?: string;
    descriptionContent?: Snippet;
    actions?: Snippet;
    view?: Snippet;
  }

  let { title, description, descriptionContent, actions, view }: Props = $props();
</script>

<header class="page-header">
  <div class="page-header__row">
    <div class="page-header__text">
      <h1>{title}</h1>
      {#if descriptionContent}<p>{@render descriptionContent()}</p>{:else if description}<p>{description}</p>{/if}
    </div>
    {#if actions}<div class="page-header__actions">{@render actions()}</div>{/if}
  </div>
  {#if view}<div class="page-header__view">{@render view()}</div>{/if}
</header>

<style>
  .page-header {
    position: relative;
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    margin-bottom: var(--space-5);
  }

  .page-header::before {
    position: absolute;
    z-index: 0;
    top: calc(-1 * var(--space-5));
    right: calc(-1 * var(--page-gutter));
    bottom: calc(-1 * var(--space-6));
    left: calc(-1 * var(--page-gutter));
    border-bottom: 1px solid var(--border-muted);
    background: var(--bg-surface);
    content: '';
    pointer-events: none;
  }

  .page-header__row {
    position: relative;
    z-index: 1;
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3) var(--space-6);
  }

  .page-header__text {
    min-width: 0;
  }

  h1 {
    margin: 0;
    color: var(--text-primary);
    font-size: var(--font-size-xl);
    font-weight: 650;
    line-height: 1.25;
  }

  p {
    margin: var(--space-1) 0 0;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .page-header__actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }

  .page-header__view {
    position: relative;
    z-index: 1;
  }
</style>
