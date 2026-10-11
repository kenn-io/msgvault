<script lang="ts">
  import { Button, EmptyState } from '@kenn-io/kit-ui';
  import UserRoundIcon from '@lucide/svelte/icons/user-round';

  import type { DirectoryPerson } from '../../directory/models';
  import { channelLabel, contactStateLabel, formatContactDate } from '../../directory/labels';
  import { ExploreSelectionState } from '../../explore/state.svelte';
  import SelectionCheckbox from '../common/SelectionCheckbox.svelte';

  interface Props {
    rows: DirectoryPerson[];
    loading: boolean;
    loadingMore: boolean;
    error: string | null;
    pageError: string | null;
    pageRecovery: 'retry' | 'reload' | null;
    hasMore: boolean;
    selectedPersonID: number | null;
    selection?: ExploreSelectionState;
    bulkPending?: boolean;
    bulkMessage?: string | null;
    bulkError?: boolean;
    bulkFailures?: Array<{ name: string; message: string }>;
    onSelect: (personID: number) => void;
    onBulkDelete?: () => void;
    onLoadMore: () => void;
    onReload: () => void;
  }

  let {
    rows,
    loading,
    loadingMore,
    error,
    pageError,
    pageRecovery,
    hasMore,
    selectedPersonID,
    selection = new ExploreSelectionState(),
    bulkPending = false,
    bulkMessage = null,
    bulkError = false,
    bulkFailures = [],
    onSelect,
    onBulkDelete = undefined,
    onLoadMore,
    onReload
  }: Props = $props();
  let gridElement = $state<HTMLDivElement>();
  let activeID = $state<number | null>(null);
  const activeIndex = $derived(activeID === null ? -1 : rows.findIndex((row) => row.id === activeID));
  const orderedKeys = $derived(rows.map((row) => String(row.id)));
  const allLoadedSelected = $derived(
    orderedKeys.length > 0 && orderedKeys.every((key) => selection.isSelected(key))
  );
  const someLoadedSelected = $derived(orderedKeys.some((key) => selection.isSelected(key)));

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
    // Keys on the row's checkbox belong to the checkbox, not to row navigation.
    if (event.target !== event.currentTarget) return;
    if (event.metaKey || event.ctrlKey || event.altKey || rows.length === 0) return;
    if (event.key === 'ArrowDown' || event.key === 'j') void moveTo(activeIndex + 1);
    else if (event.key === 'ArrowUp' || event.key === 'k') void moveTo(activeIndex - 1);
    else if (event.key === 'Home') void moveTo(0);
    else if (event.key === 'End') void moveTo(rows.length - 1);
    else if ((event.key === 'Enter' || event.key === ' ') && activeID !== null) onSelect(activeID);
    else return;
    event.preventDefault();
  }

  function toggleRowSelection(person: DirectoryPerson, index: number, range: boolean): void {
    activeID = person.id;
    selection.toggle(String(person.id), index, orderedKeys, range);
  }

  function toggleLoadedSelection(): void {
    if (allLoadedSelected) selection.clear();
    else selection.selectVisible(orderedKeys);
  }

  function personSummary(person: DirectoryPerson): string {
    return [
      person.primary_channel ? channelLabel(person.primary_channel) : 'No primary channel',
      contactStateLabel(person.contact_state),
      ...(person.organizations ?? []),
      ...(person.categories ?? [])
    ].join(' · ');
  }
</script>

{#snippet bulkStatus()}
  {#if bulkMessage}
    <div class="bulk-status" role={bulkError ? 'alert' : 'status'}>
      <p class="bulk-message" class:error={bulkError}>{bulkMessage}</p>
      {#if bulkFailures.length > 0}
        <ul class="bulk-failures">
          {#each bulkFailures as failure, index (index)}
            <li><strong>{failure.name}:</strong> {failure.message}</li>
          {/each}
        </ul>
      {/if}
    </div>
  {/if}
{/snippet}

<section class="directory-list" aria-label="Directory results">
  {#if error && rows.length === 0}
    <!-- The reload after a bulk delete can fail; its outcome must survive. -->
    {@render bulkStatus()}
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
    {#if rows.length > 0}
      <div class="selection-toolbar" aria-label="Directory selection">
        <span class="header-checkbox">
          <SelectionCheckbox
            checked={allLoadedSelected}
            mixed={!allLoadedSelected && someLoadedSelected}
            label={allLoadedSelected ? 'Unselect all loaded people' : 'Select all loaded people'}
            disabled={bulkPending}
            onToggle={toggleLoadedSelection}
          />
        </span>
        <span class="selection-label">
          {selection.count > 0 ? `${selection.count.toLocaleString()} selected` : 'Select all'}
        </span>
        {#if selection.count > 0}
          <Button
            size="sm"
            tone="danger"
            surface="soft"
            label={bulkPending ? 'Deleting…' : 'Delete…'}
            disabled={bulkPending}
            onclick={() => onBulkDelete?.()}
          />
          <Button size="sm" surface="soft" label="Clear" disabled={bulkPending} onclick={() => selection.clear()} />
        {/if}
      </div>
    {/if}
    <!-- Outside the rows check: a bulk delete can empty the list. -->
    {@render bulkStatus()}
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
        class:selection-mode={selection.count > 0}
        tabindex="-1"
      >
        {#each rows as person, index (person.id)}
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
            <span role="gridcell" class="avatar-slot">
              <span class="person-icon"><UserRoundIcon size={16} aria-hidden="true" /></span>
              <SelectionCheckbox
                checked={selection.isSelected(String(person.id))}
                label={`${selection.isSelected(String(person.id)) ? 'Unselect' : 'Select'} ${person.display_name ?? `Person ${person.id}`}`}
                disabled={bulkPending}
                onToggle={(range) => toggleRowSelection(person, index, range)}
              />
            </span>
            <span role="gridcell" class="row-copy">
              <span class="row-primary">
                <span class="name">{person.display_name ?? `Person ${person.id}`}</span>
                <span class="contact-date">{person.last_contact_at ? `Last contact ${formatContactDate(person.last_contact_at)}` : 'Never contacted'}</span>
              </span>
              <span class="meta">{personSummary(person)}</span>
            </span>
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
  [role="row"] { display: flex; min-height: 52px; align-items: center; gap: var(--space-4); text-align: left; border: 1px solid transparent; border-radius: var(--radius-sm); padding: var(--space-3) var(--space-4); background: var(--bg-surface); color: var(--text-primary); cursor: pointer; }
  [role="row"]:hover, [role="row"].active { background: var(--bg-surface-hover); }
  [role="row"].selected { border-color: var(--border-strong); }
  [role="row"]:focus-visible, [role="grid"]:focus-visible { outline: 2px solid var(--focus-ring); outline-offset: 2px; }
  .selection-toolbar { display: flex; min-height: 28px; align-items: center; gap: var(--space-2); padding-inline: var(--space-2); color: var(--text-muted); font-size: var(--font-size-xs); }
  .header-checkbox { display: grid; width: 24px; height: 24px; flex: none; place-items: center; }
  .selection-label { min-width: 0; flex: 1; }
  .bulk-message { margin: 0; padding-inline: var(--space-2); color: var(--text-muted); font-size: var(--font-size-xs); }
  .bulk-message.error { color: var(--text-danger); }
  .bulk-failures { margin: var(--space-1) 0 0; padding-inline: var(--space-6) var(--space-2); color: var(--text-secondary); font-size: var(--font-size-xs); }
  .avatar-slot { position: relative; display: grid; width: 24px; height: 24px; flex: none; place-items: center; }
  .person-icon, .avatar-slot :global(.selection-checkbox) { position: absolute; transition: opacity 80ms ease-out, transform 80ms ease-out; }
  .person-icon { display: grid; width: 24px; height: 24px; place-items: center; border-radius: 50%; background: var(--bg-inset); color: var(--text-muted); }
  .avatar-slot :global(.selection-checkbox) { opacity: 0; transform: scale(.9); }
  [role="row"]:hover .person-icon, [role="row"]:focus-within .person-icon, [role="grid"].selection-mode .person-icon { opacity: 0; transform: scale(.9); }
  [role="row"]:hover .avatar-slot :global(.selection-checkbox), [role="row"]:focus-within .avatar-slot :global(.selection-checkbox), [role="grid"].selection-mode .avatar-slot :global(.selection-checkbox) { opacity: 1; transform: scale(1); }
  .row-copy { display: grid; min-width: 0; flex: 1; gap: 2px; }
  .row-primary { display: flex; min-width: 0; align-items: baseline; justify-content: space-between; gap: var(--space-4); }
  .name { font-weight: var(--font-weight-semibold, 600); }
  .name, .meta { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .meta, .empty, .contact-date { color: var(--text-muted); font-size: var(--font-size-sm); }
  .contact-date { flex: none; font-size: var(--font-size-xs); }
  .notice { padding: var(--space-3); color: var(--text-secondary); background: var(--bg-inset); border-radius: var(--radius-sm); }
  .page-error { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); }
  .more { display: flex; justify-content: center; }
</style>
