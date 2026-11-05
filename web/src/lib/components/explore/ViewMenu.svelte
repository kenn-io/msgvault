<script lang="ts">
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import { appShortcuts, Menu, MenuContent, MenuItem, MenuRadioGroup, MenuRadioItem, MenuSeparator, MenuTrigger } from '@kenn-io/kit-ui';

  let { scoped, presentation, presentationOptions, groupOptions, sort, sortOptions, selectPresentation, selectGrouping, selectSort }: {
    scoped: boolean;
    presentation: string;
    presentationOptions: { value: string; label: string }[];
    groupOptions: { value: string; label: string; disabled?: boolean }[];
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
        {#each groupOptions as option (option.value)}
          <MenuItem disabled={option.disabled} onselect={() => selectGrouping(option.value)}>{option.label}</MenuItem>
        {/each}
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
  :global([data-section-label]) {
    padding: var(--space-2) var(--space-4) var(--space-1);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-semibold);
  }
</style>
