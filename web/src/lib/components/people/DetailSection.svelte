<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import type { Snippet } from 'svelte';
  import { Disclosure } from '@kenn-io/kit-ui';
  import EmptyLine from '../common/EmptyLine.svelte';
  let { title, children, addLabel, onadd,
    history, historyLabel, subgroup, subgroupTitle, setupMessage, onsetup, control }: {
    title: string; children?: Snippet; addLabel?: string; onadd?: () => void;
    history?: Snippet; historyLabel?: string;
    subgroup?: Snippet; subgroupTitle?: string; setupMessage?: string; onsetup?: () => void;
    /** The header's one 28 control when it isn't an add button: Network's depth, Tasks' Set up…. */
    control?: Snippet;
  } = $props();
</script>

<section aria-label={title}>
  <header><h3>{title}</h3>{#if !setupMessage && control}{@render control()}{:else if !setupMessage && addLabel && onadd}<Button label={addLabel} onclick={onadd} />{/if}</header>
  <div class="section-body">
    {#if setupMessage}<EmptyLine message={setupMessage} {onsetup} />
    {:else}
      {@render children?.()}
      {#if history && historyLabel}<Disclosure variant="plain" class="fold" title={historyLabel}>{@render history()}</Disclosure>{/if}
      {#if subgroup}<div class="subgroup">{#if subgroupTitle}<h4>{subgroupTitle}</h4>{/if}{@render subgroup()}</div>{/if}
    {/if}
  </div>
</section>
<style>
  /* L2: a 28 header in Heading type with its one 28 control at the right, then the body 8 below. The body's
   * rows carry their own padding, so its children stack with no gap; a group heading sits 24 below them. */
  section { min-width: 0; }
  header { display: flex; align-items: center; justify-content: space-between; gap: var(--space-group); min-height: var(--control-h); }
  h3 { margin: 0; font-size: var(--type-heading-size); line-height: var(--type-heading-line); }
  .section-body { display: grid; min-width: 0; margin-top: var(--space-group); }
  .subgroup { display: grid; margin-top: var(--space-section); }
  h4 { margin: 0 0 var(--space-group); color: var(--text-muted); font-size: var(--type-heading-size); font-weight: 500; line-height: var(--type-heading-line); }
</style>
