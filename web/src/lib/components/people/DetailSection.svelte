<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import type { Snippet } from 'svelte';
  import { Disclosure } from '@kenn-io/kit-ui';
  let { title, children, addLabel, onadd,
    history, historyLabel, subgroup, subgroupTitle, control, notice, collapsible = false, open = $bindable(false), summary }: {
    collapsible?: boolean; open?: boolean; summary?: string;
    /** A collapsible section's line under its title, shown open or closed: Facts' Set up line. */
    notice?: Snippet;
    title: string; children?: Snippet; addLabel?: string; onadd?: () => void;
    history?: Snippet; historyLabel?: string;
    subgroup?: Snippet; subgroupTitle?: string;
    /** The header's one 28 control when it isn't an add button: Network's depth, Tasks' Set up…. */
    control?: Snippet;
  } = $props();
</script>

{#snippet body()}
  <div class="section-body">
    {@render notice?.()}
    {@render children?.()}
    {#if history && historyLabel}<Disclosure variant="plain" class="fold" title={historyLabel}>{@render history()}</Disclosure>{/if}
    {#if subgroup}<div class="subgroup">{#if subgroupTitle}<h4>{subgroupTitle}</h4>{/if}{@render subgroup()}</div>{/if}
  </div>
{/snippet}
<section aria-label={title}>
  {#if collapsible}
    <Disclosure variant="plain" class="section-fold" bind:open>
      {#snippet summary()}<h3>{title}</h3>{/snippet}
      {#snippet meta()}{summary}{/snippet}
      {#if open}{@render body()}{/if}
    </Disclosure>
    {#if notice && !open}<div class="section-body">{@render notice()}</div>{/if}
  {:else}
    <header><h3>{title}</h3>{#if control}{@render control()}{:else if addLabel && onadd}<Button label={addLabel} onclick={onadd} />{/if}</header>
    {@render body()}
  {/if}
</section>
<style>
  /* L2: a 28 header in Heading type with its one 28 control at the right, then the body 8 below. The body's
   * rows carry their own padding, so its children stack with no gap; a group heading sits 24 below them. */
  section { min-width: 0; }
  section :global(.section-fold > .kit-disclosure__summary) { justify-content: flex-start; min-height: var(--control-h); padding: 0; gap: var(--space-tight); }
  section :global(.section-fold > .kit-disclosure__summary > .kit-disclosure__chevron) { order: 1; }
  section :global(.section-fold > .kit-disclosure__summary > .kit-disclosure__meta) { order: 2; margin-left: var(--space-group); }

  section :global(.section-fold > .kit-disclosure__summary > .kit-disclosure__title) { flex: none; }
  section :global(.section-fold > .kit-disclosure__content) { padding: 0; }
  header { display: flex; align-items: center; justify-content: space-between; gap: var(--space-group); min-height: var(--control-h); }
  h3 { margin: 0; font-size: var(--type-heading-size); line-height: var(--type-heading-line); }
  .section-body { display: grid; min-width: 0; margin-top: var(--space-group); }
  .subgroup { display: grid; margin-top: var(--space-section); }
  h4 { margin: 0 0 var(--space-group); color: var(--text-muted); font-size: var(--type-heading-size); font-weight: 500; line-height: var(--type-heading-line); }
</style>
