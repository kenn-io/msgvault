<script lang="ts">
  import XIcon from '@lucide/svelte/icons/x';
  import { Button, IconButton, SelectDropdown } from '@kenn-io/kit-ui';

  import type { APIClient } from '../../api/client';
  import type { ExploreFilter, ExploreGroupDimension, ExploreSearchMode, ExploreURLState } from '../../explore/models';
  import {
    groupingDimensionLabel,
    groupingOptions,
    isGroupingDimension
  } from '../../grouping/catalog';
  import IdentityFilter from './IdentityFilter.svelte';

  let {
    client,
    query,
    searchMode,
    filters,
    groupingChain,
    totalCount = undefined,
    presentation = 'table',
    onAddGroup,
    onRemoveGroup,
    onClearFilters,
    onFiltersChange,
    onSort = undefined,
    onPresentationChange = undefined
  }: {
    client: APIClient;
    query: string;
    searchMode: ExploreSearchMode;
    filters: ExploreFilter[];
    groupingChain: ExploreGroupDimension[];
    totalCount?: number;
    presentation?: ExploreURLState['presentation'];
    onAddGroup: (dimension: ExploreGroupDimension) => void;
    onRemoveGroup: (index: number) => void;
    onClearFilters: () => void;
    onFiltersChange: (filters: ExploreFilter[]) => void;
    onSort?: () => void;
    onPresentationChange?: (presentation: ExploreURLState['presentation']) => void;
  } = $props();

  let filtersOpen = $state(false);
  const options = $derived(groupingOptions({ excluded: groupingChain, includeUnavailable: true }));
  const firstRequestable = $derived(options.find((option) => !option.disabled)?.value ?? '');
  const presentationOptions = [
    { value: 'table', label: 'Table' },
    { value: 'timeline', label: 'Timeline' },
    { value: 'files', label: 'Files' }
  ];

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
      title="Show as"
      value={presentation}
      options={presentationOptions}
      onchange={(value) => onPresentationChange?.(value as ExploreURLState['presentation'])}
    />
    <div class="group-picker" data-group-picker>
      <SelectDropdown
        value={firstRequestable}
        {options}
        title="Group by"
        disabled={!firstRequestable}
        onchange={selectGrouping}
      />
    </div>
    <Button
      size="sm"
      surface="outline"
      label="Newest first"
      ariaLabel="Sort: newest first"
      onclick={() => onSort?.()}
    />
  </div>

  <div class="context-crumbs">
    {#if query}
      <span class="crumb crumb--query">{searchMode}: “{query}”</span>
    {/if}
    {#each filters as filter (`${filter.dimension}:${filter.values.join('\u0000')}`)}
      <span class="crumb crumb--filter">Filter {filter.dimension}: {filter.values.join(', ')}</span>
    {/each}
    {#each groupingChain as dimension, index (`${dimension}:${index}`)}
      <span class="crumb crumb--group">
        Group {groupingDimensionLabel(dimension)}
        <IconButton
          size="sm"
          ariaLabel={`Remove ${groupingDimensionLabel(dimension)} grouping`}
          onclick={() => onRemoveGroup(index)}
        ><XIcon size="12" aria-hidden="true" /></IconButton>
      </span>
    {/each}
    {#if !query && filters.length === 0 && groupingChain.length === 0}
      <span class="empty-context">All archive entries</span>
    {/if}
  </div>

  <span class="context-count" data-mono>{totalCount === undefined ? 'Count pending' : `${totalCount.toLocaleString()} results`}</span>

  {#if filtersOpen}
    <div class="filter-panel">
      <div class="filter-summary">
        {#if filters.length === 0}
          <span>No active filters. Filtering controls will expand with additional canonical dimensions.</span>
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
    min-height: 34px;
    align-items: center;
    gap: var(--space-4);
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    font-size: var(--font-size-xs);
  }

  .context-controls,
  .context-crumbs {
    display: flex;
    min-width: 0;
    align-items: center;
    gap: var(--space-2);
  }

  .context-crumbs {
    flex: 1;
    overflow-x: auto;
  }

  .group-picker {
    width: 172px;
  }

  .crumb {
    display: inline-flex;
    flex: none;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-secondary);
    white-space: nowrap;
  }

  .crumb--filter {
    border: 1px solid color-mix(in srgb, var(--accent-amber) 35%, var(--border-muted));
    background: color-mix(in srgb, var(--accent-amber) 8%, var(--bg-surface));
  }

  .crumb--group {
    border: 1px solid color-mix(in srgb, var(--accent-teal) 35%, var(--border-muted));
    background: color-mix(in srgb, var(--accent-teal) 8%, var(--bg-surface));
  }

  .empty-context,
  .context-count,
  .filter-panel {
    color: var(--text-muted);
  }

  .context-count {
    flex: none;
    font-variant-numeric: tabular-nums;
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
    box-shadow: var(--shadow-md);
  }

  .filter-summary {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
  }
</style>
