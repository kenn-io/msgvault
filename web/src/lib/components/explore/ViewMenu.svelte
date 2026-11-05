<script lang="ts">
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import { appShortcuts, Menu, MenuContent, MenuRadioGroup, MenuRadioItem, MenuSeparator, MenuTrigger } from '@kenn-io/kit-ui';

  let { scoped, presentation, presentationOptions, groupOptions, groupingChain, sort, sortOptions, selectPresentation, selectGrouping, selectSort }: {
    scoped: boolean;
    presentation: string;
    presentationOptions: { value: string; label: string }[];
    groupOptions: { value: string; label: string; disabled?: boolean }[];
    groupingChain: string[];
    sort?: { value: string };
    sortOptions: { value: string; label: string; disabled?: boolean }[];
    selectPresentation: (value: string) => void;
    selectGrouping: (value: string) => void;
    selectSort: (value: string) => void;
  } = $props();

  let open = $state(false);
  $effect(() => {
    if (open) return appShortcuts.pushScope('view-menu');
  });
</script>

<div data-group-picker={scoped ? undefined : ''} data-sort-menu={sort ? '' : undefined}>
  <Menu align="start" bind:open>
    <MenuTrigger>View <ChevronDown size={12} aria-hidden="true" /></MenuTrigger>
    <MenuContent ariaLabel="View">
      {#if !scoped}
        <div data-section-label aria-hidden="true">Show as</div>
        <MenuRadioGroup ariaLabel="Show as" value={presentation} onchange={selectPresentation}>
          {#each presentationOptions as option (option.value)}
            <MenuRadioItem value={option.value} closeOnSelect>{option.label}</MenuRadioItem>
          {/each}
        </MenuRadioGroup>
        <MenuSeparator />
        <div data-section-label aria-hidden="true">Group by</div>
        <MenuRadioGroup ariaLabel="Group by" value={groupingChain.at(-1) ?? ''} onchange={(value) => { if (!groupingChain.includes(value)) selectGrouping(value); }}>
          {#each groupOptions as option (option.value)}
            <MenuRadioItem value={option.value} disabled={option.disabled || (groupingChain.includes(option.value) && groupingChain.at(-1) !== option.value)} closeOnSelect>{option.label}</MenuRadioItem>
          {/each}
        </MenuRadioGroup>
      {/if}
      {#if sort}
        {#if !scoped}<MenuSeparator />{/if}
        <div data-section-label aria-hidden="true">Sort</div>
        <MenuRadioGroup ariaLabel="Sort" value={sort.value} onchange={selectSort}>
          {#each sortOptions as option (option.value)}
            <MenuRadioItem value={option.value} disabled={option.disabled} closeOnSelect>{option.label}</MenuRadioItem>
          {/each}
        </MenuRadioGroup>
      {/if}
    </MenuContent>
  </Menu>
</div>

<style>
  div :global(.kit-menu__trigger) {
    padding: 5px 14px;
    font-family: inherit;
    font-size: var(--kit-control-font-size, var(--font-size-md));
    font-weight: var(--font-weight-medium, 500);
    line-height: normal;
  }

  div :global(.kit-menu__trigger[aria-expanded="true"]) {
    background: color-mix(in srgb, var(--text-muted) 8%, var(--bg-inset));
    border-color: color-mix(in srgb, var(--border-default) 80%, transparent);
    color: var(--text-secondary);
  }

  div :global(.kit-menu__trigger[aria-expanded="true"]:hover:not(:disabled)) {
    background: var(--bg-surface-hover);
    border-color: var(--border-default);
    color: var(--text-primary);
  }

  div[data-section-label] {
    padding: var(--space-2) var(--space-4) var(--space-1);
  }
</style>
