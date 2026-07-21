<script lang="ts">
  import { Button, virtualSlice } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick, untrack } from 'svelte';

  import type { ExploreCacheUnavailable, ExploreFileFact, ExploreScrollAnchor } from '../../explore/models';
  import { RowGeometry, tableViewportHeight } from '../../theme/preferences.svelte';

  let {
    files,
    loading = false,
    loadingMore = false,
    hasMore = false,
    totalCount = undefined,
    generation = 0,
    error = '',
    unavailable = undefined,
    focusedKey = null,
    scrollAnchor = null,
    restoring = false,
    onOpenFile = undefined,
    onOpenItem = undefined,
    onActiveKey = undefined,
    onScrollAnchor = undefined,
    onLoadMore = undefined,
    onRetry = undefined
  }: {
    files: ExploreFileFact[];
    loading?: boolean;
    loadingMore?: boolean;
    hasMore?: boolean;
    totalCount?: number;
    generation?: number;
    error?: string;
    unavailable?: ExploreCacheUnavailable;
    focusedKey?: string | null;
    scrollAnchor?: ExploreScrollAnchor | null;
    restoring?: boolean;
    onOpenFile?: (file: ExploreFileFact) => void;
    onOpenItem?: (entryKey: string) => void;
    onActiveKey?: (key: string) => void;
    onScrollAnchor?: (key: string, offset: number) => void;
    onLoadMore?: () => Promise<unknown>;
    onRetry?: () => void;
  } = $props();

  const geometry = new RowGeometry();
  const rowHeight = $derived(geometry.height);
  const OVERSCAN = 6;
  let activeKey = $state<string | null>(untrack(() => focusedKey ?? files[0]?.key ?? null));
  let grid = $state<HTMLDivElement>();
  let headerElement = $state<HTMLDivElement>();
  let viewport = $state(360);
  let scrollTop = $state(0);
  let restoredAnchor = '';
  let previousRowCount = untrack(() => files.length);
  let commandScrolling = false;
  let suppressedScrollTop: number | undefined;

  const slice = $derived.by(() => {
    const height = rowHeight;
    if (height === undefined) return undefined;
    return virtualSlice({
      scrollTop, viewport, count: files.length, overscan: OVERSCAN,
      fixedHeight: height, heightOf: () => height
    });
  });
  const renderedFiles = $derived(slice ? files.slice(slice.start, slice.end) : []);
  const activeIndex = $derived(activeKey ? files.findIndex((file) => file.key === activeKey) : -1);
  const activeFile = $derived(activeIndex >= 0 ? files[activeIndex] : undefined);
  const renderedActiveFile = $derived(
    activeFile && renderedFiles.some((file) => file.key === activeFile.key) ? activeFile : undefined
  );
  const accessibilityRowCount = $derived.by(() => {
    if (loading || loadingMore || unavailable || (error && files.length === 0)) return undefined;
    if (files.length === 0 || !slice || rowHeight === undefined) return 2;
    return (totalCount ?? files.length) + 1;
  });

  $effect(() => {
    if (!focusedKey) return;
    const index = files.findIndex((file) => file.key === focusedKey);
    if (index >= 0) {
      activeKey = focusedKey;
      if (!untrack(() => restoring) || !scrollAnchor) scrollActiveIntoView(index);
    }
  });

  $effect(() => {
    const height = rowHeight;
    if (!grid || !scrollAnchor || height === undefined) return;
    const signature = `${generation}:${scrollAnchor.key}:${scrollAnchor.offset}`;
    if (signature === restoredAnchor) return;
    const index = files.findIndex((file) => file.key === scrollAnchor.key);
    if (index < 0) return;
    restoredAnchor = signature;
    const target = index * height + scrollAnchor.offset;
    const element = grid;
    afterVirtualLayout(element, files.length * height, () => {
      if (!grid || restoredAnchor !== signature) return;
      suppressedScrollTop = target;
      grid.scrollTop = target;
      scrollTop = grid.scrollTop;
      suppressedScrollTop = grid.scrollTop;
    });
  });

  $effect(() => {
    files;
    if (files.length < previousRowCount) restoredAnchor = '';
    previousRowCount = files.length;
    if (files.length === 0) activeKey = null;
    else {
      const key = untrack(() => activeKey);
      const index = key ? files.findIndex((file) => file.key === key) : -1;
      if (index < 0 && !restoring) {
        activeKey = files[0]!.key;
        onActiveKey?.(activeKey);
      }
    }
  });

  onMount(() => {
    if (grid) viewport = measuredViewport(grid);
    if (!grid || typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(() => {
      if (grid) viewport = measuredViewport(grid);
    });
    observer.observe(grid);
    if (headerElement) observer.observe(headerElement);
    return () => observer.disconnect();
  });
  onDestroy(() => geometry.destroy());

  function rowID(key: string): string {
    const encoded = key.replace(
      /[^a-zA-Z0-9_-]/g,
      (character) => `-${character.codePointAt(0)?.toString(16) ?? 'x'}-`
    );
    return `context-file-${encoded}`;
  }

  function formatBytes(value: number): string {
    if (value < 1024) return `${value} B`;
    if (value < 1024 * 1024) return `${Math.round(value / 1024)} KB`;
    return `${(value / (1024 * 1024)).toFixed(1)} MB`;
  }

  function formatTime(value: string): string {
    const date = new Date(value);
    return Number.isNaN(date.valueOf()) ? value : date.toLocaleDateString();
  }

  function measuredViewport(element: HTMLDivElement): number {
    return tableViewportHeight(
      element.clientHeight,
      headerElement?.offsetHeight || 34,
      window.innerHeight
    );
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
    requestAnimationFrame(() => afterVirtualLayout(element, expectedScrollHeight, callback));
  }

  function scrollActiveIntoView(index: number): void {
    const height = rowHeight;
    if (!grid || height === undefined) return;
    const top = index * height;
    const bottom = top + height;
    const visibleHeight = measuredViewport(grid);
    let target = grid.scrollTop;
    if (top < target) target = top;
    else if (bottom > target + visibleHeight) target = bottom - visibleHeight;
    grid.scrollTop = target;
    scrollTop = grid.scrollTop;
  }

  async function move(index: number): Promise<void> {
    if (files.length === 0 || rowHeight === undefined) return;
    commandScrolling = true;
    const next = Math.max(0, Math.min(files.length - 1, index));
    const file = files[next];
    if (!file) return;
    activeKey = file.key;
    onActiveKey?.(file.key);
    await tick();
    scrollActiveIntoView(next);
    requestAnimationFrame(() => requestAnimationFrame(() => { commandScrolling = false; }));
  }

  async function moveAcrossLoadedBoundary(index: number): Promise<void> {
    if (index < files.length) {
      await move(index);
      return;
    }
    const previousCount = files.length;
    if (!hasMore || loadingMore || !onLoadMore) {
      await move(files.length - 1);
      return;
    }
    await onLoadMore();
    await tick();
    if (files.length > previousCount) await move(Math.min(index, files.length - 1));
  }

  async function keydown(event: KeyboardEvent): Promise<void> {
    const height = rowHeight;
    if (event.target !== grid || event.metaKey || event.ctrlKey || event.altKey || files.length === 0 || height === undefined) return;
    if (event.key === 'j' || event.key === 'ArrowDown') await moveAcrossLoadedBoundary(activeIndex + 1);
    else if (event.key === 'k' || event.key === 'ArrowUp') await move(activeIndex - 1);
    else if (event.key === 'Home') await move(0);
    else if (event.key === 'End') await move(files.length - 1);
    else if (event.key === 'PageDown') await moveAcrossLoadedBoundary(activeIndex + Math.max(1, Math.floor(viewport / height)));
    else if (event.key === 'PageUp') await move(activeIndex - Math.max(1, Math.floor(viewport / height)));
    else if (event.key === 'Enter' && activeFile) onOpenFile?.(activeFile);
    else return;
    event.preventDefault();
  }

  function handleScroll(): void {
    scrollTop = grid?.scrollTop ?? 0;
    const height = rowHeight;
    if (height === undefined || !slice || restoring || commandScrolling) return;
    if (suppressedScrollTop !== undefined) {
      const programmatic = Math.abs(scrollTop - suppressedScrollTop) < 0.5;
      suppressedScrollTop = undefined;
      if (programmatic) return;
    }
    const first = Math.min(files.length - 1, Math.max(0, Math.floor(scrollTop / height)));
    const file = files[first];
    if (file) {
      const lastVisible = Math.min(
        files.length - 1,
        Math.ceil((scrollTop + viewport) / height) - 1
      );
      if (activeIndex < first || activeIndex > lastVisible) {
        activeKey = file.key;
        onActiveKey?.(file.key);
      }
      onScrollAnchor?.(file.key, scrollTop - first * height);
    }
    if (hasMore && !loadingMore && slice.end >= files.length - OVERSCAN) void onLoadMore?.();
  }
</script>

<section class="context-files" aria-label="Files presentation">
  <header><strong>Files in context</strong><span>{totalCount?.toLocaleString() ?? '—'} files</span></header>
  <div
    bind:this={grid}
    class="file-grid"
    role="grid"
    aria-label="Files in current context"
    aria-rowcount={accessibilityRowCount}
    aria-colcount="5"
    aria-busy={loading || loadingMore || restoring}
    aria-activedescendant={renderedActiveFile ? rowID(renderedActiveFile.key) : undefined}
    tabindex="0"
    onkeydown={keydown}
    onscroll={handleScroll}
  >
    <div class="file-row file-header" bind:this={headerElement} role="row">
      <span role="columnheader">Received</span><span role="columnheader">Filename</span>
      <span role="columnheader">Containing item</span><span role="columnheader">Source</span>
      <span role="columnheader">Size</span>
    </div>
    <div class="file-body" role="rowgroup">
      {#if unavailable}
        <div role="row"><div role="gridcell" aria-colspan="5"><div class="notice" role="alert"><strong>Analytical cache unavailable</strong><span>{unavailable.message}</span><Button label="Retry cache check" surface="outline" onclick={() => onRetry?.()} /></div></div></div>
      {:else if error && files.length === 0}
        <div role="row"><div role="gridcell" aria-colspan="5"><div class="notice" role="alert"><span>{error}</span><Button label="Retry request" surface="outline" onclick={() => onRetry?.()} /></div></div></div>
      {:else if loading && files.length === 0}
        <div role="row"><div role="gridcell" aria-colspan="5"><div class="notice" role="status">Loading files in this context…</div></div></div>
      {:else if files.length === 0}
        <div role="row"><div role="gridcell" aria-colspan="5"><div class="notice">No files match this view.</div></div></div>
      {:else if !slice || rowHeight === undefined}
        <div role="row"><div role="gridcell" aria-colspan="5"><div class="notice" role="status">Preparing files layout…</div></div></div>
      {:else}
        <div class="virtual-spacer" style:height={`${slice.totalHeight}px`}>
          <div class="virtual-window" style:transform={`translateY(${slice.topPad}px)`}>
            {#each renderedFiles as file, offset (file.key)}
              {@const index = slice.start + offset}
              <!-- svelte-ignore a11y_click_events_have_key_events -- Enter on
                   the focused grid opens the same file via handleKeydown. -->
              <div
                id={rowID(file.key)}
                class="file-row file-data"
                class:file-data--active={index === activeIndex}
                role="row"
                tabindex="-1"
                aria-rowindex={index + 2}
                onpointerdown={() => { activeKey = file.key; onActiveKey?.(file.key); grid?.focus(); }}
                onclick={(event) => {
                  if (!(event.target as Element).closest('button')) onOpenFile?.(file);
                }}
              >
                <span role="gridcell">{formatTime(file.occurred_at)}</span>
                <span class="filename" role="gridcell">{file.filename || 'Unnamed file'}</span>
                <span role="gridcell"><button type="button" aria-label={`Open containing item ${file.message_id}`} onclick={(event) => { event.stopPropagation(); onOpenItem?.(file.entry_key); }}>{file.title || 'Untitled item'}</button></span>
                <span role="gridcell">{file.source_identifier}</span>
                <span role="gridcell">{formatBytes(file.size)}</span>
              </div>
            {/each}
          </div>
        </div>
        {#if error}
          <div role="row"><div role="gridcell" aria-colspan="5"><div class="progress progress--error" role="alert"><span>{error}</span><Button label="Retry loading more files" surface="outline" onclick={() => onLoadMore?.()} /></div></div></div>
        {:else if loadingMore}
          <div role="row"><div role="gridcell" aria-colspan="5"><div class="progress" role="status">Loading more… {files.length.toLocaleString()} loaded</div></div></div>
        {:else if hasMore}
          <div role="row"><div role="gridcell" aria-colspan="5"><div class="progress"><Button label="Load more files" surface="outline" onclick={() => onLoadMore?.()} /></div></div></div>
        {/if}
      {/if}
    </div>
  </div>
</section>

<style>
  .context-files { display: flex; min-height: 0; flex: 1; flex-direction: column; gap: var(--space-3); }
  header { display: flex; justify-content: space-between; color: var(--text-secondary); font-size: var(--font-size-xs); }
  .file-grid { display: flex; min-height: 0; flex: 1; flex-direction: column; overflow: auto; border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-surface); outline: none; }
  .file-grid:focus-visible { box-shadow: inset 0 0 0 2px var(--focus-color); }
  .file-row { display: grid; grid-template-columns: 110px minmax(160px,1fr) minmax(180px,1.4fr) minmax(150px,1fr) 90px; align-items: center; gap: var(--space-3); min-height: var(--row-height); padding: 0 var(--space-3); }
  .file-header { position: sticky; z-index: 1; top: 0; min-height: var(--table-header-height); flex: 0 0 auto; border-bottom: 1px solid var(--border-default); background: var(--bg-surface); color: var(--text-muted); font-size: var(--font-size-2xs); font-weight: 600; text-transform: uppercase; }
  .file-body { position: relative; min-height: 220px; flex: 0 0 auto; }
  .virtual-spacer { position: relative; }
  .virtual-window { position: absolute; inset: 0 0 auto; }
  .file-data { height: var(--row-height); border-bottom: 1px solid var(--border-muted); background: transparent; color: var(--text-primary); text-align: left; cursor: default; }
  .file-data:hover { background: var(--bg-surface-hover); }
  .file-data--active { background: var(--selected-bg); }
  /* .file-data:hover (specificity 0,2,0) would otherwise always beat
     .file-data--active (0,1,0) regardless of source order, hiding the
     active/selected indicator on hover. */
  .file-data--active:hover { background: var(--selected-bg); }
  .file-data span { min-width: 0; overflow: hidden; color: var(--text-secondary); font-size: var(--font-size-xs); text-overflow: ellipsis; white-space: nowrap; }
  .file-data .filename { color: var(--text-primary); font-weight: 600; }
  .file-data button { max-width: 100%; padding: 0; overflow: hidden; border: 0; background: transparent; color: inherit; cursor: pointer; font: inherit; text-overflow: ellipsis; white-space: nowrap; }
  .file-data button:hover { color: var(--text-primary); text-decoration: underline; }
  .notice { display: flex; min-height: 120px; align-items: center; justify-content: center; gap: var(--space-3); padding: var(--space-5); color: var(--text-secondary); }
  .progress { position: sticky; bottom: 0; padding: var(--space-2); background: var(--bg-inset); text-align: center; }
  .progress--error { display: flex; align-items: center; justify-content: center; gap: var(--space-3); color: var(--text-danger); }
</style>
