<script lang="ts">
  import type { Snippet } from 'svelte';
  import { MediaQuery } from 'svelte/reactivity';
  import XIcon from '@lucide/svelte/icons/x';
  import { Button, Chip, IconButton, MEDIA, SelectDropdown } from '@kenn-io/kit-ui';

  import type { APIClient } from '../../api/client';
  import type { ExploreFilter, ExploreGroupDimension, ExploreSearchMode, ExploreURLState } from '../../explore/models';
  import { filterDimensionLabel, filterValueLabel, groupedByLabel, searchModeLabel } from '../../explore/labels';
  import { groupingOptions, isGroupingDimension } from '../../grouping/catalog';
  import AccountFilter from './AccountFilter.svelte';
  import IdentityFilter from './IdentityFilter.svelte';
  import FilterMenu from './FilterMenu.svelte';
  import ViewMenu from './ViewMenu.svelte';

  interface ExtraChip {
    key: string;
    label: string;
    removeLabel: string;
    onRemove: () => void;
  }

  interface SortConfig {
    options: { value: string; label: string }[];
    value: string;
    note?: string;
    onchange?: (value: string) => void;
  }

  let {
    client,
    messages = false,
    lockedFilter = undefined,
    filterExtra = undefined,
    extraControls = undefined,
    extraActive = false,
    query,
    searchMode,
    filters,
    groupingChain,
    countLabel,
    sort = undefined,
    presentation = 'table',
    extraChips = [],
    onAddGroup,
    onRemoveGroup,
    onClearFilters,
    onClearExtra = undefined,
    onFiltersChange,
    onRemoveQuery = undefined,
    onRemoveFilter = undefined,
    onPresentationChange = undefined
  }: {
    client: APIClient;
    messages?: boolean;
    lockedFilter?: ExploreFilter;
    filterExtra?: Snippet;
    extraControls?: Snippet;
    extraActive?: boolean;
    query: string;
    searchMode: ExploreSearchMode;
    filters: ExploreFilter[];
    groupingChain: ExploreGroupDimension[];
    countLabel: string;
    sort?: SortConfig;
    presentation?: ExploreURLState['presentation'];
    extraChips?: ExtraChip[];
    onAddGroup: (dimension: ExploreGroupDimension) => void;
    onRemoveGroup: (index: number) => void;
    onClearFilters: () => void;
    onClearExtra?: () => void;
    onFiltersChange: (filters: ExploreFilter[]) => void;
    onRemoveQuery?: () => void;
    onRemoveFilter?: (index: number) => void;
    onPresentationChange?: (presentation: ExploreURLState['presentation']) => void;
  } = $props();

  let filtersOpen = $state(false);
  const compact = new MediaQuery(MEDIA.compact);
  const extraInPopover = $derived(compact.current && Boolean(extraControls));
  const groupOptions = $derived(groupingOptions({ excluded: groupingChain, includeUnavailable: true }));
  const canGroup = $derived(groupOptions.some((option) => !option.disabled));
  // The triggers carry their own names (no kit `title`), so the visible text says what each control is.
  const options = $derived([
    {
      value: '',
      label: groupingChain.length > 0 ? 'Add grouping' : 'None',
      triggerLabel: groupingChain.length > 0 ? 'Add grouping' : 'Group by',
      disabled: true
    },
    ...groupOptions
  ]);
  const presentationOptions = [
    { value: 'table', label: 'Table', triggerLabel: 'Show as: Table' },
    { value: 'timeline', label: 'Timeline', triggerLabel: 'Show as: Timeline' },
    { value: 'files', label: 'Files', triggerLabel: 'Show as: Files' }
  ];
  const sortOptions = $derived([
    ...(sort?.options ?? []).map((option) => ({ ...option, triggerLabel: `Sort: ${option.label}` })),
    ...(sort?.note ? [{ value: '__note', label: sort.note, disabled: true }] : [])
  ]);
  const hasChips = $derived(
    Boolean(query) || filters.length > 0 || extraChips.length > 0 || groupingChain.length > 0
  );

  function selectGrouping(value: string): void {
    if (isGroupingDimension(value)) onAddGroup(value);
  }

  function selectPresentation(value: string): void {
    onPresentationChange?.(value as ExploreURLState['presentation']);
  }

  function selectSort(value: string): void {
    sort?.onchange?.(value);
  }
</script>

{#snippet popoverExtra()}
  {@render filterExtra?.()}
  <div class="popover-extra">
    {@render extraControls?.()}
  </div>
{/snippet}

<section class="context-bar" aria-label="Active analytical context" data-toolbar>
  <!-- L7: groups of 28 controls, 8 apart inside a group and 16 between groups; a group wraps whole. -->
  <div class="context-controls">
    <div class="context-group">
    {#if messages}
      <FilterMenu {client} {filters} {lockedFilter} extraActive={extraInPopover && extraActive} onChange={onFiltersChange} onClear={() => { onClearFilters(); if (extraInPopover) onClearExtra?.(); }} extra={extraInPopover ? popoverExtra : filterExtra} />
    {:else}
    <Button
      surface={filtersOpen || filters.length > 0 ? 'soft' : 'outline'}
      label="Filters"
      ariaLabel="Filters"
      ariaExpanded={filtersOpen}
      onclick={() => { filtersOpen = !filtersOpen; }}
    />
    {/if}
    </div>
    {#if compact.current && !lockedFilter}
    <div class="context-group">
      <ViewMenu {presentation} {presentationOptions} {groupOptions} {sort} {sortOptions} {selectPresentation} {selectGrouping} {selectSort} />
    </div>
    {:else if !lockedFilter || sort}
    <div class="context-group">
    {#if !lockedFilter}
    <SelectDropdown
      value={presentation}
      options={presentationOptions}
      onchange={selectPresentation}
    />
    <div class="group-picker" data-group-picker>
      <SelectDropdown
        value=""
        {options}
        disabled={!canGroup}
        onchange={selectGrouping}
      />
    </div>
    {/if}
    {#if sort}
      <div data-sort-menu>
        <SelectDropdown
          value={sort.value}
          options={sortOptions}
          onchange={selectSort}
        />
      </div>
    {/if}
    </div>
    {/if}
    {#if extraControls && !extraInPopover}<div class="context-group">{@render extraControls()}</div>{/if}
    <span class="context-count" aria-live="polite" data-mono>{countLabel}</span>
  </div>

  {#if hasChips}
    <div class="context-chips">
      {#if query}
        <Chip size="sm" uppercase={false} class="chip" title={`${searchModeLabel(searchMode)}: “${query}”`}>
          {searchModeLabel(searchMode)}: “{query}”
          {#snippet trailing()}
            <IconButton size="sm" ariaLabel="Remove search" onclick={() => onRemoveQuery?.()}
            ><XIcon size="12" aria-hidden="true" /></IconButton>
          {/snippet}
        </Chip>
      {/if}
      {#each filters as filter, index (`${filter.dimension}:${filter.values.join('\u0000')}`)}
        <Chip size="sm" uppercase={false} class="chip chip--filter" title={`${filterDimensionLabel(filter.dimension)}: ${filter.values.map((value) => filterValueLabel(filter.dimension, value)).join(', ')}`}>
          {filterDimensionLabel(filter.dimension)}: {filter.values.map((value) => filterValueLabel(filter.dimension, value)).join(', ')}
          {#snippet trailing()}
            <IconButton
              size="sm"
              ariaLabel={`Remove ${filterDimensionLabel(filter.dimension)} filter`}
              onclick={() => onRemoveFilter?.(index)}
            ><XIcon size="12" aria-hidden="true" /></IconButton>
          {/snippet}
        </Chip>
      {/each}
      {#each extraChips as chip (chip.key)}
        <Chip size="sm" uppercase={false} class="chip chip--filter" title={chip.label}>
          {chip.label}
          {#snippet trailing()}
            <IconButton size="sm" ariaLabel={chip.removeLabel} onclick={chip.onRemove}
            ><XIcon size="12" aria-hidden="true" /></IconButton>
          {/snippet}
        </Chip>
      {/each}
      {#each groupingChain as dimension, index (`${dimension}:${index}`)}
        <Chip size="sm" uppercase={false} class="chip chip--group" title={groupedByLabel([dimension])}>
          {groupedByLabel([dimension])}
          {#snippet trailing()}
            <IconButton
              size="sm"
              ariaLabel={`Remove ${filterDimensionLabel(dimension)} grouping`}
              onclick={() => onRemoveGroup(index)}
            ><XIcon size="12" aria-hidden="true" /></IconButton>
          {/snippet}
        </Chip>
      {/each}
    </div>
  {/if}

  {#if filtersOpen && !messages}
    <div class="filter-panel">
      <div class="filter-summary">
        {#if filters.length === 0}
          <span>No filters applied.</span>
        {:else}
          <span>{filters.length} active {filters.length === 1 ? 'filter' : 'filters'}</span>
          <Button surface="outline" label="Clear filters" onclick={onClearFilters} />
        {/if}
      </div>
      <AccountFilter {client} {filters} onChange={onFiltersChange} />
      <IdentityFilter {client} {filters} onChange={onFiltersChange} />
    </div>
  {/if}
</section>

<style>
  /* A boxed toolbar: controls inset 12 from the box, chips 8 under the controls. */
  .context-bar {
    position: relative;
    display: flex;
    min-width: 0;
    flex-direction: column;
    gap: var(--space-group);
    padding: var(--space-box);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    font-size: var(--type-meta-size);
    line-height: var(--type-meta-line);
  }

  .context-controls {
    display: flex;
    min-width: 0;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-group) var(--space-pane);
  }

  .context-group {
    display: flex;
    min-width: 0;
    flex-wrap: nowrap;
    align-items: center;
    gap: var(--space-group);
  }

  .context-chips {
    display: flex;
    min-width: 0;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-group);
  }

  .group-picker {
    width: 172px;
  }

  .popover-extra {
    min-width: 0;
    overflow-wrap: anywhere;

    :global(.file-controls > label:has(.kit-search-input, input[type="file"])) {
      display: grid;
      width: 100%;
      grid-template-columns: minmax(0, 1fr);
    }

    :global(.file-controls > label > .kit-search-input) {
      width: 100%;
    }
  }

  .context-chips :global(.chip) {
    background: var(--bg-inset);
    color: var(--text-secondary);
  }

  .context-chips :global(.chip--filter) {
    border: 1px solid color-mix(in srgb, var(--accent-amber) 35%, var(--border-muted));
    background: color-mix(in srgb, var(--accent-amber) 8%, var(--bg-surface));
  }

  .context-chips :global(.chip--group) {
    border: 1px solid color-mix(in srgb, var(--accent-teal) 35%, var(--border-muted));
    background: color-mix(in srgb, var(--accent-teal) 8%, var(--bg-surface));
  }

  .context-count {
    margin-left: auto;
    color: var(--text-muted);
    white-space: nowrap;
  }

  .filter-panel {
    position: absolute;
    z-index: var(--z-popover);
    top: calc(100% + var(--space-tight));
    left: 0;
    display: flex;
    min-width: 320px;
    align-items: stretch;
    flex-direction: column;
    gap: var(--space-group);
    padding: var(--space-box);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-muted);
    box-shadow: var(--shadow-md);
  }

  .filter-summary {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-group);
  }

  @media (max-width: 640px) {
    .context-count {
      margin-left: 0;
    }
  }
</style>
