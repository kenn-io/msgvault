<script lang="ts">
  import { Button, virtualSlice } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick, untrack } from 'svelte';

  import type {
    ExploreCacheUnavailable,
    ExploreGroupDimension,
    ExploreGroupRow,
    ExploreScrollAnchor
  } from '../../explore/models';
  import { rebaseVirtualScroll, RowGeometry, tableViewportHeight } from '../../theme/preferences.svelte';

  let {
    rows,
    dimension,
    loading = false,
    loadingMore = false,
    hasMore = false,
    totalCount = undefined,
    generation = 0,
    error = '',
    unavailable = undefined,
    drillable = true,
    focusedKey = null,
    inspectedKey = null,
    scrollAnchor = null,
    restoring = false,
    workspaceLabel = 'Everything',
    onDrill,
    onInspect = undefined,
    onLoadMore = undefined,
    onLoadThroughEnd = undefined,
    onActiveKey = undefined,
    onScrollAnchor = undefined,
    onRetry = undefined
  }: {
    rows: ExploreGroupRow[];
    dimension: ExploreGroupDimension;
    loading?: boolean;
    loadingMore?: boolean;
    hasMore?: boolean;
    totalCount?: number;
    generation?: number;
    error?: string;
    unavailable?: ExploreCacheUnavailable;
    drillable?: boolean;
    focusedKey?: string | null;
    inspectedKey?: string | null;
    scrollAnchor?: ExploreScrollAnchor | null;
    restoring?: boolean;
    workspaceLabel?: string;
    onDrill: (row: ExploreGroupRow) => void;
    onInspect?: (row: ExploreGroupRow) => void;
    onLoadMore?: () => Promise<unknown>;
    onLoadThroughEnd?: () => Promise<void>;
    onActiveKey?: (key: string) => void;
    onScrollAnchor?: (key: string, offset: number) => void;
    onRetry?: () => void;
  } = $props();

  const geometry = new RowGeometry();
  const rowHeight = $derived(geometry.height);
  const OVERSCAN = 6;
  const TEMPLATE = 'minmax(220px, 2fr) 100px 120px 132px 112px';
  let gridElement = $state<HTMLDivElement>();
  let headerElement = $state<HTMLDivElement>();
  let scrollTop = $state(0);
  let viewport = $state(360);
  let activeKey = $state<string | null>(untrack(() => focusedKey));
  let restoredAnchor = '';
  let suppressedScrollTop: number | undefined;
  let densityRebasing = false;
  let previousRowHeight = untrack(() => rowHeight);
  const activeIndex = $derived(activeKey
    ? rows.findIndex((row) => stableKey(row) === activeKey)
    : rows.length ? 0 : -1);
  const slice = $derived.by(() => {
    const height = rowHeight;
    if (height === undefined) return undefined;
    return virtualSlice({
      scrollTop,
      viewport,
      count: rows.length,
      overscan: OVERSCAN,
      fixedHeight: height,
      heightOf: () => height
    });
  });
  const renderedRows = $derived(slice ? rows.slice(slice.start, slice.end) : []);
  const accessibilityRowCount = $derived.by(() => {
    if (loading || loadingMore || unavailable || error) return undefined;
    if (rows.length === 0 || !slice || rowHeight === undefined) return 2;
    return (totalCount ?? rows.length) + 1;
  });

  $effect(() => {
    const nextHeight = rowHeight;
    const previousHeight = previousRowHeight;
    previousRowHeight = nextHeight;
    if (!gridElement || nextHeight === undefined || previousHeight === undefined || previousHeight === nextHeight) return;
    const element = gridElement;
    densityRebasing = true;
    const preservedKey = activeKey;
    const rebased = activeIndex >= 0
      ? activeIndex * nextHeight
      : rebaseVirtualScroll(scrollTop, previousHeight, nextHeight);
    const expectedScrollHeight = rows.length * nextHeight;
    requestAnimationFrame(() => applyDensityRebase(
      element, nextHeight, expectedScrollHeight, rebased, preservedKey
    ));
  });

  $effect(() => {
    if (!focusedKey) return;
    const index = rows.findIndex((row) => stableKey(row) === focusedKey);
    if (index >= 0) {
      const isRestoring = untrack(() => restoring);
      const anchorWasRestored = scrollAnchor !== null &&
        restoredAnchor === anchorSignature(scrollAnchor);
      activeKey = focusedKey;
      if (!anchorWasRestored && (!isRestoring || !scrollAnchor)) scrollActiveIntoView(index);
      if (isRestoring && gridElement) suppressedScrollTop = gridElement.scrollTop;
    }
  });

  $effect(() => {
    const height = rowHeight;
    if (!gridElement || !scrollAnchor || height === undefined) return;
    const signature = anchorSignature(scrollAnchor);
    if (signature === restoredAnchor) return;
    const index = rows.findIndex((row) => stableKey(row) === scrollAnchor.key);
    if (index < 0) return;
    gridElement.scrollTop = index * height + scrollAnchor.offset;
    scrollTop = gridElement.scrollTop;
    suppressedScrollTop = gridElement.scrollTop;
    restoredAnchor = signature;
  });

  $effect(() => {
    rows;
    if (rows.length === 0) activeKey = null;
    else if (activeIndex < 0 && !restoring) {
      activeKey = stableKey(rows[0]!);
      onActiveKey?.(activeKey);
    }
  });

  onMount(() => {
    if (!gridElement) return;
    viewport = measuredViewport(gridElement);
    if (typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(() => {
      if (gridElement) viewport = measuredViewport(gridElement);
    });
    observer.observe(gridElement);
    if (headerElement) observer.observe(headerElement);
    return () => observer.disconnect();
  });
  onDestroy(() => geometry.destroy());

  function stableKey(row: ExploreGroupRow): string {
    return `group:${dimension}:${row.key}`;
  }

  function anchorSignature(anchor: ExploreScrollAnchor): string {
    return `${generation}:${dimension}:${anchor.key}:${anchor.offset}:${rows.length}`;
  }

  function rowId(row: ExploreGroupRow): string {
    return `everything-${encodeURIComponent(stableKey(row)).replaceAll('%', '-')}`;
  }

  function bytes(value: number): string {
    if (value < 1024) return `${value} B`;
    if (value < 1024 * 1024) return `${Math.round(value / 1024)} KB`;
    return `${(value / (1024 * 1024)).toFixed(1)} MB`;
  }

  function date(value: string): string {
    const parsed = new Date(value);
    return Number.isNaN(parsed.valueOf()) ? value : parsed.toLocaleDateString();
  }

  async function move(index: number): Promise<void> {
    if (rowHeight === undefined) return;
    if (index >= rows.length && hasMore) await onLoadMore?.();
    const bounded = Math.max(0, Math.min(rows.length - 1, index));
    activeKey = rows[bounded] ? stableKey(rows[bounded]!) : null;
    if (activeKey) onActiveKey?.(activeKey);
    scrollActiveIntoView(bounded);
    await tick();
  }

  function scrollActiveIntoView(index: number): void {
    const height = rowHeight;
    if (!gridElement || index < 0 || height === undefined) return;
    const top = index * height;
    const bottom = top + height;
    const visibleHeight = measuredViewport(gridElement);
    if (top < gridElement.scrollTop) gridElement.scrollTop = top;
    else if (bottom > gridElement.scrollTop + visibleHeight) gridElement.scrollTop = bottom - visibleHeight;
    scrollTop = gridElement.scrollTop;
  }

  function measuredViewport(element: HTMLDivElement): number {
    return tableViewportHeight(
      element.clientHeight,
      headerElement?.offsetHeight || 30,
      window.innerHeight
    );
  }

  function applyDensityRebase(
    element: HTMLDivElement,
    height: number,
    expectedScrollHeight: number,
    rebased: number,
    preservedKey: string | null,
  ): void {
    if (gridElement !== element || previousRowHeight !== height) return;
    if (!element.isConnected) return;
    if (element.scrollHeight !== 0 && (
      element.scrollHeight < expectedScrollHeight || element.clientHeight > window.innerHeight
    )) {
      requestAnimationFrame(() => applyDensityRebase(
        element, height, expectedScrollHeight, rebased, preservedKey
      ));
      return;
    }
    element.scrollTop = rebased;
    scrollTop = element.scrollTop;
    suppressedScrollTop = element.scrollTop;
    if (preservedKey && rows.some((row) => stableKey(row) === preservedKey)) {
      activeKey = preservedKey;
      onActiveKey?.(preservedKey);
    }
    requestAnimationFrame(() => {
      if (previousRowHeight === height) densityRebasing = false;
    });
  }

  async function handleKeydown(event: KeyboardEvent): Promise<void> {
    if (event.target !== gridElement || event.metaKey || event.ctrlKey || event.altKey || rows.length === 0 || rowHeight === undefined) return;
    if (event.key === 'j' || event.key === 'ArrowDown') await move(activeIndex + 1);
    else if (event.key === 'k' || event.key === 'ArrowUp') await move(activeIndex - 1);
    else if (event.key === 'Home') await move(0);
    else if (event.key === 'End') {
      if (hasMore) await onLoadThroughEnd?.();
      await move(rows.length - 1);
    } else if (event.key === 'Enter' && activeIndex >= 0 && drillable) onDrill(rows[activeIndex]!);
    else return;
    event.preventDefault();
  }

  function handleScroll(): void {
    scrollTop = gridElement?.scrollTop ?? 0;
    const height = rowHeight;
    if (height === undefined || !slice) return;
    if (densityRebasing) return;
    if (suppressedScrollTop !== undefined) {
      const isProgrammaticRestoration = Math.abs(scrollTop - suppressedScrollTop) < 0.5;
      suppressedScrollTop = undefined;
      if (isProgrammaticRestoration) return;
    }
    const first = Math.min(rows.length - 1, Math.max(0, Math.floor(scrollTop / height)));
    const row = rows[first];
    if (row && !restoring) {
      activeKey = stableKey(row);
      onActiveKey?.(activeKey);
      onScrollAnchor?.(activeKey, scrollTop - first * height);
    }
    if (!restoring && hasMore && !loadingMore && slice.end >= rows.length - OVERSCAN) void onLoadMore?.();
  }
</script>

<section class="group-table" aria-label={`Grouped by ${dimension}`}>
  <div
    class="group-grid"
    bind:this={gridElement}
    role="grid"
    data-scroll
    aria-label={`${workspaceLabel} grouped by ${dimension}`}
    aria-rowcount={accessibilityRowCount}
    aria-colcount="5"
    aria-busy={loading || loadingMore}
    aria-activedescendant={activeIndex >= 0 && rows[activeIndex] ? rowId(rows[activeIndex]!) : undefined}
    tabindex="0"
    onkeydown={handleKeydown}
    onscroll={handleScroll}
  >
    <div class="group-header" bind:this={headerElement} role="row" style:grid-template-columns={TEMPLATE}>
      <span role="columnheader">Group</span>
      <span role="columnheader" class="numeric">Items</span>
      <span role="columnheader" class="numeric">Estimated</span>
      <span role="columnheader">Latest</span>
      <span role="columnheader">Action</span>
    </div>
    <div class="group-body" role="rowgroup">
      {#if unavailable}
        <div role="row"><div role="gridcell" aria-colspan="5"><div class="group-cache-unavailable" role="alert">
        <strong>Analytical cache unavailable</strong>
        <span>{unavailable.message}</span>
        <span>Rebuild it with <code>{unavailable.recovery_action}</code>, then retry.</span>
        <div><Button label="Retry cache check" tone="info" surface="outline" onclick={() => onRetry?.()} /></div>
        </div></div></div>
      {:else if error}
        <div role="row"><div role="gridcell" aria-colspan="5"><div class="group-request-error" role="alert">
        <span>{error}</span>
        <Button
          label={restoring ? 'Retry restoration' : 'Retry request'}
          tone="info"
          surface="outline"
          onclick={() => onRetry?.()}
        />
        </div></div></div>
      {:else if loading && rows.length === 0}
        <div role="row"><div role="gridcell" aria-colspan="5"><div class="group-state" role="status">Loading grouped results…</div></div></div>
      {:else if rows.length === 0}
        <div role="row"><div role="gridcell" aria-colspan="5"><div class="group-state">No groups match this view.</div></div></div>
      {:else if !slice || rowHeight === undefined}
        <div role="row"><div role="gridcell" aria-colspan="5"><div class="group-state" role="status">Preparing grouped layout…</div></div></div>
      {:else}
      <div class="group-spacer" style:height={`${slice.totalHeight}px`}>
        <div class="group-window" style:transform={`translateY(${slice.topPad}px)`}>
          {#each renderedRows as row, offset (row.key)}
            {@const index = slice.start + offset}
            <div
              class="group-row"
              class:group-row--active={index === activeIndex}
              class:group-row--inspected={inspectedKey === stableKey(row)}
              id={rowId(row)}
              role="row"
              aria-rowindex={index + 2}
              aria-current={inspectedKey === stableKey(row) ? 'true' : undefined}
              tabindex="-1"
              style:grid-template-columns={TEMPLATE}
              onpointerdown={(event) => {
                activeKey = stableKey(row);
                onActiveKey?.(activeKey);
                gridElement?.focus();
                if (!(event.target as Element).closest('button')) onInspect?.(row);
              }}
              ondblclick={() => { if (drillable) onDrill(row); }}
            >
              <div role="gridcell"><strong>{row.label}</strong></div>
              <span role="gridcell" class="numeric" data-mono>{row.count.toLocaleString()}</span>
              <span role="gridcell" class="numeric" data-mono>{bytes(row.estimated_bytes)}</span>
              <div role="gridcell"><time datetime={row.latest_at} data-mono>{date(row.latest_at)}</time></div>
              <span role="gridcell">
                {#if drillable}
                  <Button
                    size="sm"
                    surface="soft"
                    label="Drill"
                    ariaLabel={`Drill into ${row.label}`}
                    onclick={() => onDrill(row)}
                  />
                {:else}
                  <span class="not-drillable">Not filterable</span>
                {/if}
              </span>
            </div>
          {/each}
        </div>
      </div>
      {/if}
      {#if loadingMore}<div role="row"><div role="gridcell" aria-colspan="5"><div class="group-progress" role="status">Loading more groups…</div></div></div>{/if}
    </div>
  </div>
</section>

<style>
  .group-table {
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    overflow: hidden;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
  }

  .group-header,
  .group-row {
    display: grid;
    align-items: center;
  }

  .group-grid {
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    overflow: auto;
    outline: none;
  }

  .group-grid:focus-visible {
    outline: var(--focus-ring);
    outline-offset: -2px;
  }

  .group-header {
    position: sticky;
    z-index: 1;
    top: 0;
    flex: 0 0 auto;
    min-height: 30px;
    border-bottom: 1px solid var(--border-default);
    background: var(--bg-surface);
    box-shadow: 0 1px 0 var(--hairline-sheen);
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    font-weight: 600;
    letter-spacing: 0.06em;
    text-transform: uppercase;
  }

  .numeric {
    text-align: right;
  }

  .group-header span,
  .group-row > :global(*) {
    min-width: 0;
    padding: 0 var(--space-4);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .group-body {
    position: relative;
    min-height: 200px;
    flex: 0 0 auto;
  }

  .group-spacer {
    position: relative;
    min-width: 760px;
  }

  .group-window {
    position: absolute;
    inset: 0 0 auto;
  }

  .group-row {
    height: var(--row-height);
    border-bottom: 1px solid var(--border-muted);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .group-row {
    transition: background-color 80ms ease-out;
  }

  .group-row:hover {
    background: var(--bg-surface-hover);
  }

  .group-row strong {
    color: var(--text-primary);
  }

  .group-row--active {
    box-shadow: inset 2px 0 0 var(--accent-blue);
  }

  .group-row--inspected {
    background: var(--selected-bg);
    box-shadow: inset 2px 0 0 var(--accent-blue);
  }

  .group-state {
    display: grid;
    min-height: 200px;
    place-content: center;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .group-cache-unavailable,
  .group-request-error {
    display: grid;
    min-height: 200px;
    gap: var(--space-3);
    place-content: center;
    padding: var(--space-6);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .group-cache-unavailable strong,
  .group-request-error {
    color: var(--accent-amber);
  }

  .not-drillable {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .group-progress {
    position: sticky;
    bottom: 0;
    padding: var(--space-2) var(--space-4);
    background: var(--bg-surface);
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    text-align: right;
  }
</style>
