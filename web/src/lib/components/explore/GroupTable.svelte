<script lang="ts">
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import { groupRowName, type SourceInventory } from '../../explore/labels';
  import { formatBytes, formatDate } from '../../util/format';
  import { Button, IconButton, MEDIA, virtualSlice } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick, untrack } from 'svelte';
  import { MediaQuery } from 'svelte/reactivity';

  import type {
    ExploreCacheUnavailable,
    ExploreGroupDimension,
    ExploreGroupRow,
    ExploreScrollAnchor
  } from '../../explore/models';
  import { rebaseVirtualScroll, RowGeometry, scrollPositionAnchor, tableViewportHeight } from '../../theme/preferences.svelte';

  let {
    sources = new Map(),
    rows,
    dimension,
    loading = false,
    loadingMore = false,
    hasMore = false,
    totalCount = undefined,
    generation = 0,
    error = '',
    pageError = '',
    unavailable = undefined,
    drillable = true,
    focusedKey = null,
    inspectedKey = null,
    scrollAnchor = null,
    restoring = false,
    workspaceLabel = 'Messages',
    onDrill,
    onInspect = undefined,
    onLoadMore = undefined,
    onLoadThroughEnd = undefined,
    onActiveKey = undefined,
    onScrollAnchor = undefined,
    onRetry = undefined
  }: {
    sources?: SourceInventory;
    rows: ExploreGroupRow[];
    dimension: ExploreGroupDimension;
    loading?: boolean;
    loadingMore?: boolean;
    hasMore?: boolean;
    totalCount?: number;
    generation?: number;
    error?: string;
    pageError?: string;
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

  const compact = new MediaQuery(MEDIA.compact);
  const geometry = new RowGeometry('--table-short-row-height');
  const rowHeight = $derived(geometry.height);
  const OVERSCAN = 6;
  let width = $state(1000);
  // The compact two-line row already fits size and latest beneath the name.
  const narrow = $derived(!compact.current && (Boolean(inspectedKey) || width < 640));
  const timeGroup = $derived((dimension === 'month' || dimension === 'year') && rows.some((row) => row.total_items !== undefined));
  const columnCount = $derived((narrow ? 3 : 5) + (timeGroup ? 1 : 0));
  const template = $derived(narrow
    ? `minmax(0,480px) 80px ${timeGroup ? '96px ' : ''}minmax(0,1fr) 40px`
    : 'minmax(200px,480px) 88px 96px 128px minmax(96px,1fr) 40px');
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
    if (loading || loadingMore || unavailable || error || pageError) return undefined;
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
    const rebased = rebaseVirtualScroll(scrollTop, previousHeight, nextHeight, activeIndex, untrack(() => viewport));
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
    gridElement.scrollTop = index * height + Math.min(scrollAnchor.offset, height - 1);
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
    width = gridElement.clientWidth;
    if (typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(() => {
      if (gridElement) { viewport = measuredViewport(gridElement); width = gridElement.clientWidth; }
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
      headerElement?.offsetHeight ?? 30,
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
    if (!restoring) persistAnchor(height);
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
    const { index } = scrollPositionAnchor(scrollTop, height, rows.length);
    const row = rows[index];
    if (row && !restoring) {
      activeKey = stableKey(row);
      onActiveKey?.(activeKey);
      persistAnchor(height);
    }
    if (!restoring && hasMore && !loadingMore && slice.end >= rows.length - OVERSCAN) void onLoadMore?.();
  }

  function persistAnchor(height: number): void {
    const { index, offset } = scrollPositionAnchor(scrollTop, height, rows.length);
    const row = rows[index];
    if (row) onScrollAnchor?.(stableKey(row), offset);
  }
</script>

<section class="group-table" aria-label={`Grouped by ${dimension}`}>
  <div
    class="group-grid"
    bind:this={gridElement}
    role="grid"
    aria-label={`${workspaceLabel} grouped by ${dimension}`}
    aria-rowcount={accessibilityRowCount}
    aria-colcount={columnCount}
    style:--group-columns={template}
    aria-busy={loading || loadingMore}
    aria-activedescendant={activeIndex >= 0 && rows[activeIndex] ? rowId(rows[activeIndex]!) : undefined}
    tabindex="0"
    onkeydown={handleKeydown}
    onscroll={handleScroll}
  >
    <div class="group-header" bind:this={headerElement} role="row">
      <span role="columnheader" class={[compact.current && 'kit-sr-only']}>Group</span>
      <span role="columnheader" class={['numeric', compact.current && 'kit-sr-only']}>Items</span>
      {#if !narrow}
        <span role="columnheader" class={['numeric', compact.current && 'kit-sr-only']}>Size</span>
        <span role="columnheader" class={[compact.current && 'kit-sr-only']}>Latest</span>
      {/if}
      {#if timeGroup}<span role="columnheader" class={[compact.current && 'kit-sr-only']}>Share</span>{/if}
      <span role="columnheader" aria-label="Show items"></span>
    </div>
    <div class="group-body" role="rowgroup">
      {#if unavailable}
        <div role="row"><div role="gridcell" aria-colspan={columnCount}><div class="group-cache-unavailable" role="alert">
        <strong>{unavailable.readiness === 'building' ? 'Preparing analytical cache' : 'Analytical cache unavailable'}</strong>
        <span>{unavailable.message}</span>
        {#if unavailable.readiness === 'building'}
          <span>This view will refresh automatically.</span>
        {:else}
          <span>Rebuild it with <code>{unavailable.recovery_action}</code>, then retry.</span>
          <div><Button label="Retry cache check" tone="info" surface="outline" onclick={() => onRetry?.()} /></div>
        {/if}
        </div></div></div>
      {:else if error}
        <div role="row"><div role="gridcell" aria-colspan={columnCount}><div class="group-request-error" role="alert">
        <span>{error}</span>
        <Button
          label={restoring ? 'Retry restoration' : 'Retry request'}
          tone="info"
          surface="outline"
          onclick={() => onRetry?.()}
        />
        </div></div></div>
      {:else if loading && rows.length === 0}
        <div role="row"><div role="gridcell" aria-colspan={columnCount}><div class="group-state" role="status">Loading grouped results…</div></div></div>
      {:else if rows.length === 0}
        <div role="row"><div role="gridcell" aria-colspan={columnCount}><div class="group-state">No groups match this view.</div></div></div>
      {:else if !slice || rowHeight === undefined}
        <div role="row"><div role="gridcell" aria-colspan={columnCount}><div class="group-state" role="status">Preparing grouped layout…</div></div></div>
      {:else}
      <div class="group-spacer" style:height={`${slice.totalHeight}px`}>
        <div class="group-window" style:transform={`translateY(${slice.topPad}px)`}>
          {#each renderedRows as row, offset (row.key)}
            {@const index = slice.start + offset}
            {@const display = groupRowName(dimension, row.key, row.label, dimension === 'source' ? sources : undefined)}
            <div
              class="group-row"
              class:group-row--active={index === activeIndex}
              class:group-row--inspected={inspectedKey === stableKey(row)}
              id={rowId(row)}
              role="row"
              aria-rowindex={index + 2}
              aria-current={inspectedKey === stableKey(row) ? 'true' : undefined}
              tabindex="-1"
              onpointerdown={(event) => {
                activeKey = stableKey(row);
                onActiveKey?.(activeKey);
                gridElement?.focus();
                if (!(event.target as Element).closest('button')) onInspect?.(row);
              }}
              ondblclick={() => { if (drillable) onDrill(row); }}
            >
              <div role="gridcell" class="group-name" title={display.title}>
                {#if typeof display.icon === 'string'}<span class="group-icon" aria-hidden="true">{display.icon}</span>
                {:else if display.icon}{@const Icon = display.icon}<Icon size={14} class="group-icon" aria-hidden="true" />{/if}
                <strong>{display.name}</strong>
              </div>
              <span role="gridcell" class="numeric group-count" data-mono>{row.count.toLocaleString()}<span class="count-label">{' '}{row.count === 1 ? 'item' : 'items'}</span></span>
              {#if !narrow}
                <span role="gridcell" class="numeric group-size" data-mono>{formatBytes(row.estimated_bytes)}</span>
                <div role="gridcell" class="group-latest"><time datetime={row.latest_at} data-mono>{formatDate(row.latest_at)}</time></div>
              {/if}
              {#if timeGroup}
                <div role="gridcell" class="group-share">
                  {#if row.total_items}
                    {@const share = row.count / row.total_items * 100}
                    <div class="share-track" title={`${Number(share.toFixed(1))}% of ${row.total_items.toLocaleString()} items`}><div class="share-fill" style:width={`${share}%`}></div></div>
                  {/if}
                </div>
              {/if}
              <span role="gridcell" class="group-action">
                {#if drillable}<IconButton size="sm" ariaLabel={`Show items in ${display.name}`} onclick={() => onDrill(row)}><ChevronRight size={14} /></IconButton>{/if}
              </span>
            </div>
          {/each}
        </div>
      </div>
      {/if}
      {#if pageError}
        <!-- A failed cursor page keeps the groups already loaded. While the
             cursor survived (a transient failure), offer a quiet retry that
             re-attempts the same page; a terminal failure dropped the
             cursor, so the only recovery is reloading the view. -->
        <div role="row"><div role="gridcell" aria-colspan={columnCount}><div class="group-page-error" role="alert">
          <span>{pageError}</span>
          {#if hasMore}
            <Button size="sm" surface="outline" label="Retry loading more" onclick={() => void onLoadMore?.()} />
          {:else}
            <Button size="sm" surface="outline" label="Reload view" onclick={() => onRetry?.()} />
          {/if}
        </div></div></div>
      {/if}
      {#if loadingMore}<div role="row"><div role="gridcell" aria-colspan={columnCount}><div class="group-progress" role="status">Loading more groups…</div></div></div>{/if}
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
    grid-template-columns: var(--group-columns);
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

  .group-header > :last-child { grid-column: -2; }

  .group-body {
    position: relative;
    min-height: 200px;
    flex: 0 0 auto;
  }

  .group-spacer {
    position: relative;
  }

  .group-window {
    position: absolute;
    inset: 0 0 auto;
  }

  .group-row {
    height: var(--table-short-row-height);
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
    border-start-start-radius: 0;
    border-end-start-radius: 0;
    box-shadow: inset 2px 0 0 var(--accent-blue);
  }

  .group-row--inspected {
    border-start-start-radius: 0;
    border-end-start-radius: 0;
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

  .group-row { cursor: pointer; }
  .group-name { display: flex; align-items: center; gap: var(--space-2); }
  .group-name strong { overflow: hidden; text-overflow: ellipsis; }
  .group-name :global(.group-icon) { flex-shrink: 0; color: var(--artifact-ink); }
  .group-row > .group-action { grid-column: -2; }
  .group-row > .group-action { display: flex; justify-content: center; padding: 0; }
  .share-track { height: 6px; border-radius: var(--radius-sm); background: var(--bg-inset); overflow: hidden; }
  .share-fill { height: 100%; background: var(--accent-blue); }

  .group-page-error {
    position: sticky;
    bottom: 0;
    display: flex;
    align-items: center;
    justify-content: center;
    gap: var(--space-3);
    padding: var(--space-2) var(--space-4);
    background: var(--bg-surface);
    color: var(--text-danger);
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
  .count-label { display: none; }
  @media (max-width: 640px) {
    .group-spacer { min-width: 0; }
    .group-header { min-height: 0; border: 0; box-shadow: none; }
    .group-row {
      grid-template-columns: auto auto auto minmax(0, 1fr) auto;
      grid-template-areas: 'label label label label action' 'count size latest share action';
      align-content: center;
      gap: var(--space-1) 0;
      padding: var(--pane-pad-y) var(--pane-pad-x);
    }
    .group-row > :global(*) { padding: 0; }
    .group-row > .group-name { grid-area: label; }
    .group-row strong { display: block; overflow: hidden; text-overflow: ellipsis; }
    .group-row > .group-count { grid-area: count; }
    .count-label { display: inline; }
    .group-row > .group-size { grid-area: size; }
    .group-row > .group-latest { grid-area: latest; }
    .group-row > .group-size::before, .group-row > .group-latest::before { content: ' · '; white-space: pre; }
    .group-row > .group-share { grid-area: share; padding-left: var(--space-2); }
    .group-row > .group-action { grid-area: action; }
    .group-row .numeric, .group-row time { color: var(--text-muted); font-size: var(--font-size-xs); text-align: left; }
    .group-row :global(button) { min-height: var(--control-height); }
  }
</style>
