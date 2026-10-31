<script module lang="ts">
  export interface PeopleRow {
    /** `cluster:N`, `domain:name`, or `person:N` for a saved person listed on its own. */
    key: string;
    label: string;
    /** "42 messages", or the organization for a saved person. */
    detail: string;
    lastAt: string;
    saved: boolean;
    domain: boolean;
  }
</script>

<script lang="ts">
  import {
    Button,
    Chip,
    FilterDropdown,
    EmptyState,
    IconButton,
    SegmentedControl,
    SelectDropdown,
    Tooltip,
    type FilterDropdownSection,
  } from '@kenn-io/kit-ui';
  import BookmarkCheck from '@lucide/svelte/icons/bookmark-check';
  import X from '@lucide/svelte/icons/x';

  import type { ExploreCacheUnavailable } from '../../explore/models';
  import { ExploreSelectionState } from '../../explore/state.svelte';
  import { compactDate as relativeDate } from '../../util/dates';
  import type { RelationshipFacet } from '../../relationships/models';
  import IdentityAvatar from '../common/IdentityAvatar.svelte';
  import SelectionCheckbox from '../common/SelectionCheckbox.svelte';

  const LOAD_MORE_PROXIMITY_PX = 240;

  interface FilterChip {
    id: string;
    label: string;
    onremove: () => void;
  }

  interface Props {
    rows: PeopleRow[];
    loading: boolean;
    loadingMore?: boolean;
    pageRecovery?: 'retry' | 'reload' | null;
    hasMore?: boolean;
    /** "99 people" once the total is known. */
    countLabel?: string;
    error: string | null;
    degraded: ExploreCacheUnavailable | null;
    facet: RelationshipFacet;
    query: string;
    activeKey: string | null;
    selection: ExploreSelectionState;
    filterSections: FilterDropdownSection[];
    filterCount: number;
    onResetFilters: () => void;
    chips: FilterChip[];
    sort: string;
    sortOptions: Array<{ value: string; label: string }>;
    /** Linking requires observed people, in addition to a selection count. */
    canMerge: boolean;
    onFacetChange: (facet: RelationshipFacet) => void;
    onSortChange: (value: string) => void;
    onOpen: (key: string) => void;
    onLoadMore?: () => void;
    onRetry?: () => void;
    onMerge: () => void;
  }

  let {
    rows,
    loading,
    loadingMore = false,
    pageRecovery = null,
    hasMore = false,
    countLabel = '',
    error,
    degraded,
    facet,
    query,
    activeKey,
    selection,
    filterSections,
    filterCount,
    onResetFilters,
    chips,
    sort,
    sortOptions,
    canMerge,
    onFacetChange,
    onSortChange,
    onOpen,
    onLoadMore = undefined,
    onRetry = undefined,
    onMerge,
  }: Props = $props();

  let gridElement = $state<HTMLDivElement>();
  let focusKey = $state<string | null>(null);
  let pointerFocus = $state(false);

  const orderedKeys = $derived(rows.map((row) => row.key));
  const focusIndex = $derived(focusKey === null ? -1 : orderedKeys.indexOf(focusKey));
  const anySelected = $derived(selection.count > 0);

  $effect(() => {
    if (focusKey !== null && orderedKeys.includes(focusKey)) return;
    focusKey = activeKey && orderedKeys.includes(activeKey) ? activeKey : (orderedKeys[0] ?? null);
  });

  function editableTarget(target: EventTarget | null): boolean {
    return Boolean((target as HTMLElement | null)?.closest('input, textarea, select, [contenteditable]:not([contenteditable="false"])'));
  }

  function moveTo(index: number): void {
    if (rows.length === 0) return;
    const next = Math.max(0, Math.min(rows.length - 1, index));
    focusKey = rows[next]!.key;
    const key = focusKey;
    Array.from(gridElement?.querySelectorAll<HTMLElement>('[data-row-key]') ?? [])
      .find((element) => element.dataset.rowKey === key)
      ?.scrollIntoView({ block: 'nearest' });
  }

  function handleKeydown(event: KeyboardEvent): void {
    pointerFocus = false;
    if (editableTarget(event.target) || rows.length === 0) return;
    if (event.metaKey || event.ctrlKey || event.altKey) return;
    if (event.key === 'j' || event.key === 'ArrowDown') moveTo(focusIndex + 1);
    else if (event.key === 'k' || event.key === 'ArrowUp') moveTo(focusIndex - 1);
    else if (event.key === 'Enter' && focusIndex >= 0) onOpen(rows[focusIndex]!.key);
    else if (event.key === ' ' && focusIndex >= 0 && !rows[focusIndex]!.domain) {
      selection.toggle(rows[focusIndex]!.key, focusIndex, orderedKeys, event.shiftKey);
    } else return;
    event.preventDefault();
    event.stopPropagation();
  }

  function handleScroll(): void {
    if (!gridElement || !hasMore || loadingMore || loading) return;
    const remaining = gridElement.scrollHeight - gridElement.scrollTop - gridElement.clientHeight;
    if (remaining <= LOAD_MORE_PROXIMITY_PX) onLoadMore?.();
  }

  function selectAllLoaded(): void {
    selection.selectVisible(rows.filter((row) => !row.domain).map((row) => row.key));
  }
</script>

<aside class="people-list" aria-label="People">
  <div class="toolbar">
    <div class="toolbar-row">
      <SegmentedControl
        ariaLabel="List"
        value={facet}
        options={[
          { value: 'people', label: 'People' },
          { value: 'domains', label: 'Domains' },
        ]}
        onchange={(value) => onFacetChange(value as RelationshipFacet)}
      />
      {#if facet === 'people'}
        <FilterDropdown
          label="Filter"
          title="Filter people"
          sections={filterSections}
          badgeCount={filterCount}
          resetLabel="Clear filters"
          onReset={onResetFilters}
          searchable
          searchPlaceholder="Find a filter"
        />
      {/if}
      <SelectDropdown class="sort-select" title="Sort" value={sort} options={sortOptions} onchange={onSortChange} />
      <span class="spacer"></span>

    </div>
    {#if chips.length > 0}
      <div class="chips">
        {#each chips as chip (chip.id)}
          <Chip size="sm" tone="neutral" uppercase={false}>
            {chip.label}
            {#snippet trailing()}
              <IconButton size="sm" ariaLabel={`Remove ${chip.label}`} onclick={chip.onremove}>
                <X size={12} aria-hidden="true" />
              </IconButton>
            {/snippet}
          </Chip>
        {/each}
      </div>
    {/if}
    {#if countLabel}<span class="count" aria-live="polite">{countLabel}</span>{/if}
  </div>

  {#if degraded?.readiness === 'building'}
    <EmptyState title="Preparing the people list…" />
  {:else if degraded}
    <EmptyState title="The people list needs the analytics cache" description="Run msgvault build-cache to prepare the analytics cache." />
  {:else if error && rows.length === 0}
    <EmptyState title={error} />{#if onRetry}<Button label="Retry" onclick={onRetry} />{/if}
  {:else}
    {#if error}
      <div class="list-error" role="alert">
        <span>{error}</span>
        {#if pageRecovery === 'reload'}<Button size="sm" surface="outline" label="Reload" onclick={onRetry} />
        {:else if hasMore}<Button size="sm" surface="outline" label="Retry" onclick={() => onLoadMore?.()} />{/if}
      </div>
    {/if}
    <div
      class="rows"
      class:selection-mode={anySelected}
      class:pointer-focus={pointerFocus}
      role="grid"
      aria-label="People results"
      aria-busy={loading || loadingMore}
      tabindex="0"
      data-scroll
      bind:this={gridElement}
      onkeydown={handleKeydown}
      onscroll={handleScroll}
    >
      {#if loading && rows.length === 0}
        <p class="list-status" role="status">Loading…</p>
      {:else if rows.length === 0}
        <EmptyState
          title={query.trim() ? 'No matches' : 'No people yet'}
        />
      {:else}
        {#each rows as row, index (row.key)}
          <!-- svelte-ignore a11y_click_events_have_key_events, a11y_interactive_supports_focus --
               The grid owns focus and opens its active row with Enter. -->
          <div
            class="row"
            class:open={row.key === activeKey}
            class:focus={row.key === focusKey}
            role="row"
            data-row-key={row.key}
            aria-selected={row.key === activeKey}
            onpointerdown={() => {
              pointerFocus = true;
              focusKey = row.key;
              gridElement?.focus();
            }}
            onclick={(event) => {
              if (event.button === 0) onOpen(row.key);
            }}
          >
            <div role="gridcell">
              <span class="avatar-slot">
                <span class="avatar">
                  <IdentityAvatar label={row.label} seed={row.key} shape={row.domain ? 'domain' : 'person'} size={28} />
                </span>
                {#if !row.domain}
                  <SelectionCheckbox
                    checked={selection.isSelected(row.key)}
                    label={`${selection.isSelected(row.key) ? 'Unselect' : 'Select'} ${row.label}`}
                    inGrid
                    onToggle={(range) => selection.toggle(row.key, index, orderedKeys, range)}
                  />
                {/if}
              </span>
              <div class="body">
                <div class="line">
                  <span class="name" title={row.label}>{row.label}</span>
                  {#if row.saved}
                    <Tooltip text="Saved"><span class="saved" role="img" aria-label="Saved"><BookmarkCheck size={12} aria-hidden="true" /></span></Tooltip>
                  {/if}
                  <span class="when" data-mono>{relativeDate(row.lastAt)}</span>
                </div>
                <span class="detail" data-mono>{row.detail}</span>
              </div>
            </div>
          </div>
        {/each}
        {#if loadingMore}<p class="list-status" role="status">Loading…</p>{/if}
      {/if}
    </div>
  {/if}

  {#if selection.count > 0}
    <div class="selection-bar"><span>{selection.count} selected</span>
      <Button label="Select loaded" onclick={selectAllLoaded} /><Button label="Clear selection" onclick={() => selection.clear()} />
      <Button label="Same person" disabled={!canMerge} onclick={onMerge} />
    </div>
  {/if}
</aside>

<style>
  .selection-bar { display: flex; align-items: center; gap: var(--control-gap); flex-wrap: wrap; padding: var(--space-3); }
  .people-list {
    display: flex;
    min-width: 0;
    min-height: 0;
    height: 100%;
    flex: 1;
    flex-direction: column;
    overflow: hidden;
    background: var(--bg-primary);
  }

  .toolbar {
    display: flex;
    flex-direction: column;
    gap: var(--control-gap);
    padding: var(--pane-pad-y) var(--pane-pad-x);
    border-bottom: 1px solid var(--border-muted);
  }

  .toolbar-row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--control-gap);
  }

  .toolbar-row :global(.sort-select) {
    min-width: 0;
  }

  .toolbar-row > :global(*) {
    flex: none;
  }

  .toolbar-row > .spacer {
    flex: 1;
  }

  .chips {
    display: flex;
    flex-wrap: wrap;
    gap: var(--control-gap);
  }

  .count {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .rows {
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    overflow: auto;
    outline: none;
  }

  .rows:focus-visible:not(.pointer-focus) {
    outline: var(--focus-ring);
    outline-offset: -2px;
  }

  .row {
    flex: none;
    cursor: default;
    border-start-start-radius: 0;
    border-end-start-radius: 0;
    box-shadow: inset 2px 0 0 transparent;
  }

  .row [role='gridcell'] {
    display: flex;
    min-height: 52px;
    align-items: center;
    gap: var(--control-gap);
    padding: var(--pane-pad-y) var(--pane-pad-x);
  }

  .row:hover {
    background: var(--bg-surface-hover);
  }

  .row.focus:not(.open) {
    background: var(--bg-surface-hover);
  }

  .row.open {
    border-start-start-radius: 0;
    border-end-start-radius: 0;
    background: var(--selected-bg);
    box-shadow: inset 2px 0 0 var(--accent-blue);
  }

  .avatar-slot {
    position: relative;
    display: grid;
    width: 28px;
    height: 28px;
    flex: none;
    place-items: center;
  }

  .avatar,
  .avatar-slot :global(.selection-checkbox) {
    position: absolute;
    transition:
      opacity 80ms ease-out,
      transform 80ms ease-out;
  }

  .avatar-slot :global(.selection-checkbox) {
    opacity: 0;
    transform: scale(0.9);
  }

  .row:hover .avatar,
  .row:focus-within .avatar,
  .rows.selection-mode .avatar {
    opacity: 0;
    transform: scale(0.9);
  }

  .row:hover .avatar-slot :global(.selection-checkbox),
  .row:focus-within .avatar-slot :global(.selection-checkbox),
  .rows.selection-mode .avatar-slot :global(.selection-checkbox) {
    opacity: 1;
    transform: scale(1);
  }

  .body {
    display: flex;
    min-width: 0;
    flex: 1;
    flex-direction: column;
    gap: var(--space-1);
  }

  .line {
    display: flex;
    align-items: center;
    gap: var(--control-gap);
  }

  .name {
    min-width: 0;
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    font-weight: var(--font-weight-medium, 500);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .saved {
    display: inline-flex;
    flex: none;
    color: var(--accent-blue);
  }

  .when {
    flex: none;
    margin-left: auto;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .detail {
    min-height: 1em;
    overflow: hidden;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .list-status {
    margin: 0;
    padding: var(--pane-pad-y) var(--pane-pad-x);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .list-error {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--control-gap);
    padding: var(--pane-pad-y) var(--pane-pad-x);
    background: color-mix(in srgb, var(--accent-red) 10%, transparent);
    color: var(--text-primary);
    font-size: var(--font-size-xs);
  }
</style>
