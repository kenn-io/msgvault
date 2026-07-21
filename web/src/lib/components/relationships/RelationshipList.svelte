<script lang="ts">
  import { Button, SearchInput, SegmentedControl } from '@kenn-io/kit-ui';

  import type { DomainSummary, PersonSummary } from '../../explore/models';
  import type { RelationshipFacet, RelationshipRow } from '../../relationships/models';

  interface Props {
    rows: RelationshipRow[] | PersonSummary[] | DomainSummary[];
    loading: boolean;
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
    onOpenEverything?: () => void;
  }

  interface ListRowView {
    key: string;
    target: string;
    label: string;
    lastAt: string;
    itemCount: number;
    modalities: number | null;
    score: number | null;
  }

  let {
    rows,
    loading,
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
    onOpenEverything = undefined
  }: Props = $props();

  let gridElement = $state<HTMLDivElement>();
  let activeKey = $state<string | null>(null);

  const views = $derived(rows.map(describeRow));
  const maxScore = $derived(Math.max(1, ...views.map((view) => view.score ?? 0)));
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

  function describeRow(row: RelationshipRow | PersonSummary | DomainSummary): ListRowView {
    if (isRelationshipRow(row)) {
      return {
        key: `cluster:${row.canonical_id}`,
        target: `cluster:${row.canonical_id}`,
        label: row.display_label,
        lastAt: row.signals.last_interaction_at,
        itemCount: row.signals.sent_count,
        modalities: row.signals.modalities,
        score: row.score
      };
    }
    if (isDomainSummary(row)) {
      return {
        key: `domain:${row.domain}`,
        target: `domain:${row.domain}`,
        label: row.domain,
        lastAt: row.last_at,
        itemCount: row.activity_count,
        modalities: null,
        score: null
      };
    }
    return {
      key: `cluster:${row.id}`,
      target: `cluster:${row.id}`,
      label: row.display_label,
      lastAt: row.last_at,
      itemCount: row.activity_count,
      modalities: null,
      score: null
    };
  }

  function formatDate(value: string): string {
    const date = new Date(value);
    return Number.isNaN(date.valueOf()) ? value : date.toLocaleDateString();
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
</script>

<aside class="relationship-list" aria-label="Relationship search and results">
  <div class="toolbar">
    <SearchInput value={query} ariaLabel="Search people and domains" placeholder="Search names and identifiers…"
      oninput={(value) => onQueryChange(value)} />
    <SegmentedControl ariaLabel="Relationship facet" value={facet}
      options={[{ value: 'people', label: 'People' }, { value: 'domains', label: 'Domains' }]}
      onchange={(value) => onFacetChange(value as RelationshipFacet)} />
  </div>
  <label class="show-all">
    <input type="checkbox" checked={showAll} onchange={(event) => onShowAllChange(event.currentTarget.checked)} />
    Show all senders
  </label>

  {#if degraded === 'cache_unavailable'}
    <section class="named-state" role="status">
      <strong>Relationship ranking needs the analytical cache/engine</strong>
      <span>Rebuild the analytical cache with <code>msgvault build-cache</code>, then retry.</span>
      <div><Button label="Open Everything" surface="outline" onclick={() => onOpenEverything?.()} /></div>
    </section>
  {:else if error}
    <section class="named-state" role="alert">{error}</section>
  {:else}
    <div
      class="results-grid"
      role="grid"
      aria-label="Relationship results"
      aria-busy={loading}
      tabindex="0"
      bind:this={gridElement}
      onkeydown={handleKeydown}
    >
      {#if loading && views.length === 0}
        <p role="status">Loading relationships…</p>
      {:else if views.length === 0}
        <p role="status">No relationships found.</p>
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
            onpointerdown={() => selectRow(view)}
          >
            <div role="gridcell">
              <div class="row-main">
                <span class="label">{view.label}</span>
                {#if view.modalities !== null}
                  <span class="modality-badge" aria-label={`${view.modalities} modalities`}>{view.modalities}</span>
                {/if}
              </div>
              <div class="row-meta">
                <span class="last-at">{formatDate(view.lastAt)}</span>
                <span class="item-count">{view.itemCount.toLocaleString()} items</span>
              </div>
              {#if view.score !== null}
                <span class="activity-track" aria-label={`Activity score ${view.score.toFixed(2)}`}>
                  <span class="activity-bar" style:width={`${Math.min(100, (view.score / maxScore) * 100)}%`}></span>
                </span>
              {/if}
            </div>
          </div>
        {/each}
      {/if}
    </div>
  {/if}
</aside>

<style>
  .relationship-list {
    display: flex;
    min-width: 0;
    flex-direction: column;
    gap: var(--space-3);
    overflow: hidden;
  }

  .toolbar {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .show-all {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
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
    gap: 2px;
    overflow: auto;
    outline: none;
  }

  .results-grid:focus-visible {
    outline: var(--focus-ring);
    outline-offset: -2px;
  }

  .result-row {
    border: 1px solid transparent;
    border-radius: var(--radius-sm);
    cursor: default;
  }

  .result-row [role='gridcell'] {
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding: var(--space-2) var(--space-3);
  }

  .result-row:hover {
    background: var(--bg-surface-hover);
  }

  .result-row.active {
    box-shadow: inset 3px 0 var(--accent-teal);
  }

  .result-row.selected {
    background: color-mix(in srgb, var(--accent-teal) 12%, var(--bg-surface));
    border-color: var(--selected-border);
  }

  .row-main {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .label {
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    font-weight: 600;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .modality-badge {
    display: inline-flex;
    min-width: 16px;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    padding: 0 4px;
  }

  .row-meta {
    display: flex;
    gap: var(--space-3);
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
  }

  .activity-track {
    display: block;
    height: 3px;
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
  }

  .activity-bar {
    display: block;
    height: 100%;
    border-radius: var(--radius-sm);
    background: var(--accent-teal);
  }
</style>
