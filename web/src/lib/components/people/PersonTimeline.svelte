<script lang="ts">
  import type { EntryRow, ExploreCacheUnavailable } from '../../explore/models';
  import { untrack } from 'svelte';
  import { ExploreSelectionState } from '../../explore/state.svelte';
  import type { ExploreScrollAnchor } from '../../explore/models';
  import EverythingTable from '../explore/EverythingTable.svelte';

  interface Props {
    rows: EntryRow[];
    totalCount?: number;
    loading?: boolean;
    loadingMore?: boolean;
    hasMore?: boolean;
    generation?: number;
    error?: string;
    pageError?: string;
    unavailable?: ExploreCacheUnavailable;
    onOpen?: (row: EntryRow) => void;
    onRetry?: () => void;
    onLoadMore?: () => Promise<unknown>;
    onLoadThroughEnd?: () => Promise<void>;
    selection?: ExploreSelectionState;
    focusedKey?: string | null;
    inspectedKey?: string | null;
    scrollAnchor?: ExploreScrollAnchor | null;
    restoring?: boolean;
    onActiveKey?: (key: string) => void;
    onScrollAnchor?: (key: string, offset: number) => void;
    onVisibleRows?: (keys: string[]) => void;
  }

  let { rows, totalCount = undefined, loading = false, loadingMore = false, hasMore = false,
    generation = 0, error = '', pageError = '', unavailable = undefined, onOpen = undefined, onRetry = undefined,
    onLoadMore = undefined, onLoadThroughEnd = undefined, selection: providedSelection = undefined,
    focusedKey = null, inspectedKey = null, scrollAnchor = null, restoring = false,
    onActiveKey = undefined, onScrollAnchor = undefined, onVisibleRows = undefined }: Props = $props();
  const selection = untrack(() => providedSelection ?? new ExploreSelectionState());
</script>

<section class="timeline" aria-label="Canonical activity timeline">
  <header>
    <div><p>Canonical context</p><h2>Activity</h2></div>
    <span>{totalCount === undefined ? 'Bounded timeline' : `${totalCount.toLocaleString()} items`}</span>
  </header>
  <EverythingTable {rows} {selection} {loading} {loadingMore} {hasMore} {generation} {error}
    {pageError} {unavailable} {totalCount} {onOpen} {onRetry} {onLoadMore} {onLoadThroughEnd}
    {focusedKey} {inspectedKey} {scrollAnchor} {restoring} {onActiveKey} {onScrollAnchor} {onVisibleRows} />
</section>

<style>
  .timeline { display: flex; min-height: 320px; flex-direction: column; gap: var(--space-3); }
  header { display: flex; align-items: end; justify-content: space-between; }
  header p, header h2 { margin: 0; }
  header p { color: var(--accent-amber); font-size: var(--font-size-2xs); font-weight: 800; letter-spacing: .08em; text-transform: uppercase; }
  header h2 { font-size: var(--font-size-lg); }
  header span { color: var(--text-muted); font-size: var(--font-size-xs); }
</style>
