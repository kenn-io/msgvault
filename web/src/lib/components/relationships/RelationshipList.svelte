<script lang="ts">
  import { Button, SearchInput, SegmentedControl } from '@kenn-io/kit-ui';

  import type { DomainSummary, PersonSummary } from '../../explore/models';
  import type { RelationshipFacet, RelationshipRow } from '../../relationships/models';
  import { compactDate } from '../../util/dates';
  import EmptyState from '../common/EmptyState.svelte';
  import IdentityAvatar from '../common/IdentityAvatar.svelte';

  interface Props {
    rows: RelationshipRow[] | PersonSummary[] | DomainSummary[];
    loading: boolean;
    loadingMore?: boolean;
    hasMore?: boolean;
    totalCount?: number | null;
    error: string | null;
    degraded: 'cache_unavailable' | null;
    facet: RelationshipFacet;
    query: string;
    showAll: boolean;
    activeTarget?: string | null;
    onQueryChange: (value: string) => void;
    onFacetChange: (facet: RelationshipFacet) => void;
    onShowAllChange: (value: boolean) => void;
    onSelect: (target: string) => void;
    onLoadMore?: () => void;
    onOpenEverything?: () => void;
  }

  interface ListRowView {
    key: string;
    target: string;
    label: string;
    lastAt: string;
    summary: string;
  }

  let {
    rows,
    loading,
    loadingMore = false,
    hasMore = false,
    totalCount = null,
    error,
    degraded,
    facet,
    query,
    showAll,
    activeTarget = null,
    onQueryChange,
    onFacetChange,
    onShowAllChange,
    onSelect,
    onLoadMore = undefined,
    onOpenEverything = undefined
  }: Props = $props();

  /* Quiet infinite scroll (same scroll-proximity idiom as
   * RelationshipTimeline): once the remaining scroll runway shrinks below
   * this, the next page loads with only the inline "Loading more…" line. */
  const LOAD_MORE_PROXIMITY_PX = 200;

  let gridElement = $state<HTMLDivElement>();
  let activeKey = $state<string | null>(null);

  const views = $derived(rows.map(describeRow));
  const activeIndex = $derived(activeKey ? views.findIndex((view) => view.key === activeKey) : -1);

  $effect(() => {
    const keys = views.map((view) => view.key);
    if (activeKey && keys.includes(activeKey)) return;
    activeKey = keys[0] ?? null;
  });

  // Plain `'field' in row` narrowing doesn't work here: every branch of the
  // union carries a trailing `[key: string]: unknown` index signature
  // (from the generated OpenAPI types), which defeats TypeScript's control-
  // flow narrowing. Named predicates with an explicit `is` return type are
  // trusted at the call site instead of re-derived from the `in` check.
  function isRelationshipRow(row: RelationshipRow | PersonSummary | DomainSummary): row is RelationshipRow {
    return 'canonical_id' in row;
  }

  function isDomainSummary(row: PersonSummary | DomainSummary): row is DomainSummary {
    return 'domain' in row;
  }

  /** One quiet line of only the nonzero signals ("651 sent · 12 meetings");
   * a fully quiet relationship keeps an empty line so row heights stay even. */
  function signalSummary(row: RelationshipRow): string {
    const parts: string[] = [];
    if (row.signals.sent_count > 0) parts.push(`${row.signals.sent_count.toLocaleString()} sent`);
    if (row.signals.meeting_count > 0) parts.push(`${row.signals.meeting_count.toLocaleString()} meetings`);
    return parts.join(' · ');
  }

  function describeRow(row: RelationshipRow | PersonSummary | DomainSummary): ListRowView {
    if (isRelationshipRow(row)) {
      return {
        key: `cluster:${row.canonical_id}`,
        target: `cluster:${row.canonical_id}`,
        label: row.display_label,
        lastAt: row.signals.last_interaction_at,
        summary: signalSummary(row)
      };
    }
    if (isDomainSummary(row)) {
      return {
        key: `domain:${row.domain}`,
        target: `domain:${row.domain}`,
        label: row.domain,
        lastAt: row.last_at,
        summary: `${row.activity_count.toLocaleString()} items`
      };
    }
    return {
      key: `cluster:${row.id}`,
      target: `cluster:${row.id}`,
      label: row.display_label,
      lastAt: row.last_at,
      summary: `${row.activity_count.toLocaleString()} items`
    };
  }

  function editableTarget(target: EventTarget | null): boolean {
    const element = target as HTMLElement | null;
    return Boolean(element?.closest('input, textarea, select, [contenteditable]:not([contenteditable="false"])'));
  }

  function moveTo(index: number): void {
    if (views.length === 0) return;
    const next = Math.max(0, Math.min(views.length - 1, index));
    activeKey = views[next]!.key;
    gridElement?.querySelector<HTMLElement>(`[data-row-key="${cssEscape(activeKey)}"]`)?.scrollIntoView({ block: 'nearest' });
  }

  function cssEscape(value: string): string {
    return value.replace(/["\\]/g, '\\$&');
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (editableTarget(event.target) || views.length === 0) return;
    if (event.metaKey || event.ctrlKey || event.altKey) return;
    if (event.key === 'j' || event.key === 'ArrowDown') moveTo(activeIndex + 1);
    else if (event.key === 'k' || event.key === 'ArrowUp') moveTo(activeIndex - 1);
    else if (event.key === 'Enter' && activeIndex >= 0) onSelect(views[activeIndex]!.target);
    else return;
    event.preventDefault();
  }

  function selectRow(view: ListRowView): void {
    activeKey = view.key;
    onSelect(view.target);
  }

  // Fires on user scrolling and on the scrollIntoView calls moveTo makes, so
  // keyboard navigation toward the end also pulls the next page in.
  function handleScroll(): void {
    if (!gridElement || !hasMore || loadingMore || loading) return;
    const remaining = gridElement.scrollHeight - gridElement.scrollTop - gridElement.clientHeight;
    if (remaining <= LOAD_MORE_PROXIMITY_PX) onLoadMore?.();
  }
</script>

<aside class="relationship-list" aria-label="Relationship search and results">
  <div class="toolbar">
    <SearchInput value={query} ariaLabel="Search people and domains" placeholder="Search names and identifiers…"
      block oninput={(value) => onQueryChange(value)} />
    <div class="toolbar-row">
      <SegmentedControl ariaLabel="Relationship facet" value={facet}
        options={[{ value: 'people', label: 'People' }, { value: 'domains', label: 'Domains' }]}
        onchange={(value) => onFacetChange(value as RelationshipFacet)} />
      {#if facet === 'people'}
        <button
          type="button"
          class="show-all-chip"
          aria-pressed={showAll}
          onclick={() => onShowAllChange(!showAll)}
        >
          All senders
        </button>
      {/if}
    </div>
  </div>

  {#if degraded === 'cache_unavailable'}
    <section class="named-state" role="status">
      <strong>Relationship ranking needs the analytical cache/engine</strong>
      <span>Rebuild the analytical cache with <code>msgvault build-cache</code>, then retry.</span>
      <div><Button label="Open Everything" surface="outline" onclick={() => onOpenEverything?.()} /></div>
    </section>
  {:else if error && views.length === 0}
    <section class="named-state" role="alert">{error}</section>
  {:else}
    {#if error}
      <!-- A failed page fetch mid-scroll must not wipe the rows already
           loaded: keep the list and surface the failure as a slim banner. -->
      <p class="list-error" role="alert">{error}</p>
    {/if}
    <div
      class="results-grid"
      role="grid"
      aria-label="Relationship results"
      aria-busy={loading || loadingMore}
      aria-rowcount={totalCount ?? undefined}
      tabindex="0"
      data-scroll
      bind:this={gridElement}
      onkeydown={handleKeydown}
      onscroll={handleScroll}
    >
      {#if loading && views.length === 0}
        <p class="list-empty" role="status">Loading relationships…</p>
      {:else if views.length === 0}
        <EmptyState
          glyph="people"
          label="No relationships found"
          hint="Try a different search, or switch between People and Domains."
        />
      {:else}
        {#each views as view, index (view.key)}
          <div
            class="result-row"
            class:active={index === activeIndex}
            class:selected={view.target === activeTarget}
            role="row"
            tabindex="-1"
            data-row-key={view.key}
            aria-selected={view.target === activeTarget}
            style:--reveal-index={index}
            onpointerdown={() => selectRow(view)}
          >
            <div role="gridcell">
              <IdentityAvatar
                label={view.label}
                seed={view.key}
                shape={view.key.startsWith('domain:') ? 'domain' : 'person'}
                size={24}
              />
              <div class="row-body">
                <div class="row-main">
                  <span class="label">{view.label}</span>
                  <span class="last-at" data-mono>{compactDate(view.lastAt)}</span>
                </div>
                <span class="row-summary" data-mono>{view.summary}</span>
              </div>
            </div>
          </div>
        {/each}
        {#if loadingMore}
          <p class="list-more" role="status">Loading more…</p>
        {/if}
      {/if}
    </div>
  {/if}
</aside>

<style>
  .relationship-list {
    display: flex;
    min-width: 0;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    gap: var(--space-4);
    overflow: hidden;
    padding: var(--space-5) var(--space-4) 0;
  }

  /* One cohesive header block on the 4px grid: search on top, the facet
   * segment and the all-senders filter chip sharing one 24px-tall line. */
  .toolbar {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }

  .toolbar-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
  }

  .show-all-chip {
    flex: none;
    border: 1px solid var(--border-muted);
    border-radius: 999px;
    background: transparent;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    line-height: 1;
    padding: var(--space-2) var(--space-4);
    cursor: pointer;
    transition: background-color 80ms ease-out, color 80ms ease-out, border-color 80ms ease-out;
  }

  .show-all-chip:hover {
    color: var(--text-secondary);
    border-color: var(--border-strong);
  }

  .show-all-chip[aria-pressed='true'] {
    border-color: color-mix(in srgb, var(--accent-blue) 45%, transparent);
    background: color-mix(in srgb, var(--accent-blue) 12%, transparent);
    color: var(--text-primary);
  }

  .list-empty {
    display: grid;
    gap: var(--space-2);
    margin: 0;
    padding: var(--space-8) var(--space-5);
    color: var(--text-muted);
    font-size: var(--font-size-sm);
    text-align: center;
  }

  .list-error {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--accent-red) 10%, transparent);
    color: var(--text-primary);
    font-size: var(--font-size-xs);
  }

  .list-more {
    margin: 0;
    padding: var(--space-3) var(--space-5);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    text-align: center;
  }

  .named-state {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-md);
    padding: var(--space-4);
    font-size: var(--font-size-sm);
  }

  .results-grid {
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    overflow: auto;
    outline: none;
    margin-inline: calc(var(--space-2) * -1);
    padding-bottom: var(--space-4);
  }

  .results-grid:focus-visible {
    outline: var(--focus-ring);
    outline-offset: -2px;
  }

  .result-row {
    border-radius: var(--radius-sm);
    cursor: default;
    transition: background-color 80ms ease-out;
  }

  .result-row [role='gridcell'] {
    display: flex;
    min-height: 52px;
    align-items: center;
    gap: var(--space-4);
    padding: var(--space-3) var(--space-4);
  }

  .row-body {
    display: flex;
    min-width: 0;
    flex: 1;
    flex-direction: column;
    gap: 2px;
  }

  .result-row:hover {
    background: var(--bg-surface-hover);
  }

  .result-row.active {
    box-shadow: inset 2px 0 0 var(--accent-blue);
  }

  .result-row.selected {
    background: var(--selected-bg);
    box-shadow: inset 2px 0 0 var(--accent-blue);
  }

  .row-main {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-4);
  }

  .label {
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    font-weight: 500;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .last-at {
    flex: none;
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    font-variant-numeric: tabular-nums;
  }

  .row-summary {
    min-height: 1em;
    overflow: hidden;
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  @media (prefers-reduced-motion: no-preference) {
    .result-row {
      animation: row-reveal 150ms ease-out backwards;
      animation-delay: calc(min(var(--reveal-index, 0), 15) * 14ms);
    }

    @keyframes row-reveal {
      from {
        opacity: 0;
        transform: translateY(4px);
      }
    }
  }
</style>
