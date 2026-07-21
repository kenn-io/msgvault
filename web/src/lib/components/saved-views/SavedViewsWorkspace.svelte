<script lang="ts">
  import { Button, Modal } from '@kenn-io/kit-ui';
  import { onMount } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { components } from '../../api/generated/schema';
  import {
    DEFAULT_EXPLORE_COLUMNS,
    type ExploreColumn,
    type ExploreURLState
  } from '../../explore/models';
  import { isGroupingDimension } from '../../grouping/catalog';

  type SavedView = components['schemas']['SavedView'];
  type CanonicalState = components['schemas']['SavedViewStateEnvelope'];
  const CURRENT_SCHEMA_VERSION = 1;

  let {
    client,
    currentState,
    selection = undefined,
    onOpen = () => undefined
  }: {
    client: APIClient;
    currentState: ExploreURLState;
    selection?: unknown;
    onOpen?: (state: Partial<ExploreURLState>) => void;
  } = $props();

  let views = $state<SavedView[]>([]);
  let loading = $state(true);
  let saving = $state(false);
  let error = $state('');
  let name = $state('');
  let description = $state('');
  let editing = $state<SavedView>();
  let editName = $state('');
  let editDescription = $state('');
  let deleting = $state<SavedView>();

  onMount(() => void load());

  async function load(): Promise<void> {
    loading = true;
    error = '';
    try {
      const { data, error: responseError } = await client.GET('/api/v1/saved-views');
      if (!data) throw new Error(messageFor(responseError, 'Unable to load Saved Views.'));
      views = data.saved_views ?? [];
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to load Saved Views.';
    } finally {
      loading = false;
    }
  }

  function canonicalState(): CanonicalState {
    return {
      ...(currentState.query ? { query: currentState.query } : {}),
      search_mode: currentState.searchMode,
      filters: currentState.filters.map((filter) => ({
        field: filter.dimension, operator: 'in', values: [...filter.values]
      })),
      grouping: [...currentState.groupingChain],
      presentation: currentState.presentation,
      sort: currentState.sort.map((sort) => ({ field: sort.field, direction: sort.direction })),
      columns: [...currentState.columns]
    };
  }

  async function createView(): Promise<void> {
    if (!name.trim()) return;
    saving = true;
    error = '';
    try {
      const { data, error: responseError } = await client.POST('/api/v1/saved-views', {
        body: {
          name: name.trim(),
          ...(description.trim() ? { description: description.trim() } : {}),
          canonical_state: canonicalState(),
          schema_version: CURRENT_SCHEMA_VERSION
        }
      });
      if (!data) throw new Error(messageFor(responseError, 'Unable to save this view.'));
      views = [...views, data].sort((left, right) => left.name.localeCompare(right.name));
      name = '';
      description = '';
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to save this view.';
    } finally {
      saving = false;
    }
  }

  function beginEdit(view: SavedView): void {
    editing = view;
    editName = view.name;
    editDescription = view.description ?? '';
  }

  async function saveEdit(): Promise<void> {
    if (!editing || !editName.trim()) return;
    saving = true;
    error = '';
    const target = editing;
    try {
      const { data, error: responseError, response } = await client.PATCH('/api/v1/saved-views/{id}', {
        params: {
          path: { id: target.id },
          header: { 'If-Match': `"saved-view-${target.id}-r${target.revision}"` }
        },
        body: {
          name: editName.trim(),
          description: editDescription.trim()
        }
      });
      if (!data) {
        if (response.status === 409) throw new Error('This Saved View changed in another session. Reload and review the latest revision.');
        throw new Error(messageFor(responseError, 'Unable to update this view.'));
      }
      views = views.map((view) => view.id === data.id ? data : view);
      editing = undefined;
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to update this view.';
    } finally {
      saving = false;
    }
  }

  async function confirmDelete(): Promise<void> {
    if (!deleting) return;
    const target = deleting;
    saving = true;
    error = '';
    try {
      const { response, error: responseError } = await client.DELETE('/api/v1/saved-views/{id}', {
        params: {
          path: { id: target.id },
          header: { 'If-Match': `"saved-view-${target.id}-r${target.revision}"` }
        }
      });
      if (!response.ok) throw new Error(messageFor(responseError, 'Unable to delete this view.'));
      views = views.filter((view) => view.id !== target.id);
      deleting = undefined;
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to delete this view.';
    } finally {
      saving = false;
    }
  }

  function open(view: SavedView): void {
    const incompatibility = incompatibilityFor(view);
    if (incompatibility) return;
    const saved = view.canonical_state;
    const filters = (saved.filters ?? []).map((filter) => {
      const aliases: Record<string, ExploreURLState['filters'][number]['dimension']> = {
        source_id: 'source', participant_id: 'participant'
      };
      return {
        dimension: aliases[filter.field] ?? filter.field as ExploreURLState['filters'][number]['dimension'],
        values: [...filter.values]
      };
    });
    onOpen({
      workspace: 'everything',
      query: saved.query ?? '',
      searchMode: saved.search_mode === 'semantic' || saved.search_mode === 'hybrid'
        ? saved.search_mode : 'full_text',
      filters,
      groupingChain: [...(saved.grouping ?? [])] as ExploreURLState['groupingChain'],
      presentation: saved.presentation ?? 'table',
      sort: (saved.sort ?? [{ field: 'occurred_at', direction: 'desc' }]) as ExploreURLState['sort'],
      columns: (saved.columns ?? DEFAULT_EXPLORE_COLUMNS) as ExploreURLState['columns'],
      activeRow: null,
      selectedRow: null,
      conversationAnchor: null,
      scrollAnchor: null
    });
  }

  function incompatibilityFor(view: SavedView): string {
    if (view.schema_version !== CURRENT_SCHEMA_VERSION) {
      return `This view uses schema version ${view.schema_version}. Automatic migration is not supported; remove it and save the current view again.`;
    }
    const saved = view.canonical_state;
    const filterDimensions = new Set(['source', 'participant', 'domain', 'message_type', 'after', 'before', 'deletion']);
    const filterAliases = new Set(['source_id', 'participant_id']);
    for (const filter of saved.filters ?? []) {
      if (!filterDimensions.has(filter.field) && !filterAliases.has(filter.field)) {
        return `This view has an unsupported v1 filter field: ${filter.field}.`;
      }
      if (filter.operator !== 'eq' && filter.operator !== 'in') {
        return `This view has an unsupported v1 filter operator: ${filter.operator}.`;
      }
      if (!Array.isArray(filter.values) || filter.values.length === 0 || filter.values.some((value) => !value)) {
        return `This view has an unsupported v1 filter value for ${filter.field}.`;
      }
    }
    if (saved.search_mode && !['full_text', 'semantic', 'hybrid'].includes(saved.search_mode)) {
      return `This view has an unsupported v1 search mode: ${saved.search_mode}.`;
    }
    if ((saved.grouping ?? []).some((dimension) => !isGroupingDimension(dimension))) {
      return 'This view has an unsupported v1 grouping dimension.';
    }
    if ((saved.sort ?? []).some((sort) => sort.field !== 'occurred_at' || sort.direction !== 'desc')) {
      return 'This view has an unsupported v1 sort.';
    }
    const columns = new Set<ExploreColumn>(['kind', 'people', 'title', 'excerpt', 'time', 'attachments', 'size']);
    if ((saved.columns ?? []).some((column) => !columns.has(column as ExploreColumn))) {
      return 'This view has an unsupported v1 column.';
    }
    return '';
  }

  function messageFor(value: unknown, fallback: string): string {
    return typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string'
      ? value.message : fallback;
  }
</script>

<main class="saved-views" aria-label="Saved Views">
  <header><div><p>Archive workspace</p><h1>Saved Views</h1></div><span>Shared daemon state</span></header>

  {#if error}<p class="notice notice--error" role="alert">{error}</p>{/if}

  <form class="create" onsubmit={(event) => { event.preventDefault(); void createView(); }}>
    <label>Name<input bind:value={name} autocomplete="off" /></label>
    <label>Description<input bind:value={description} autocomplete="off" /></label>
    <Button type="submit" tone="workflow" surface="solid" label="Save current view" disabled={saving || !name.trim()} />
  </form>

  {#if loading}
    <p role="status">Loading Saved Views…</p>
  {:else if views.length === 0}
    <p class="empty">No Saved Views yet.</p>
  {:else}
    <section class="view-list" aria-label="Saved View library">
      {#each views as view (view.id)}
        {@const incompatibility = incompatibilityFor(view)}
        <article>
          {#if editing?.id === view.id}
            <label>Edit name<input aria-label="Edit name" bind:value={editName} /></label>
            <label>Edit description<input aria-label="Edit description" bind:value={editDescription} /></label>
            <div class="actions">
              <Button size="sm" tone="info" surface="solid" label="Save changes" onclick={() => void saveEdit()} disabled={saving} />
              <Button size="sm" surface="soft" label="Cancel edit" onclick={() => { editing = undefined; }} />
            </div>
          {:else}
            <div class="view-copy"><h2>{view.name}</h2><p>{view.description ?? 'No description'}</p></div>
            {#if incompatibility}
              <p class="notice" role="alert">{incompatibility}</p>
            {/if}
            <div class="actions">
              <Button size="sm" tone="info" surface="soft" label={`Open ${view.name}`} disabled={Boolean(incompatibility)} onclick={() => open(view)} />
              {#if !incompatibility}
                <Button size="sm" surface="soft" label={`Edit ${view.name}`} onclick={() => beginEdit(view)} />
                <Button size="sm" tone="danger" surface="soft" label={`Delete ${view.name}`} onclick={() => { deleting = view; }} />
              {:else}
                <Button size="sm" tone="danger" surface="soft" label={`Remove incompatible ${view.name}`} onclick={() => { deleting = view; }} />
              {/if}
            </div>
          {/if}
        </article>
      {/each}
    </section>
  {/if}
</main>

{#if deleting}
  <Modal title="Delete Saved View?" tone="danger" onclose={() => { deleting = undefined; }}>
    <p>Delete “{deleting.name}” from every authenticated browser session?</p>
    {#snippet footer()}
      <Button surface="soft" label="Cancel" onclick={() => { deleting = undefined; }} />
      <Button tone="danger" surface="solid" label="Confirm delete" disabled={saving} onclick={() => void confirmDelete()} />
    {/snippet}
  </Modal>
{/if}

<style>
  .saved-views { display: flex; min-height: 0; flex: 1; flex-direction: column; gap: var(--space-4); padding: var(--space-5) var(--space-6); }
  header, .create, article, .actions { display: flex; align-items: center; gap: var(--space-3); }
  header { justify-content: space-between; }
  header p, h1, h2, article p { margin: 0; }
  header p { color: var(--accent-amber); font-size: var(--font-size-2xs); font-weight: 800; letter-spacing: .1em; text-transform: uppercase; }
  header span, article p, .empty { color: var(--text-muted); font-size: var(--font-size-xs); }
  .create { padding: var(--space-3); border: 1px solid var(--border-muted); border-radius: var(--radius-md); background: var(--bg-surface); }
  label { display: grid; gap: var(--space-1); color: var(--text-muted); font-size: var(--font-size-xs); }
  input { min-width: 12rem; padding: var(--space-2); border: 1px solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-canvas); color: var(--text-primary); }
  .view-list { display: grid; gap: var(--space-2); }
  article { justify-content: space-between; padding: var(--space-3); border-bottom: 1px solid var(--border-muted); }
  .view-copy { display: grid; min-width: 12rem; gap: var(--space-1); }
  .notice { padding: var(--space-2) var(--space-3); border-left: 3px solid var(--accent-amber); background: var(--bg-subtle); }
  .notice--error { border-color: var(--accent-red); color: var(--text-danger); }
  @media (max-width: 760px) { .create, article { align-items: stretch; flex-direction: column; } input { min-width: 0; width: 100%; } }
</style>
