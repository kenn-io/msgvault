<script lang="ts">
  import { Button, SearchInput, SegmentedControl } from '@kenn-io/kit-ui';

  import type { DomainSummary, PersonSummary } from '../../explore/models';
  import type { RelationshipFacet, RelationshipRow } from '../../relationships/models';
  import EmptyState from '../common/EmptyState.svelte';
  import IdentityAvatar from '../common/IdentityAvatar.svelte';

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
    itemUnit: 'sent' | 'items';
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
        itemUnit: 'sent',
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
        itemUnit: 'items',
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
      itemUnit: 'items',
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
  {#if facet === 'people'}
    <label class="show-all">
      <input type="checkbox" checked={showAll} onchange={(event) => onShowAllChange(event.currentTarget.checked)} />
      Show all senders
    </label>
  {/if}

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
      data-scroll
      bind:this={gridElement}
      onkeydown={handleKeydown}
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
                size={28}
              />
              <div class="row-body">
                <div class="row-main">
                  <span class="label">{view.label}</span>
                  <span class="last-at" data-mono>{formatDate(view.lastAt)}</span>
                </div>
                <div class="row-meta">
                  <span class="item-count" data-mono>{view.itemCount.toLocaleString()} {view.itemUnit}</span>
                  {#if view.modalities !== null}
                    <span class="modality-badge" data-mono aria-label={`${view.modalities} modalities`}>{view.modalities}</span>
                  {/if}
                  {#if view.score !== null}
                    <span class="activity-track" aria-label={`Activity score ${view.score.toFixed(2)}`}>
                      <span class="activity-bar" style:width={`${Math.min(100, (view.score / maxScore) * 100)}%`}></span>
                    </span>
                  {/if}
                </div>
              </div>
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
    min-height: 0;
    flex: 1;
    flex-direction: column;
    gap: var(--space-3);
    overflow: hidden;
    padding: var(--space-5) var(--space-4) 0;
  }

  .toolbar {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .show-all {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
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
    align-items: center;
    gap: var(--space-4);
    padding: var(--space-2) var(--space-3);
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
    gap: var(--space-3);
  }

  .label {
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    font-weight: 600;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .last-at {
    flex: none;
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
  }

  .modality-badge {
    display: inline-flex;
    min-width: 16px;
    flex: none;
    align-items: center;
    justify-content: center;
    border: 1px solid var(--border-muted);
    border-radius: 999px;
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    line-height: 1.4;
    padding: 0 5px;
  }

  .row-meta {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
  }

  /* Fixed-width gauge on the row's right edge. The track itself stays
   * invisible — only the accent fill shows, so low-score rows read as
   * quiet instead of as a broken progress bar. */
  .activity-track {
    display: block;
    width: 56px;
    height: 3px;
    flex: none;
    align-self: center;
    margin-left: auto;
  }

  .activity-bar {
    display: block;
    min-width: 3px;
    height: 100%;
    border-radius: 2px;
    background: color-mix(in srgb, var(--accent-blue) 45%, transparent);
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
