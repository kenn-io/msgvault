<script lang="ts">
  import { Button, SelectDropdown } from '@kenn-io/kit-ui';

  import type { ExploreFilter, ExploreGroupDimension, ExploreSearchMode, ExploreURLState } from '../../explore/models';
  import {
    groupingDimensionLabel,
    groupingOptions,
    isGroupingDimension
  } from '../../grouping/catalog';

  let {
    query,
    searchMode,
    filters,
    groupingChain,
    totalCount = undefined,
    presentation = 'table',
    onAddGroup,
    onRemoveGroup,
    onClearFilters,
    onSort = undefined,
    onPresentationChange = undefined
  }: {
    query: string;
    searchMode: ExploreSearchMode;
    filters: ExploreFilter[];
    groupingChain: ExploreGroupDimension[];
    totalCount?: number;
    presentation?: ExploreURLState['presentation'];
    onAddGroup: (dimension: ExploreGroupDimension) => void;
    onRemoveGroup: (index: number) => void;
    onClearFilters: () => void;
    onSort?: () => void;
    onPresentationChange?: (presentation: ExploreURLState['presentation']) => void;
  } = $props();

  let filtersOpen = $state(false);
  const options = $derived(groupingOptions({ excluded: groupingChain, includeUnavailable: true }));
  const firstRequestable = $derived(options.find((option) => !option.disabled)?.value ?? '');

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
    <label class="show-as">
      <span>Show as</span>
      <select
        aria-label="Show as"
        value={presentation}
        onchange={(event) => onPresentationChange?.(
          event.currentTarget.value as ExploreURLState['presentation']
        )}
      >
        <option value="table">Table</option>
        <option value="timeline">Timeline</option>
        <option value="files">Files</option>
      </select>
    </label>
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
        <button type="button" aria-label={`Remove ${groupingDimensionLabel(dimension)} grouping`} onclick={() => onRemoveGroup(index)}>×</button>
      </span>
    {/each}
    {#if !query && filters.length === 0 && groupingChain.length === 0}
      <span class="empty-context">All archive entries</span>
    {/if}
  </div>

  <span class="context-count" data-mono>{totalCount === undefined ? 'Count pending' : `${totalCount.toLocaleString()} results`}</span>

  {#if filtersOpen}
    <div class="filter-panel">
      {#if filters.length === 0}
        <span>No active filters. Filtering controls will expand with additional canonical dimensions.</span>
      {:else}
        <span>{filters.length} active {filters.length === 1 ? 'filter' : 'filters'}</span>
        <Button size="sm" surface="outline" label="Clear filters" onclick={onClearFilters} />
      {/if}
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

  .show-as {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    white-space: nowrap;
  }

  .show-as select {
    min-height: var(--control-height);
    border: 1px solid var(--control-border);
    border-radius: var(--radius-sm);
    background: var(--control-bg);
    color: var(--text-primary);
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
    border-left: 2px solid var(--accent-amber);
  }

  .crumb--group {
    border-left: 2px solid var(--accent-teal);
  }

  .crumb button {
    padding: 0;
    border: 0;
    background: transparent;
    color: var(--text-muted);
    cursor: pointer;
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
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    padding: var(--space-4);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    box-shadow: var(--shadow-md);
  }
</style>
