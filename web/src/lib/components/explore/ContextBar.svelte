<script lang="ts">
  import XIcon from '@lucide/svelte/icons/x';
  import { Button, IconButton, SelectDropdown } from '@kenn-io/kit-ui';
  import type { Snippet } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { ExploreFilter, ExploreGroupDimension, ExploreSearchMode, ExploreURLState } from '../../explore/models';
  import { filterDimensionLabel, groupedByLabel, searchModeLabel } from '../../explore/labels';
  import { groupingOptions, isGroupingDimension } from '../../grouping/catalog';
  import IdentityFilter from './IdentityFilter.svelte';

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
    query,
    searchMode,
    filters,
    groupingChain,
    countLabel,
    sort = undefined,
    presentation = 'table',
    extra = undefined,
    extraChips = [],
    onAddGroup,
    onRemoveGroup,
    onClearFilters,
    onFiltersChange,
    onRemoveQuery = undefined,
    onRemoveFilter = undefined,
    onPresentationChange = undefined
  }: {
    client: APIClient;
    query: string;
    searchMode: ExploreSearchMode;
    filters: ExploreFilter[];
    groupingChain: ExploreGroupDimension[];
    countLabel: string;
    sort?: SortConfig;
    presentation?: ExploreURLState['presentation'];
    extra?: Snippet;
    extraChips?: ExtraChip[];
    onAddGroup: (dimension: ExploreGroupDimension) => void;
    onRemoveGroup: (index: number) => void;
    onClearFilters: () => void;
    onFiltersChange: (filters: ExploreFilter[]) => void;
    onRemoveQuery?: () => void;
    onRemoveFilter?: (index: number) => void;
    onPresentationChange?: (presentation: ExploreURLState['presentation']) => void;
  } = $props();

  let filtersOpen = $state(false);
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
</script>

<section class="context-bar" aria-label="Active analytical context">
  <div class="context-controls">
    <Button
      size="sm"
      surface={filtersOpen || filters.length > 0 ? 'soft' : 'outline'}
      label="Filters"
      ariaLabel="Filters"
      ariaExpanded={filtersOpen}
      onclick={() => { filtersOpen = !filtersOpen; }}
    />
    <SelectDropdown
      value={presentation}
      options={presentationOptions}
      onchange={(value) => onPresentationChange?.(value as ExploreURLState['presentation'])}
    />
    <div class="group-picker" data-group-picker>
      <SelectDropdown
        value=""
        {options}
        disabled={!canGroup}
        onchange={selectGrouping}
      />
    </div>
    {#if sort}
      <div data-sort-menu>
        <SelectDropdown
          value={sort.value}
          options={sortOptions}
          onchange={(value) => sort.onchange?.(value)}
        />
      </div>
    {/if}
    {@render extra?.()}
    <span class="context-count" aria-live="polite" data-mono>{countLabel}</span>
  </div>

  {#if hasChips}
    <div class="context-chips">
      {#if query}
        <span class="chip">
          {searchModeLabel(searchMode)}: “{query}”
          <IconButton size="sm" ariaLabel="Remove search" onclick={() => onRemoveQuery?.()}
          ><XIcon size="12" aria-hidden="true" /></IconButton>
        </span>
      {/if}
      {#each filters as filter, index (`${filter.dimension}:${filter.values.join('\u0000')}`)}
        <span class="chip chip--filter">
          {filterDimensionLabel(filter.dimension)}: {filter.values.join(', ')}
          <IconButton
            size="sm"
            ariaLabel={`Remove ${filterDimensionLabel(filter.dimension)} filter`}
            onclick={() => onRemoveFilter?.(index)}
          ><XIcon size="12" aria-hidden="true" /></IconButton>
        </span>
      {/each}
      {#each extraChips as chip (chip.key)}
        <span class="chip chip--filter">
          {chip.label}
          <IconButton size="sm" ariaLabel={chip.removeLabel} onclick={chip.onRemove}
          ><XIcon size="12" aria-hidden="true" /></IconButton>
        </span>
      {/each}
      {#each groupingChain as dimension, index (`${dimension}:${index}`)}
        <span class="chip chip--group">
          {groupedByLabel([dimension])}
          <IconButton
            size="sm"
            ariaLabel={`Remove ${filterDimensionLabel(dimension)} grouping`}
            onclick={() => onRemoveGroup(index)}
          ><XIcon size="12" aria-hidden="true" /></IconButton>
        </span>
      {/each}
    </div>
  {/if}

  {#if filtersOpen}
    <div class="filter-panel">
      <div class="filter-summary">
        {#if filters.length === 0}
          <span>No filters applied.</span>
        {:else}
          <span>{filters.length} active {filters.length === 1 ? 'filter' : 'filters'}</span>
          <Button size="sm" surface="outline" label="Clear filters" onclick={onClearFilters} />
        {/if}
      </div>
      <IdentityFilter {client} {filters} onChange={onFiltersChange} />
    </div>
  {/if}
</section>

<style>
  .context-bar {
    position: relative;
    display: flex;
    min-width: 0;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    font-size: var(--font-size-xs);
  }

  .context-controls,
  .context-chips {
    display: flex;
    min-width: 0;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }

  .group-picker {
    width: 172px;
  }

  .chip {
    display: inline-flex;
    max-width: 100%;
    align-items: center;
    gap: var(--space-1);
    padding: 0 0 0 var(--space-2);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-secondary);
    overflow-wrap: anywhere;
  }

  .chip--filter {
    border: 1px solid color-mix(in srgb, var(--accent-amber) 35%, var(--border-muted));
    background: color-mix(in srgb, var(--accent-amber) 8%, var(--bg-surface));
  }

  .chip--group {
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
    top: calc(100% + var(--space-2));
    left: 0;
    display: flex;
    min-width: 320px;
    align-items: stretch;
    flex-direction: column;
    gap: var(--space-4);
    padding: var(--space-4);
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
    gap: var(--space-4);
  }
</style>
