<script lang="ts">
  import { virtualSlice } from '@kenn-io/kit-ui';
  import { onDestroy, onMount } from 'svelte';

  import type { RelationshipTimelineRow } from '../../relationships/models';
  import { RowGeometry, tableViewportHeight } from '../../theme/preferences.svelte';
  import { monthGroupKey, monthGroupLabel, timelineRowTitle } from './timeline-support';

  interface Props {
    rows: RelationshipTimelineRow[];
    loading?: boolean;
    loadingMore?: boolean;
    hasMore?: boolean;
    error?: string | null;
    restartNotice?: string | null;
    selectedKey?: string | null;
    onRowOpen: (row: RelationshipTimelineRow) => void;
    onLoadMore?: () => void;
  }

  type DisplayItem =
    | { type: 'header'; key: string; label: string }
    | { type: 'row'; key: string; row: RelationshipTimelineRow };

  let {
    rows,
    loading = false,
    loadingMore = false,
    hasMore = false,
    error = null,
    restartNotice = null,
    selectedKey = null,
    onRowOpen,
    onLoadMore = undefined
  }: Props = $props();

  const MONTH_HEADER_HEIGHT = 28;
  const OVERSCAN = 6;

  const geometry = new RowGeometry();
  const rowHeight = $derived(geometry.height);

  let gridElement = $state<HTMLDivElement>();
  let scrollTop = $state(0);
  let viewport = $state(360);
  let activeKey = $state<string | null>(null);

  const displayItems = $derived.by((): DisplayItem[] => {
    const items: DisplayItem[] = [];
    let lastMonth: string | null = null;
    for (const row of rows) {
      const month = monthGroupKey(row.occurred_at);
      if (month !== lastMonth) {
        items.push({ type: 'header', key: `month:${month}`, label: monthGroupLabel(row.occurred_at) });
        lastMonth = month;
      }
      items.push({ type: 'row', key: row.key, row });
    }
    return items;
  });

  $effect(() => {
    const keys = rows.map((row) => row.key);
    if (activeKey && keys.includes(activeKey)) return;
    activeKey = keys[0] ?? null;
  });

  const activeRowIndex = $derived(activeKey ? rows.findIndex((row) => row.key === activeKey) : -1);

  const slice = $derived.by(() => {
    const height = rowHeight;
    if (height === undefined) return undefined;
    return virtualSlice({
      scrollTop,
      viewport,
      count: displayItems.length,
      overscan: OVERSCAN,
      heightOf: (index) => (displayItems[index]!.type === 'header' ? MONTH_HEADER_HEIGHT : height)
    });
  });
  const renderedItems = $derived(slice ? displayItems.slice(slice.start, slice.end) : []);

  onMount(() => {
    if (!gridElement) return;
    viewport = measuredViewport(gridElement);
    if (typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(() => {
      if (gridElement) viewport = measuredViewport(gridElement);
    });
    observer.observe(gridElement);
    return () => observer.disconnect();
  });
  onDestroy(() => geometry.destroy());

  function measuredViewport(element: HTMLDivElement): number {
    return tableViewportHeight(element.clientHeight, 0, window.innerHeight);
  }

  function formatTime(value: string): string {
    const date = new Date(value);
    if (Number.isNaN(date.valueOf())) return value;
    return new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' }).format(date);
  }

  function editableTarget(target: EventTarget | null): boolean {
    const element = target as HTMLElement | null;
    return Boolean(element?.closest('input, textarea, select, [contenteditable]:not([contenteditable="false"])'));
  }

  function displayIndexOf(key: string | null): number {
    return key === null ? -1 : displayItems.findIndex((item) => item.type === 'row' && item.key === key);
  }

  function heightOfDisplayIndex(index: number): number {
    const height = rowHeight ?? 0;
    return displayItems[index]!.type === 'header' ? MONTH_HEADER_HEIGHT : height;
  }

  function scrollActiveIntoView(displayIndex: number): void {
    if (!gridElement || rowHeight === undefined || displayIndex < 0) return;
    let top = 0;
    for (let i = 0; i < displayIndex; i += 1) top += heightOfDisplayIndex(i);
    const bottom = top + heightOfDisplayIndex(displayIndex);
    const visibleHeight = measuredViewport(gridElement);
    let target = gridElement.scrollTop;
    if (top < target) target = top;
    else if (bottom > target + visibleHeight) target = bottom - visibleHeight;
    scrollTop = target;
    gridElement.scrollTop = target;
  }

  function moveTo(index: number): void {
    if (rows.length === 0) return;
    const next = Math.max(0, Math.min(rows.length - 1, index));
    activeKey = rows[next]!.key;
    scrollActiveIntoView(displayIndexOf(activeKey));
  }

  // Enter opens the reading pane. Home/End jump within already-loaded rows
  // only — unlike EverythingTable's End, this never triggers a walk-to-end
  // fetch; growth happens solely via scroll-proximity onLoadMore below.
  function handleKeydown(event: KeyboardEvent): void {
    if (editableTarget(event.target) || rows.length === 0) return;
    if (event.metaKey || event.ctrlKey || event.altKey) return;
    if (event.key === 'j' || event.key === 'ArrowDown') moveTo(activeRowIndex + 1);
    else if (event.key === 'k' || event.key === 'ArrowUp') moveTo(activeRowIndex - 1);
    else if (event.key === 'Home') moveTo(0);
    else if (event.key === 'End') moveTo(rows.length - 1);
    else if (event.key === 'Enter' && activeRowIndex >= 0) onRowOpen(rows[activeRowIndex]!);
    else return;
    event.preventDefault();
  }

  function handleScroll(): void {
    scrollTop = gridElement?.scrollTop ?? 0;
    if (!slice || !hasMore || loadingMore) return;
    if (slice.end >= displayItems.length - OVERSCAN) onLoadMore?.();
  }

  function selectRow(row: RelationshipTimelineRow): void {
    activeKey = row.key;
    onRowOpen(row);
  }
</script>

<section class="relationship-timeline" aria-label="Relationship timeline">
  {#if restartNotice}<p class="restart-notice" role="status">{restartNotice}</p>{/if}
  {#if error}<p class="timeline-error" role="alert">{error}</p>{/if}
  <div
    class="timeline-grid"
    role="grid"
    aria-label="Relationship activity"
    aria-busy={loading || loadingMore}
    tabindex="0"
    bind:this={gridElement}
    onkeydown={handleKeydown}
    onscroll={handleScroll}
  >
    <div class="timeline-body" role="rowgroup">
      {#if loading && rows.length === 0}
        <div role="row"><div role="gridcell"><p role="status">Loading activity…</p></div></div>
      {:else if rows.length === 0}
        <div role="row"><div role="gridcell"><p role="status">No activity yet.</p></div></div>
      {:else if !slice || rowHeight === undefined}
        <div role="row"><div role="gridcell"><p role="status">Preparing timeline layout…</p></div></div>
      {:else}
        <div class="virtual-spacer" style:height={`${slice.totalHeight}px`}>
          <div class="virtual-window" style:transform={`translateY(${slice.topPad}px)`}>
            {#each renderedItems as item (item.key)}
              {#if item.type === 'header'}
                <div class="month-header" role="row" style:height={`${MONTH_HEADER_HEIGHT}px`}>
                  <span role="gridcell" aria-label={`Month: ${item.label}`}>{item.label}</span>
                </div>
              {:else}
                <div
                  class="timeline-row"
                  class:active={item.key === activeKey}
                  class:selected={item.key === selectedKey}
                  role="row"
                  tabindex="-1"
                  data-row-key={item.key}
                  aria-selected={item.key === selectedKey}
                  style:height={`${rowHeight}px`}
                  onpointerdown={() => selectRow(item.row)}
                  ondblclick={() => selectRow(item.row)}
                >
                  <div role="gridcell">
                    <strong>{timelineRowTitle(item.row)}</strong>
                    <span class="preview">{item.row.preview}</span>
                    <time datetime={item.row.occurred_at}>{formatTime(item.row.occurred_at)}</time>
                  </div>
                </div>
              {/if}
            {/each}
          </div>
        </div>
      {/if}
      {#if loadingMore}
        <div role="row"><div role="gridcell"><p role="status">Loading more…</p></div></div>
      {/if}
    </div>
  </div>
</section>

<style>
  .relationship-timeline {
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    gap: var(--space-2);
  }

  .restart-notice,
  .timeline-error {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    border-radius: var(--radius-sm);
    font-size: var(--font-size-xs);
  }

  .restart-notice {
    background: var(--bg-inset);
    color: var(--text-secondary);
  }

  .timeline-error {
    background: color-mix(in srgb, var(--accent-red) 10%, transparent);
    color: var(--text-primary);
  }

  .timeline-grid {
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    overflow: auto;
    outline: none;
  }

  .timeline-grid:focus-visible {
    outline: var(--focus-ring);
    outline-offset: -2px;
  }

  .timeline-body {
    position: relative;
    flex: 0 0 auto;
  }

  .virtual-spacer {
    position: relative;
  }

  .virtual-window {
    position: absolute;
    inset: 0 0 auto;
  }

  .month-header {
    display: flex;
    align-items: center;
    padding: 0 var(--space-3);
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    font-weight: 700;
    letter-spacing: 0.04em;
    text-transform: uppercase;
  }

  .timeline-row {
    display: flex;
    flex-direction: column;
    justify-content: center;
    gap: 2px;
    border-bottom: 1px solid var(--border-muted);
    cursor: default;
  }

  .timeline-row [role='gridcell'] {
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding: 0 var(--space-3);
  }

  .timeline-row:hover {
    background: var(--bg-surface-hover);
  }

  .timeline-row.active {
    box-shadow: inset 3px 0 var(--accent-teal);
  }

  .timeline-row.selected {
    outline: 2px solid var(--accent-blue);
    outline-offset: -2px;
  }

  .timeline-row strong {
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .timeline-row .preview {
    overflow: hidden;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .timeline-row time {
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
  }
</style>
