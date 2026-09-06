<script lang="ts">
  import { Button, EmptyState } from '@kenn-io/kit-ui';

  import type { DirectoryPerson } from '../../directory/models';

  interface Props {
    rows: DirectoryPerson[];
    loading: boolean;
    loadingMore: boolean;
    error: string | null;
    pageError: string | null;
    pageRecovery: 'retry' | 'reload' | null;
    hasMore: boolean;
    selectedPersonID: number | null;
    onSelect: (personID: number) => void;
    onLoadMore: () => void;
    onReload: () => void;
  }

  let { rows, loading, loadingMore, error, pageError, pageRecovery, hasMore, selectedPersonID, onSelect, onLoadMore, onReload }: Props = $props();
  let gridElement = $state<HTMLDivElement>();
  let activeID = $state<number | null>(null);
  const activeIndex = $derived(activeID === null ? -1 : rows.findIndex((row) => row.id === activeID));

  $effect(() => {
    if (activeID !== null && rows.some((row) => row.id === activeID)) return;
    activeID = rows[0]?.id ?? null;
  });

  async function moveTo(index: number): Promise<void> {
    if (rows.length === 0) return;
    const next = Math.max(0, Math.min(rows.length - 1, index));
    activeID = rows[next]!.id;
    await Promise.resolve();
    const row = gridElement?.querySelector<HTMLElement>(`[data-person-id="${activeID}"]`);
    row?.scrollIntoView({ block: 'nearest' });
    row?.focus();
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.metaKey || event.ctrlKey || event.altKey || rows.length === 0) return;
    if (event.key === 'ArrowDown' || event.key === 'j') void moveTo(activeIndex + 1);
    else if (event.key === 'ArrowUp' || event.key === 'k') void moveTo(activeIndex - 1);
    else if (event.key === 'Home') void moveTo(0);
    else if (event.key === 'End') void moveTo(rows.length - 1);
    else if ((event.key === 'Enter' || event.key === ' ') && activeID !== null) onSelect(activeID);
    else return;
    event.preventDefault();
  }
</script>

<section class="directory-list" aria-label="Directory results">
  {#if error && rows.length === 0}
    <div role="alert" class="notice">{error}</div>
  {:else}
    {#if pageError}
      <div role="alert" class="notice page-error">
        <span>{pageError}</span>
        {#if pageRecovery === 'retry' && hasMore}
          <Button size="sm" surface="outline" label="Retry loading more people" onclick={onLoadMore} />
        {:else if pageRecovery === 'reload'}
          <Button size="sm" surface="outline" label="Reload directory" onclick={onReload} />
        {/if}
      </div>
    {/if}
    {#if loading && rows.length > 0}
      <p role="status" class="empty">Updating people…</p>
    {/if}
    {#if loading && rows.length === 0}
      <p role="status" class="empty">Loading people…</p>
    {:else if rows.length === 0}
      <EmptyState title="No people found" description="Try a different search or filter." />
    {:else}
      <div
        bind:this={gridElement}
        role="grid"
        aria-label="Directory people"
        aria-busy={loading || loadingMore}
        tabindex="-1"
      >
        {#each rows as person (person.id)}
          <div
            role="row"
            data-person-id={person.id}
            class:active={person.id === activeID}
            class:selected={person.id === selectedPersonID}
            aria-selected={person.id === selectedPersonID}
            tabindex={person.id === activeID ? 0 : -1}
            onkeydown={handleKeydown}
            onclick={() => { activeID = person.id; onSelect(person.id); }}
          >
            <span role="gridcell" class="name">{person.display_name ?? `Person ${person.id}`}</span>
            <span role="gridcell" class="meta">{person.primary_channel ?? 'No primary channel'} · {person.contact_state}</span>
            <span role="gridcell" class="meta">{person.last_contact_at ? `Last contact ${person.last_contact_at}` : 'Never contacted'}</span>
            {#if person.organizations?.length}<span role="gridcell" class="meta">{person.organizations.join(' · ')}</span>{/if}
            {#if person.categories?.length}<span role="gridcell" class="meta">{person.categories.join(' · ')}</span>{/if}
          </div>
        {/each}
      </div>
    {/if}
    {#if hasMore && rows.length > 0 && pageRecovery !== 'reload'}
      <div class="more"><Button label={loadingMore ? 'Loading more…' : 'Load more people'} disabled={loadingMore} onclick={onLoadMore} /></div>
    {/if}
  {/if}
</section>

<style>
  .directory-list { min-width: 0; display: flex; flex-direction: column; gap: var(--space-3); }
  [role="grid"] { display: grid; gap: 2px; outline: none; }
  [role="row"] { display: grid; gap: 2px; text-align: left; border: 1px solid transparent; border-radius: var(--radius-sm); padding: var(--space-3); background: var(--bg-surface); color: var(--text-primary); cursor: pointer; }
  [role="row"]:hover, [role="row"].active { background: var(--bg-surface-hover); }
  [role="row"].selected { border-color: var(--border-strong); }
  [role="row"]:focus-visible, [role="grid"]:focus-visible { outline: 2px solid var(--focus-ring); outline-offset: 2px; }
  .name { font-weight: var(--font-weight-semibold, 600); }
  .meta, .empty { color: var(--text-muted); font-size: var(--font-size-sm); }
  .notice { padding: var(--space-3); color: var(--text-secondary); background: var(--bg-inset); border-radius: var(--radius-sm); }
  .page-error { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); }
  .more { display: flex; justify-content: center; }
</style>
