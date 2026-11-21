<script lang="ts">
  import { Button, Disclosure } from '@kenn-io/kit-ui';
  import type { Snippet } from 'svelte';
  let { title, summary, expanded = true, children, addLabel, onadd,
    history, historyLabel, subgroup, subgroupTitle, control }: {
    title: string; summary?: string; expanded?: boolean;
    children?: Snippet; addLabel?: string; onadd?: () => void;
    history?: Snippet; historyLabel?: string;
    subgroup?: Snippet; subgroupTitle?: string;
    /** Custom header controls in place of the default add or collapse button. */
    control?: Snippet;
  } = $props();
</script>

<section aria-label={title}>
  <header><h3>{title}</h3>{#if summary}<span class="summary">{summary}</span>{/if}{#if control}{@render control()}{:else if addLabel && onadd}<Button label={addLabel} onclick={onadd} />{/if}</header>
  {#if expanded}<div class="section-body">
    {@render children?.()}
    {#if history && historyLabel}<Disclosure variant="plain" class="fold" title={historyLabel}>{@render history()}</Disclosure>{/if}
    {#if subgroup}<div class="subgroup">{#if subgroupTitle}<h4>{subgroupTitle}</h4>{/if}{@render subgroup()}</div>{/if}
  </div>{/if}
</section>
<style>
  /* A 28 header sits above the body, with controls at the right. The body's rows carry their own padding,
   * so its children stack with no gap; a group heading sits 24 below them. */
  section { min-width: 0; }
  header { display: flex; align-items: center; justify-content: space-between; gap: var(--space-group); min-height: var(--control-h); }
  h3 { margin: 0; font-size: var(--type-heading-size); line-height: var(--type-heading-line); }
  .summary { color: var(--text-muted); font-size: var(--font-size-sm); }
  .section-body { display: grid; min-width: 0; margin-top: var(--space-group); }
  .subgroup { display: grid; margin-top: var(--space-section); }
  h4 { margin: 0 0 var(--space-group); color: var(--text-muted); font-size: var(--type-heading-size); font-weight: 500; line-height: var(--type-heading-line); }
</style>
