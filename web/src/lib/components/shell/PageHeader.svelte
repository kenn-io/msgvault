<script lang="ts">
  import type { Snippet } from 'svelte';

  interface Props {
    title: string;
    description?: string;
    actions?: Snippet;
    view?: Snippet;
    visuallyHiddenTitle?: boolean;
  }

  let { title, description, actions, view, visuallyHiddenTitle = false }: Props = $props();
</script>

<header class="page-header">
  <div class="page-header__row">
    <div class="page-header__text" class:kit-sr-only={visuallyHiddenTitle}>
      <h1>{title}</h1>
      {#if description}<p>{description}</p>{/if}
    </div>
    {#if actions}<div class="page-header__actions">{@render actions()}</div>{/if}
  </div>
  {#if view}<div class="page-header__view">{@render view()}</div>{/if}
</header>

<style>
  .page-header {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .page-header__row {
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
</style>
