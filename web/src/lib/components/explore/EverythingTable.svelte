<script lang="ts">
  import { Button, virtualSlice } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick, untrack } from 'svelte';

  import type {
    EntryRow,
    ExploreCacheUnavailable,
    ExploreColumn,
    ExploreScrollAnchor
  } from '../../explore/models';
  import { DEFAULT_EXPLORE_COLUMNS } from '../../explore/models';
  import type { ExploreSelectionState } from '../../explore/state.svelte';
  import { rebaseVirtualScroll, RowGeometry, tableViewportHeight } from '../../theme/preferences.svelte';
  import RowKind from './RowKind.svelte';

  interface Props {
    rows: EntryRow[];
    selection: ExploreSelectionState;
    columns?: ExploreColumn[];
    columnWidths?: Partial<Record<ExploreColumn, number>>;
    loading?: boolean;
    loadingMore?: boolean;
    hasMore?: boolean;
    totalCount?: number;
    generation?: number;
    error?: string;
    unavailable?: ExploreCacheUnavailable;
    focusedKey?: string | null;
    inspectedKey?: string | null;
    scrollAnchor?: ExploreScrollAnchor | null;
    restoring?: boolean;
    onOpen?: (row: EntryRow) => void;
    onColumnsChange?: (columns: ExploreColumn[]) => void;
    onScrollAnchor?: (key: string, offset: number) => void;
    onLoadMore?: () => Promise<unknown>;
    onLoadThroughEnd?: () => Promise<void>;
    onActiveKey?: (key: string) => void;
    onVisibleRows?: (rowKeys: string[]) => void;
    onRetry?: () => void;
  }

  let {
    rows,
    selection,
    columns: providedColumns = DEFAULT_EXPLORE_COLUMNS,
    columnWidths = {},
    loading = false,
    loadingMore = false,
    hasMore = false,
    totalCount = undefined,
    generation = 0,
    error = '',
    unavailable = undefined,
    focusedKey = null,
    inspectedKey = null,
    scrollAnchor = null,
    restoring = false,
    onOpen = undefined,
    onColumnsChange = undefined,
    onScrollAnchor = undefined,
    onLoadMore = undefined,
    onLoadThroughEnd = undefined,
    onActiveKey = undefined,
    onVisibleRows = undefined,
    onRetry = undefined
  }: Props = $props();

  const geometry = new RowGeometry();
  const rowHeight = $derived(geometry.height);
  const OVERSCAN = 6;
  const ALL_COLUMNS: Array<{ id: ExploreColumn; label: string }> = [
    { id: 'kind', label: 'Kind' },
    { id: 'people', label: 'People / source' },
    { id: 'title', label: 'Subject / title' },
    { id: 'excerpt', label: 'Excerpt' },
    { id: 'time', label: 'Time' },
    { id: 'attachments', label: 'Attachments' },
    { id: 'size', label: 'Size' }
  ];

  let gridElement = $state<HTMLDivElement>();
  let headerElement = $state<HTMLDivElement>();
  let scrollTop = $state(0);
  let viewport = $state(360);
  let activeKey = $state<string | null>(untrack(() => focusedKey ?? rows[0]?.key ?? null));
  let visibleColumns = $state<ExploreColumn[]>(untrack(() => [...providedColumns]));
  let restoredAnchor = '';
  let previousRowCount = untrack(() => rows.length);
  let suppressedScrollTop: number | undefined;
  let densityRebasing = false;
  let commandScrolling = false;
  let scrollIntent = 0;
  let visibleRowSignature = '';
  let previousRowHeight = untrack(() => rowHeight);

  $effect(() => {
    const nextHeight = rowHeight;
    const previousHeight = previousRowHeight;
    previousRowHeight = nextHeight;
    if (!gridElement || nextHeight === undefined || previousHeight === undefined || previousHeight === nextHeight) return;
    const element = gridElement;
    densityRebasing = true;
    scrollIntent += 1;
    const preservedKey = activeKey;
    const focusedIndex = activeKey ? rows.findIndex((row) => row.key === activeKey) : -1;
    const rebased = focusedIndex >= 0
      ? focusedIndex * nextHeight
      : rebaseVirtualScroll(scrollTop, previousHeight, nextHeight);
    const expectedScrollHeight = rows.length * nextHeight;
    requestAnimationFrame(() => applyDensityRebase(
      element, nextHeight, expectedScrollHeight, rebased, preservedKey
    ));
  });

  $effect(() => {
    visibleColumns = [...providedColumns];
  });

  $effect(() => {
    if (!focusedKey) return;
    const index = rows.findIndex((row) => row.key === focusedKey);
    if (index >= 0) {
      const isRestoring = untrack(() => restoring);
      activeKey = focusedKey;
      if (!isRestoring || !scrollAnchor) scrollActiveIntoView(index);
      if (isRestoring && gridElement) suppressedScrollTop = gridElement.scrollTop;
    }
  });

  $effect(() => {
    const height = rowHeight;
    if (!gridElement || !scrollAnchor || height === undefined) return;
    const signature = anchorSignature(scrollAnchor);
    if (signature === restoredAnchor) return;
    const index = rows.findIndex((row) => row.key === scrollAnchor.key);
    if (index < 0) return;
    const element = gridElement;
    const target = index * height + scrollAnchor.offset;
    const intent = scrollIntent;
    restoredAnchor = signature;
    afterVirtualLayout(element, rows.length * height, () => {
      if (gridElement !== element || restoredAnchor !== signature || scrollIntent !== intent) return;
      element.scrollTop = target;
      scrollTop = element.scrollTop;
      suppressedScrollTop = element.scrollTop;
    });
  });

  $effect(() => {
    rows;
    if (rows.length < previousRowCount) restoredAnchor = '';
    previousRowCount = rows.length;
    if (rows.length === 0) activeKey = null;
    else {
      const key = untrack(() => activeKey);
      const index = key ? rows.findIndex((row) => row.key === key) : -1;
      const anchorWasRestored = scrollAnchor !== null &&
        restoredAnchor === anchorSignature(scrollAnchor);
      if (index < 0 && !restoring) {
        activeKey = rows[0]!.key;
        onActiveKey?.(activeKey);
      }
      else if (!anchorWasRestored) scrollActiveIntoView(index);
    }
  });

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
    const keys = renderedRows.filter((row) => row.kind === 'conversation').map((row) => row.key);
    const signature = keys.join('\u0000');
    if (signature === visibleRowSignature) return;
    visibleRowSignature = signature;
    onVisibleRows?.(keys);
  });
  const activeIndex = $derived(activeKey ? rows.findIndex((row) => row.key === activeKey) : -1);
  const activeRow = $derived(activeIndex >= 0 ? rows[activeIndex] : undefined);
  const template = $derived(
    visibleColumns
      .map((column) => {
        const configured = columnWidths[column];
        if (configured !== undefined) return `${configured}px`;
        if (column === 'kind') return 'minmax(86px, 0.7fr)';
        if (column === 'people') return 'minmax(150px, 1.2fr)';
        if (column === 'title') return 'minmax(180px, 1.5fr)';
        if (column === 'excerpt') return 'minmax(220px, 2fr)';
        if (column === 'time') return '112px';
        if (column === 'attachments') return '42px';
        return '76px';
      })
      .join(' ')
  );

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

  function rowId(row: EntryRow): string {
    const encoded = row.key.replace(
      /[^a-zA-Z0-9_-]/g,
      (character) => `-${character.codePointAt(0)?.toString(16) ?? 'x'}-`
    );
    return `everything-row-${encoded}`;
  }

  function anchorSignature(anchor: ExploreScrollAnchor): string {
    return `${generation}:${anchor.key}:${anchor.offset}:${rows.length}`;
  }

  function people(row: EntryRow): string {
    const labels = row.participant_labels ?? [];
    return labels.length > 0 ? labels.join(', ') : row.source_identifier;
  }

  function formatTime(value: string): string {
    const date = new Date(value);
    if (Number.isNaN(date.valueOf())) return value;
    return new Intl.DateTimeFormat(undefined, {
      month: 'short',
      day: 'numeric',
      year: date.getFullYear() === new Date().getFullYear() ? undefined : 'numeric'
    }).format(date);
  }

  function formatBytes(value: number): string {
    if (value < 1024) return `${value} B`;
    if (value < 1024 * 1024) return `${Math.round(value / 1024)} KB`;
    return `${(value / (1024 * 1024)).toFixed(1)} MB`;
  }

  function scrollActiveIntoView(index: number): void {
    const height = rowHeight;
    if (!gridElement || height === undefined) return;
    const top = index * height;
    const bottom = top + height;
    const visibleHeight = measuredViewport(gridElement);
    let target = gridElement.scrollTop;
    if (top < target) target = top;
    else if (bottom > target + visibleHeight) target = bottom - visibleHeight;
    scrollTop = target;
    gridElement.scrollTop = target;
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
    afterVirtualLayout(element, expectedScrollHeight, () => {
      if (gridElement !== element || previousRowHeight !== height) return;
      suppressedScrollTop = rebased;
      element.scrollTop = rebased;
      scrollTop = element.scrollTop;
      suppressedScrollTop = element.scrollTop;
      if (preservedKey && rows.some((row) => row.key === preservedKey)) {
        activeKey = preservedKey;
        onActiveKey?.(preservedKey);
      }
      requestAnimationFrame(() => {
        if (previousRowHeight === height) densityRebasing = false;
      });
    });
  }

  function afterVirtualLayout(
    element: HTMLDivElement,
    expectedScrollHeight: number,
    callback: () => void
  ): void {
    if (!element.isConnected) return;
    if (element.scrollHeight === 0 || (
      element.scrollHeight >= expectedScrollHeight && element.clientHeight <= window.innerHeight
    )) {
      callback();
      return;
    }
    requestAnimationFrame(() => afterVirtualLayout(
      element, expectedScrollHeight, callback
    ));
  }

  function waitForVirtualLayout(element: HTMLDivElement, expectedScrollHeight: number): Promise<void> {
    return new Promise((resolve) => afterVirtualLayout(element, expectedScrollHeight, resolve));
  }

  async function moveTo(index: number): Promise<void> {
    const height = rowHeight;
    if (rows.length === 0 || height === undefined) return;
    if (index >= rows.length && hasMore && !loadingMore) await onLoadMore?.();
    commandScrolling = true;
    scrollIntent += 1;
    const nextIndex = Math.max(0, Math.min(rows.length - 1, index));
    const nextKey = rows[nextIndex]?.key ?? null;
    activeKey = nextKey;
    if (activeKey) onActiveKey?.(activeKey);
    await tick();
    if (!gridElement || gridElement.scrollHeight === 0) commandScrolling = false;
    else requestAnimationFrame(() => requestAnimationFrame(() => {
      commandScrolling = false;
    }));
    if (gridElement) await waitForVirtualLayout(gridElement, rows.length * height);
    scrollActiveIntoView(nextIndex);
    activeKey = nextKey;
    if (activeKey) onActiveKey?.(activeKey);
    await tick();
  }

  function editableTarget(target: EventTarget | null): boolean {
    const element = target as HTMLElement | null;
    return Boolean(
      element?.closest('input, textarea, select, [contenteditable]:not([contenteditable="false"]), iframe')
    );
  }

  async function handleKeydown(event: KeyboardEvent): Promise<void> {
    const height = rowHeight;
    if (event.target !== gridElement || editableTarget(event.target) || rows.length === 0 || height === undefined) return;
    if (event.metaKey || event.ctrlKey || event.altKey) return;
    if (event.key === 'j' || event.key === 'ArrowDown') await moveTo(activeIndex + 1);
    else if (event.key === 'k' || event.key === 'ArrowUp') await moveTo(activeIndex - 1);
    else if (event.key === 'Home') await moveTo(0);
    else if (event.key === 'End') {
      if (hasMore) await onLoadThroughEnd?.();
      await moveTo(rows.length - 1);
    }
    else if (event.key === 'PageDown') await moveTo(activeIndex + Math.max(1, Math.floor(viewport / height)));
    else if (event.key === 'PageUp') await moveTo(activeIndex - Math.max(1, Math.floor(viewport / height)));
    else if (event.key === ' ' && activeRow) {
      selection.toggle(activeRow.key, activeIndex, rows.map((row) => row.key), event.shiftKey);
    } else if (event.key.toLowerCase() === 'a') {
      const firstVisible = Math.max(0, Math.ceil(scrollTop / height));
      const lastVisible = Math.min(
        rows.length - 1,
        Math.ceil((scrollTop + viewport) / height) - 1
      );
      selection.selectVisible(rows.slice(firstVisible, lastVisible + 1).map((row) => row.key));
    } else if (event.key === 'x') {
      selection.clear();
    } else if (event.key === 'Enter' && activeRow) {
      onOpen?.(activeRow);
    } else return;
    event.preventDefault();
  }

  function handleScroll(): void {
    scrollTop = gridElement?.scrollTop ?? 0;
    const height = rowHeight;
    if (height === undefined || !slice) return;
    if (densityRebasing || commandScrolling) return;
    if (suppressedScrollTop !== undefined) {
      const isProgrammaticRestoration = Math.abs(scrollTop - suppressedScrollTop) < 0.5;
      suppressedScrollTop = undefined;
      if (isProgrammaticRestoration) return;
    }
    const first = Math.min(rows.length - 1, Math.max(0, Math.floor(scrollTop / height)));
    const row = rows[first];
    if (row && !restoring) {
      const lastVisible = Math.min(
        rows.length - 1,
        Math.ceil((scrollTop + viewport) / height) - 1
      );
      if (activeIndex < first || activeIndex > lastVisible) {
        activeKey = row.key;
        onActiveKey?.(row.key);
      }
      onScrollAnchor?.(row.key, scrollTop - first * height);
    }
    if (!restoring && hasMore && !loadingMore && slice.end >= rows.length - OVERSCAN) void onLoadMore?.();
  }

  function toggleColumn(column: ExploreColumn): void {
    const next = visibleColumns.includes(column)
      ? visibleColumns.filter((item) => item !== column)
      : ALL_COLUMNS.map(({ id }) => id).filter(
          (id) => visibleColumns.includes(id) || id === column
        );
    visibleColumns = next.length > 0 ? next : ['title'];
    onColumnsChange?.([...visibleColumns]);
  }
</script>

<section class="everything-table" aria-label="Everything table">
  <div class="table-tools">
    <details>
      <summary>Columns</summary>
      <div class="column-picker kit-popover-card">
        {#each ALL_COLUMNS as column (column.id)}
          <label>
            <input
              type="checkbox"
              checked={visibleColumns.includes(column.id)}
              onchange={() => toggleColumn(column.id)}
            />
            {column.label === 'Size' ? 'Size' : column.label}
          </label>
        {/each}
      </div>
    </details>
  </div>

  <div
    class="table-grid"
    bind:this={gridElement}
    role="grid"
    aria-label="Everything results"
    aria-rowcount={accessibilityRowCount}
    aria-colcount={visibleColumns.length}
    aria-busy={loading || loadingMore}
    aria-activedescendant={activeRow ? rowId(activeRow) : undefined}
    tabindex="0"
    onkeydown={handleKeydown}
    onscroll={handleScroll}
  >
    <div class="table-header" bind:this={headerElement} role="row" style:grid-template-columns={template}>
      {#each visibleColumns as column (column)}
        <span role="columnheader" aria-label={column === 'attachments' ? 'Attachments' : undefined}>
          {column === 'attachments' ? '⌕' : ALL_COLUMNS.find((entry) => entry.id === column)?.label}
        </span>
      {/each}
    </div>
    <div
      class="table-body"
      role="rowgroup"
    >
      {#if unavailable}
        <div role="row"><div role="gridcell" aria-colspan={visibleColumns.length}><div class="cache-unavailable" role="alert">
            <strong>Analytical cache unavailable</strong>
            <span>{unavailable.message}</span>
            <span>Rebuild it with <code>{unavailable.recovery_action}</code>, then retry.</span>
            <div><Button label="Retry cache check" tone="info" surface="outline" onclick={() => onRetry?.()} /></div>
          </div>
        </div>
        </div>
      {:else if error}
        <div role="row"><div role="gridcell" aria-colspan={visibleColumns.length}><div class="request-error" role="alert">
            <span>{error}</span>
            <Button
              label={restoring ? 'Retry restoration' : 'Retry request'}
              tone="info"
              surface="outline"
              onclick={() => onRetry?.()}
            />
          </div>
        </div>
        </div>
      {:else if loading && rows.length === 0}
        <div class="skeletons">
          {#each { length: 10 } as _, index (index)}
            <div
              class="skeleton-row"
              data-testid="everything-skeleton"
              role="row"
              style:grid-template-columns={template}
            >
              {#each visibleColumns as column (column)}
                <span class="skeleton-cell" role="gridcell"><i></i></span>
              {/each}
            </div>
          {/each}
        </div>
      {:else if rows.length === 0}
        <div role="row"><div class="empty" role="gridcell" aria-colspan={visibleColumns.length}>No items match this view.</div></div>
      {:else if !slice || rowHeight === undefined}
        <div role="row"><div role="gridcell" aria-colspan={visibleColumns.length}><p class="empty" role="status">Preparing table layout…</p></div></div>
      {:else}
        <div class="virtual-spacer" style:height={`${slice.totalHeight}px`}>
          <div class="virtual-window" style:transform={`translateY(${slice.topPad}px)`}>
            {#each renderedRows as row, offset (row.key)}
              {@const index = slice.start + offset}
              <div
                class="data-row"
                class:data-row--active={index === activeIndex}
                class:data-row--selected={selection.isSelected(row.key)}
                class:data-row--inspected={inspectedKey === row.key}
                id={rowId(row)}
                data-row-key={row.key}
                role="row"
                tabindex="-1"
                aria-rowindex={index + 2}
                aria-selected={selection.isSelected(row.key)}
                aria-current={inspectedKey === row.key ? 'true' : undefined}
                style:grid-template-columns={template}
                onpointerdown={() => {
                  activeKey = row.key;
                  onActiveKey?.(row.key);
                  gridElement?.focus();
                }}
                ondblclick={() => onOpen?.(row)}
              >
                {#each visibleColumns as column (column)}
                  <span class={`cell cell--${column}`} role="gridcell">
                    {#if column === 'kind'}
                      {#if selection.isSelected(row.key)}
                        <span class="selection-marker" aria-hidden="true">✓</span>
                      {/if}
                      <RowKind kind={row.kind} messageType={row.message_type} />
                    {:else if column === 'people'}
                      {people(row)}
                    {:else if column === 'title'}
                      <strong>{row.title || '(untitled)'}</strong>
                    {:else if column === 'excerpt'}
                      {row.match.strongest_excerpt || row.preview}
                      {#if row.match.lexical_match_count !== undefined}
                        <span class="match-count">{row.match.lexical_match_count} lexical matches</span>
                      {/if}
                    {:else if column === 'time'}
                      <time datetime={row.occurred_at}>{formatTime(row.occurred_at)}</time>
                    {:else if column === 'attachments'}
                      {#if row.has_attachments}
                        <span class="attachment" aria-label={`${row.attachment_count} attachments`}>⌕</span>
                      {:else}
                        <span aria-label="No attachments">—</span>
                      {/if}
                    {:else if column === 'size'}
                      {formatBytes(row.attachment_size)}
                    {/if}
                  </span>
                {/each}
              </div>
            {/each}
          </div>
        </div>
      {/if}
      {#if loadingMore}
        <div role="row"><div role="gridcell" aria-colspan={visibleColumns.length}>
          <div class="page-progress" role="status">Loading more… {rows.length.toLocaleString()} loaded</div>
        </div></div>
      {/if}
    </div>
  </div>
</section>

<style>
  .everything-table {
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    overflow: hidden;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    box-shadow: var(--shadow-sm);
  }

  .table-tools {
    display: flex;
    min-height: 30px;
    align-items: center;
    justify-content: flex-end;
    padding: 0 var(--space-4);
    border-bottom: 1px solid var(--border-muted);
    background: color-mix(in srgb, var(--bg-surface) 90%, var(--accent-amber));
  }

  details {
    position: relative;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }

  summary {
    cursor: pointer;
  }

  .column-picker {
    position: absolute;
    z-index: var(--z-popover);
    top: 24px;
    right: 0;
    display: grid;
    width: 176px;
    gap: var(--space-3);
    padding: var(--space-4);
  }

  .column-picker label {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    white-space: nowrap;
  }

  .table-header,
  .data-row {
    display: grid;
    align-items: center;
  }

  .table-grid {
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    overflow: auto;
    overflow-anchor: none;
    outline: none;
  }

  .table-grid:focus-visible {
    outline: var(--focus-ring);
    outline-offset: -2px;
  }

  .table-header {
    position: sticky;
    z-index: 1;
    top: 0;
    flex: 0 0 auto;
    min-height: 30px;
    border-bottom: 1px solid var(--border-default);
    background: var(--bg-surface);
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    font-weight: 700;
    letter-spacing: 0.04em;
    text-transform: uppercase;
  }

  .table-header span,
  .cell {
    min-width: 0;
    padding: 0 var(--space-4);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .table-body {
    position: relative;
    min-height: 200px;
    flex: 0 0 auto;
  }

  .virtual-spacer {
    position: relative;
    min-width: 900px;
  }

  .virtual-window {
    position: absolute;
    inset: 0 0 auto;
  }

  .data-row,
  .skeleton-row {
    height: var(--row-height);
    border-bottom: 1px solid var(--border-muted);
  }

  .data-row {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-variant-numeric: tabular-nums;
    cursor: default;
  }

  .data-row:nth-child(even) {
    background: color-mix(in srgb, var(--bg-inset) 45%, transparent);
  }

  .data-row:hover {
    background: var(--bg-surface-hover);
  }

  .data-row--active {
    box-shadow: inset 3px 0 var(--accent-teal);
  }

  .data-row--selected {
    background: color-mix(in srgb, var(--accent-teal) 12%, var(--bg-surface));
    box-shadow: inset 0 0 0 1px var(--selected-border);
  }

  .data-row--inspected {
    outline: 1px solid var(--artifact-ink);
    outline-offset: -1px;
  }

  .cell--title strong {
    color: var(--text-primary);
    font-weight: 600;
  }

  .cell--time,
  .cell--attachments,
  .cell--size {
    color: var(--text-muted);
  }

  .match-count {
    margin-left: var(--space-3);
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
  }

  .cell--attachments {
    text-align: center;
  }

  .attachment {
    color: var(--artifact-ink);
  }

  .selection-marker {
    margin-right: var(--space-2);
    color: var(--active-ink);
    font-weight: 800;
  }

  .empty {
    margin: 0;
    padding: var(--space-8);
    color: var(--text-muted);
    font-size: var(--font-size-sm);
    text-align: center;
  }

  .skeletons {
    min-height: 200px;
  }

  .skeleton-row {
    display: grid;
    align-items: center;
  }

  .skeleton-cell {
    min-width: 0;
    padding: 0 var(--space-4);
  }

  .skeleton-cell i {
    display: block;
    height: 10px;
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
  }

  .cache-unavailable {
    display: grid;
    min-height: 200px;
    place-content: center;
    gap: var(--space-4);
    padding: var(--space-7);
    border-left: 4px solid var(--accent-amber);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .cache-unavailable strong {
    color: var(--text-primary);
    font-size: var(--font-size-lg);
  }

  .request-error {
    display: grid;
    min-height: 200px;
    place-content: center;
    padding: var(--space-7);
    border-left: 4px solid var(--accent-red);
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }

  .page-progress {
    position: sticky;
    bottom: 0;
    padding: var(--space-2) var(--space-4);
    background: var(--bg-surface);
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    text-align: right;
  }

  code {
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-primary);
    font-family: var(--font-mono);
  }

  @media (prefers-reduced-motion: reduce) {
    *,
    *::before,
    *::after {
      scroll-behavior: auto !important;
    }
  }
</style>
