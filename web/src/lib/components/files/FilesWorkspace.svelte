<script lang="ts">
  import { Button, SearchInput, virtualSlice } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import { analyticalAuthority } from '../../explore/authority';
  import type { ExplorePredicate, FileMIMEFamily, FileSearchResponse, FileSearchRow, FileSearchSort } from '../../explore/models';
  import { rebaseVirtualScroll, RowGeometry, tableViewportHeight } from '../../theme/preferences.svelte';
  import FileViewer from './FileViewer.svelte';

  type IdentityFileScope =
    | { kind: 'person'; id: number }
    | { kind: 'domain'; domain: string };

  interface PendingRestoration {
    generation: number;
    epoch: number;
    activeKey: string | null;
  }

  interface Props {
    client: APIClient;
    predicate: ExplorePredicate;
    identityScope?: IdentityFileScope;
    expectedAuthority?: string;
    sort: FileSearchSort;
    filenameQuery?: string;
    mimeFamilies?: FileMIMEFamily[];
    activeKey?: string | null;
    selectedKey?: string | null;
    restorationEpoch?: number;
    onSortChange?: (sort: FileSearchSort) => void;
    onActiveKey?: (key: string) => void;
    onSelectedKey?: (key: string | null) => void;
    onFilenameQueryChange?: (query: string) => void;
    onMIMEFamiliesChange?: (families: FileMIMEFamily[]) => void;
    onRestorationComplete?: (epoch: number) => void;
    onOpenItem?: (entryKey: string) => void;
    onOpenConversation?: (entryKey: string, messageID: number, conversationID: number) => void;
  }

  let {
    client,
    predicate,
    identityScope = undefined,
    expectedAuthority = undefined,
    sort,
    filenameQuery = '',
    mimeFamilies = [],
    activeKey: providedActiveKey = null,
    selectedKey = null,
    restorationEpoch = undefined,
    onSortChange = undefined,
    onActiveKey = undefined,
    onSelectedKey = undefined,
    onFilenameQueryChange = undefined,
    onMIMEFamiliesChange = undefined,
    onRestorationComplete = undefined,
    onOpenItem = undefined,
    onOpenConversation = undefined
  }: Props = $props();

  const geometry = new RowGeometry();
  const rowHeight = $derived(geometry.height);
  const OVERSCAN = 6;
  let rows = $state<FileSearchRow[]>([]);
  let totalCount = $state(0);
  let nextCursor = $state<string>();
  let loading = $state(false);
  let loadingMore = $state(false);
  let error = $state('');
  let pageError = $state('');
  let unavailable = $state('');
  let grid = $state<HTMLDivElement>();
  let headerElement = $state<HTMLDivElement>();
  let viewport = $state(360);
  let scrollTop = $state(0);
  let activeKey = $state<string | null>(untrack(() => providedActiveKey));
  let viewerFile = $state<FileSearchRow>();
  let viewerReturnFocus = $state<HTMLElement>();
  let controller: AbortController | undefined;
  let generation = 0;
  let cacheRevision = '';
  let pageAuthority = '';
  let seenCursors = new Set<string>();
  let requestSignature = '';
  let previousRowHeight = untrack(() => rowHeight);
  let pendingRestoration = $state<PendingRestoration>();
  let completingRestoration = '';

  $effect(() => {
    const signature = JSON.stringify({ predicate, identityScope, expectedAuthority, sort, filenameQuery, mimeFamilies, restorationEpoch });
    signature;
    if (signature === requestSignature) return;
    requestSignature = signature;
    const currentGeneration = ++generation;
    controller?.abort();
    controller = new AbortController();
    rows = [];
    totalCount = 0;
    nextCursor = undefined;
    cacheRevision = '';
    pageAuthority = '';
    seenCursors = new Set<string>();
    error = '';
    pageError = '';
    unavailable = '';
    pendingRestoration = undefined;
    completingRestoration = '';
    loading = true;
    void loadPage(currentGeneration, undefined, controller.signal).then(() =>
      restoreDeepState(currentGeneration, restorationEpoch, controller?.signal));
  });

  $effect(() => {
    if (providedActiveKey && rows.some((row) => row.key === providedActiveKey)) activeKey = providedActiveKey;
  });

  $effect(() => {
    if (selectedKey) {
      const file = rows.find((row) => row.key === selectedKey);
      if (file) viewerFile = file;
    }
  });

  const slice = $derived.by(() => {
    const height = rowHeight;
    if (height === undefined) return undefined;
    return virtualSlice({
      scrollTop, viewport, count: rows.length, overscan: OVERSCAN,
      fixedHeight: height, heightOf: () => height
    });
  });
  const renderedRows = $derived(slice ? rows.slice(slice.start, slice.end) : []);
  const accessibilityRowCount = $derived.by(() => {
    if (loading || loadingMore || unavailable || error) return undefined;
    if (rows.length === 0 || !slice || rowHeight === undefined) return 2;
    return totalCount + 1;
  });
  const activeIndex = $derived(activeKey ? rows.findIndex((row) => row.key === activeKey) : -1);
  const activeRow = $derived(activeIndex >= 0 ? rows[activeIndex] : rows[0]);
  const renderedActiveRow = $derived(
    activeRow && renderedRows.some((row) => row.key === activeRow.key) ? activeRow : undefined
  );

  $effect(() => {
    const pending = pendingRestoration;
    const height = rowHeight;
    if (!pending || height === undefined) return;
    const signature = `${pending.generation}:${pending.epoch}`;
    if (completingRestoration === signature) return;
    completingRestoration = signature;
    void completeDeepRestoration(pending, height, signature);
  });

  $effect(() => {
    const nextHeight = rowHeight;
    const previousHeight = previousRowHeight;
    previousRowHeight = nextHeight;
    if (!grid || nextHeight === undefined || previousHeight === undefined || previousHeight === nextHeight) return;
    const element = grid;
    const rebased = activeIndex >= 0
      ? activeIndex * nextHeight
      : rebaseVirtualScroll(scrollTop, previousHeight, nextHeight);
    const expectedScrollHeight = rows.length * nextHeight;
    requestAnimationFrame(() => applyDensityRebase(
      element, nextHeight, expectedScrollHeight, rebased
    ));
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
  onDestroy(() => { controller?.abort(); geometry.destroy(); });

  async function loadPage(currentGeneration: number, cursor: string | undefined, signal: AbortSignal): Promise<boolean> {
    if (cursor) {
      if (seenCursors.has(cursor)) {
        failPaging('Pagination stopped because the server repeated a cursor without progress.');
        return false;
      }
    }
    const requestPredicate = { ...predicate };
    const body = {
      predicate: requestPredicate, sort, limit: 500,
      ...(filenameQuery ? { filename_query: filenameQuery } : {}),
      ...(mimeFamilies.length ? { mime_families: mimeFamilies } : {}),
      ...(cursor ? { cursor } : {})
    };
    let searchResponse;
    try {
      searchResponse = identityScope?.kind === 'person'
        ? await client.POST('/api/v1/people/{id}/files/search', {
            params: { path: { id: identityScope.id } }, body, signal
          })
        : identityScope?.kind === 'domain'
          ? await client.POST('/api/v1/domains/{domain}/files/search', {
              params: { path: { domain: identityScope.domain } }, body, signal
            })
          : await client.POST('/api/v1/files/search', { body, signal });
    } catch (cause: unknown) {
      if (!signal.aborted && currentGeneration === generation) {
        const message = cause instanceof Error ? cause.message : 'Files could not be loaded.';
        if (cursor) pageError = message;
        else error = message;
        loading = false;
        loadingMore = false;
      }
      return false;
    }
    const { data, error: responseError, response } = searchResponse;
    if (signal.aborted || currentGeneration !== generation) return false;
    loading = false;
    loadingMore = false;
    if (!data) {
      const message = responseError && typeof responseError === 'object' && 'message' in responseError
        ? String(responseError.message)
        : 'Files could not be loaded.';
      if (response.status === 503) unavailable = message;
      else if (cursor) pageError = message;
      else error = message;
      return false;
    }
    const result = data as FileSearchResponse;
    const authority = analyticalAuthority(result);
    if (!cursor) {
      if (expectedAuthority && authority !== expectedAuthority) {
        failPaging('Results changed while loading related files. Reload this view.');
        return false;
      }
      cacheRevision = result.cache_revision;
      pageAuthority = authority;
    } else if (result.cache_revision !== cacheRevision || authority !== pageAuthority) {
      failPaging('Results changed while loading another page. Reload this view.');
      return false;
    }
    if (cursor) seenCursors.add(cursor);
    error = '';
    pageError = '';
    const previousCount = rows.length;
    const merged = new Map(rows.map((row) => [row.key, row]));
    for (const row of result.files ?? []) merged.set(row.key, row);
    rows = [...merged.values()];
    totalCount = result.total_count;
    const followingCursor = result.next_cursor;
    if (followingCursor && (followingCursor === cursor || seenCursors.has(followingCursor))) {
      failPaging('Pagination stopped because the server repeated a cursor without progress.');
      return false;
    }
    if (cursor && followingCursor && rows.length === previousCount) {
      failPaging('Pagination stopped because the next page made no row progress.');
      return false;
    }
    nextCursor = followingCursor;
    if (!activeKey && rows[0]) {
      activeKey = rows[0].key;
      onActiveKey?.(activeKey);
    }
    return true;
  }

  function failPaging(message: string): void {
    error = message;
    nextCursor = undefined;
    loading = false;
    loadingMore = false;
  }

  async function restoreDeepState(
    currentGeneration: number,
    epoch: number | undefined,
    signal: AbortSignal | undefined
  ): Promise<void> {
    if (epoch === undefined || !signal) return;
    const targets = [providedActiveKey, selectedKey].filter((key): key is string => Boolean(key));
    while (
      currentGeneration === generation && !signal.aborted && nextCursor &&
      targets.some((key) => !rows.some((row) => row.key === key))
    ) {
      loadingMore = true;
      if (!await loadPage(currentGeneration, nextCursor, signal)) return;
    }
    if (currentGeneration !== generation || signal.aborted) return;
    if (providedActiveKey && rows.some((row) => row.key === providedActiveKey)) {
      activeKey = providedActiveKey;
    }
    pendingRestoration = {
      generation: currentGeneration,
      epoch,
      activeKey: providedActiveKey
    };
  }

  async function completeDeepRestoration(
    pending: PendingRestoration,
    height: number,
    signature: string
  ): Promise<void> {
    try {
      if (!isCurrentRestoration(pending)) return;
      const target = pending.activeKey
        ? rows.find((row) => row.key === pending.activeKey)
        : undefined;
      if (target) activeKey = target.key;
      await tick();
      if (!isCurrentRestoration(pending) || !grid) return;
      if (target) {
        const index = rows.findIndex((row) => row.key === target.key);
        if (index < 0) return;
        grid.scrollTop = index * height;
        scrollTop = grid.scrollTop;
        await tick();
        if (!isCurrentRestoration(pending) || !renderedRows.some((row) => row.key === target.key)) return;
        if (!grid?.querySelector(`#${rowID(target)}`)) return;
      }
      pendingRestoration = undefined;
      onRestorationComplete?.(pending.epoch);
    } finally {
      if (completingRestoration === signature) completingRestoration = '';
    }
  }

  function isCurrentRestoration(pending: PendingRestoration): boolean {
    return pending.generation === generation &&
      pendingRestoration?.generation === pending.generation &&
      pendingRestoration.epoch === pending.epoch;
  }

  async function loadMore(): Promise<void> {
    if (!nextCursor || loadingMore || !controller) return;
    pageError = '';
    loadingMore = true;
    await loadPage(generation, nextCursor, controller.signal);
  }

  function toggleMIME(family: FileMIMEFamily): void {
    onMIMEFamiliesChange?.(mimeFamilies.includes(family)
      ? mimeFamilies.filter((value) => value !== family)
      : [...mimeFamilies, family]);
  }

  function chooseSort(field: FileSearchSort['field']): void {
    const direction = sort.field === field && sort.direction === 'asc' ? 'desc' : 'asc';
    onSortChange?.({ field, direction });
  }

  function rowID(row: FileSearchRow): string {
    return `file-row-${row.id}`;
  }

  function open(row: FileSearchRow, returnFocus: HTMLElement | undefined = grid): void {
    activeKey = row.key;
    onActiveKey?.(row.key);
    viewerReturnFocus = returnFocus;
    viewerFile = row;
    onSelectedKey?.(row.key);
  }

  function move(index: number): void {
    const height = rowHeight;
    if (height === undefined) return;
    const next = Math.max(0, Math.min(rows.length - 1, index));
    const row = rows[next];
    if (!row) return;
    activeKey = row.key;
    onActiveKey?.(row.key);
    const top = next * height;
    const visibleHeight = grid ? measuredViewport(grid) : viewport;
    if (grid && top < grid.scrollTop) grid.scrollTop = top;
    else if (grid && top + height > grid.scrollTop + visibleHeight) grid.scrollTop = top + height - visibleHeight;
  }

  function measuredViewport(element: HTMLDivElement): number {
    return tableViewportHeight(
      element.clientHeight,
      headerElement?.offsetHeight || 34,
      window.innerHeight
    );
  }

  function applyDensityRebase(
    element: HTMLDivElement,
    height: number,
    expectedScrollHeight: number,
    rebased: number,
  ): void {
    if (grid !== element || previousRowHeight !== height) return;
    if (!element.isConnected) return;
    if (element.scrollHeight !== 0 && (
      element.scrollHeight < expectedScrollHeight || element.clientHeight > window.innerHeight
    )) {
      requestAnimationFrame(() => applyDensityRebase(
        element, height, expectedScrollHeight, rebased
      ));
      return;
    }
    element.scrollTop = rebased;
    scrollTop = element.scrollTop;
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.target !== grid || rows.length === 0 || rowHeight === undefined) return;
    if (event.key === 'ArrowDown' || event.key === 'j') move(activeIndex + 1);
    else if (event.key === 'ArrowUp' || event.key === 'k') move(activeIndex - 1);
    else if (event.key === 'Home') move(0);
    else if (event.key === 'End') move(rows.length - 1);
    else if (event.key === 'Enter') open(rows[Math.max(0, activeIndex)]!, event.currentTarget as HTMLElement);
    else return;
    event.preventDefault();
  }

  function handleScroll(): void {
    scrollTop = grid?.scrollTop ?? 0;
    if (!slice) return;
    if (nextCursor && !loadingMore && slice.end >= rows.length - OVERSCAN) void loadMore();
  }
</script>

<main class="files-workspace" aria-label="Files">
  <header class="workspace-header">
    <div><h1>Files</h1></div>
    <span aria-live="polite">{totalCount.toLocaleString()} files</span>
  </header>

  <div class="file-controls" aria-label="File filters">
    <label>
      Filename
      <SearchInput
        value={filenameQuery}
        ariaLabel="Filter filename"
        placeholder="Filter filename"
        oninput={(value) => onFilenameQueryChange?.(value)}
      />
    </label>
    <div class="mime-controls" aria-label="MIME families">
      {#each ['image', 'pdf', 'audio', 'video', 'text', 'document', 'archive', 'other'] as family}
        <label>
          <input
            type="checkbox"
            checked={mimeFamilies.includes(family as FileMIMEFamily)}
            onchange={() => toggleMIME(family as FileMIMEFamily)}
          />
          {family}
        </label>
      {/each}
    </div>
  </div>

  <section class="files-table" aria-label="Files table">
    <div
      class="files-grid"
      bind:this={grid}
      role="grid"
      aria-label="Files results"
      aria-rowcount={accessibilityRowCount}
      aria-colcount="8"
      aria-busy={loading || loadingMore || pendingRestoration !== undefined || (restorationEpoch !== undefined && rowHeight === undefined)}
      aria-activedescendant={renderedActiveRow ? rowID(renderedActiveRow) : undefined}
      tabindex="0"
      onkeydown={handleKeydown}
      onscroll={handleScroll}
    >
      <div class="table-header" bind:this={headerElement} role="row">
        <span role="columnheader"><button type="button" aria-label="Sort by date" onclick={() => chooseSort('occurred_at')}>Date</button></span>
        <span role="columnheader"><button type="button" aria-label="Sort by filename" onclick={() => chooseSort('filename')}>Filename</button></span>
        <span role="columnheader">Type</span>
        <span role="columnheader"><button type="button" aria-label="Sort by size" onclick={() => chooseSort('size')}>Size</button></span>
        <span role="columnheader">People / domain</span>
        <span role="columnheader">Source</span>
        <span role="columnheader">Containing item</span>
        <span role="columnheader">Availability</span>
      </div>
      <div class="table-body" role="rowgroup">
        {#if unavailable}
          <div role="row"><div role="gridcell" aria-colspan="8"><div class="notice" role="alert"><strong>Analytical cache unavailable</strong><span>{unavailable}</span></div></div></div>
        {:else if error && rows.length === 0}
          <div role="row"><div role="gridcell" aria-colspan="8"><div class="notice" role="alert">{error}</div></div></div>
        {:else if loading && rows.length === 0}
          <div role="row"><div role="gridcell" aria-colspan="8"><div class="notice" role="status">Loading files…</div></div></div>
        {:else if rows.length === 0}
          <div role="row"><div role="gridcell" aria-colspan="8"><div class="notice">No files match this view.</div></div></div>
        {:else if !slice || rowHeight === undefined}
          <div role="row"><div role="gridcell" aria-colspan="8"><div class="notice" role="status">Preparing files layout…</div></div></div>
        {:else}
        <div class="virtual-spacer" style:height={`${slice.totalHeight}px`}>
          <div class="virtual-window" style:transform={`translateY(${slice.topPad}px)`}>
            {#each renderedRows as row, offset (row.key)}
              {@const index = slice.start + offset}
              <div
                id={rowID(row)}
                class="data-row"
                class:data-row--active={index === activeIndex}
                role="row"
                tabindex="-1"
                aria-rowindex={index + 2}
                onpointerdown={(event) => { activeKey = row.key; onActiveKey?.(row.key); grid?.focus(); viewerReturnFocus = event.currentTarget as HTMLElement; }}
                ondblclick={(event) => open(row, event.currentTarget as HTMLElement)}
              >
                <span role="gridcell"><time datetime={row.occurred_at}>{formatDate(row.occurred_at)}</time></span>
                <span role="gridcell"><strong>{row.filename || '(unnamed)'}</strong></span>
                <span role="gridcell">{row.mime_type || row.mime_family}</span>
                <span role="gridcell">{formatBytes(row.size_bytes)}</span>
                <span role="gridcell">{people(row)}</span>
                <span role="gridcell">{row.source_identifier}</span>
                <span role="gridcell">{row.containing_title || row.entry_key}</span>
                <span role="gridcell">{availability(row)}</span>
              </div>
            {/each}
          </div>
        </div>
        {/if}
        {#if pageError}
          <div role="row"><div role="gridcell" aria-colspan="8"><div class="page-error" role="alert">
            <span>{pageError}</span>
            <Button size="sm" surface="outline" label="Retry loading more files" onclick={() => void loadMore()} />
          </div></div></div>
        {/if}
        {#if loadingMore}<div role="row"><div role="gridcell" aria-colspan="8"><div class="progress" role="status">Loading more…</div></div></div>{/if}
      </div>
    </div>
  </section>
</main>

{#if viewerFile}
  <FileViewer
    {client}
    file={viewerFile}
    returnFocus={viewerReturnFocus ?? grid}
    onClose={() => { viewerFile = undefined; onSelectedKey?.(null); }}
    {onOpenItem}
    {onOpenConversation}
  />
{/if}

<script lang="ts" module>
  function formatDate(value: string): string {
    const date = new Date(value);
    return Number.isNaN(date.valueOf()) ? value : new Intl.DateTimeFormat(undefined, { dateStyle: 'medium' }).format(date);
  }
  function formatBytes(value: number): string {
    if (value < 1024) return `${value} B`;
    if (value < 1024 * 1024) return `${Math.round(value / 1024)} KB`;
    return `${(value / (1024 * 1024)).toFixed(1)} MB`;
  }
  function people(row: FileSearchRow): string {
    const labels = row.participant_labels ?? [];
    const domains = row.participant_domains ?? [];
    return [...labels, ...domains].join(', ') || '—';
  }
  function availability(row: FileSearchRow): string {
    if (row.content_state === 'local_content') return 'Local content';
    if (row.content_state === 'missing_blob') return 'Missing blob';
    if (row.content_state === 'url_only') return 'URL only';
    return 'Metadata only';
  }
</script>

<style>
  .files-workspace { display: flex; min-height: 0; flex: 1; flex-direction: column; gap: var(--space-4); }
  .workspace-header { display: flex; align-items: baseline; justify-content: space-between; }
  h1 { margin: 0; font-family: var(--font-sans); font-size: var(--font-size-xl); font-weight: 650; line-height: 1.2; }
  .workspace-header span { color: var(--text-muted); font-size: var(--font-size-xs); }
  .file-controls, .mime-controls { display: flex; align-items: center; gap: var(--space-3); }
  .file-controls { flex-wrap: wrap; }
  .file-controls label { display: flex; align-items: center; gap: var(--space-2); color: var(--text-secondary); font-size: var(--font-size-xs); }
  .files-table { display: flex; min-height: 0; flex: 1; flex-direction: column; overflow: hidden; border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-surface); }
  .table-header, .data-row { display: grid; grid-template-columns: 112px minmax(150px, 1.5fr) minmax(120px, 1fr) 82px minmax(140px, 1.2fr) minmax(130px, 1fr) minmax(160px, 1.3fr) 105px; align-items: center; }
  .files-grid { display: flex; min-height: 0; flex: 1; flex-direction: column; overflow: auto; outline: none; }
  .files-grid:focus-visible { box-shadow: inset 0 0 0 2px var(--accent-blue); }
  .table-header { position: sticky; z-index: 1; top: 0; min-height: var(--table-header-height); flex: 0 0 auto; border-bottom: 1px solid var(--border-default); background: var(--bg-surface); color: var(--text-muted); font-size: var(--font-size-2xs); font-weight: 600; text-transform: uppercase; }
  .table-header span, .data-row span { min-width: 0; padding: 0 var(--space-3); overflow: hidden; text-align: left; text-overflow: ellipsis; white-space: nowrap; }
  .table-header button { width: 100%; height: var(--table-header-height); padding: 0; border: 0; background: transparent; color: inherit; cursor: pointer; font: inherit; text-align: left; text-transform: inherit; }
  .table-body { position: relative; min-height: 220px; flex: 0 0 auto; outline: none; }
  .virtual-spacer { position: relative; }
  .virtual-window { position: absolute; inset: 0 0 auto; }
  .data-row { height: var(--row-height); border-bottom: 1px solid var(--border-muted); color: var(--text-secondary); font-size: var(--font-size-xs); cursor: default; }
  .data-row--active { background: color-mix(in srgb, var(--accent-blue) 12%, var(--bg-surface)); box-shadow: inset 3px 0 var(--accent-blue); }
  .notice { display: flex; min-height: 180px; align-items: center; justify-content: center; gap: var(--space-3); flex-direction: column; color: var(--text-secondary); }
  .page-error { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); padding: var(--space-2) var(--space-3); color: var(--text-danger); }
  .progress { position: sticky; bottom: 0; padding: var(--space-2); background: var(--bg-inset); text-align: center; }
</style>
