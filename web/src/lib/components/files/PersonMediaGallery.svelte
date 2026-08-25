<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';

  import type { APIClient } from '../../api/client';
  import type { PersonFileSearchRow } from '../../explore/models';
  import MediaThumbnail from './MediaThumbnail.svelte';

  interface Props {
    client: APIClient;
    rows: PersonFileSearchRow[];
    loading?: boolean;
    loadingMore?: boolean;
    hasMore?: boolean;
    error?: string;
    pageError?: string;
    onOpen?: (row: PersonFileSearchRow, returnFocus: HTMLElement) => void;
    onLoadMore?: () => void;
    onReload?: () => void;
  }

  let {
    client, rows, loading = false, loadingMore = false, hasMore = false, error = '',
    pageError = '', onOpen = undefined, onLoadMore = undefined, onReload = undefined
  }: Props = $props();
</script>

<section class="media-gallery" aria-label="Person media gallery" aria-busy={loading || loadingMore}>
  {#if loading && rows.length === 0}
    <p class="notice" role="status">Loading media…</p>
  {:else if error && rows.length === 0}
    <p class="notice error" role="alert">{error}</p>
  {:else if rows.length === 0}
    <p class="notice">No media match this view.</p>
  {:else}
    <div class="cards">
      {#each rows as row (row.key)}
        <button
          type="button"
          class="media-card"
          aria-label={`Open ${row.filename || `attachment ${row.id}`}`}
          onclick={(event) => onOpen?.(row, event.currentTarget)}
        >
          <MediaThumbnail {client} file={row} />
          <span class="card-copy">
            <strong>{row.filename || '(unnamed)'}</strong>
            <span><time datetime={row.occurred_at}>{formatDate(row.occurred_at)}</time><span aria-hidden="true"> · </span><span>{row.source_identifier}</span></span>
            <span>{relationship(row)}</span>
            <span>{row.containing_title || row.entry_key}</span>
          </span>
        </button>
      {/each}
    </div>
  {/if}

  {#if error && rows.length > 0}
    <div class="page-error" role="alert">
      <span>{error}</span>
      <Button size="sm" label="Reload media" onclick={() => onReload?.()} />
    </div>
  {:else if pageError}
    <div class="page-error" role="alert">
      <span>{pageError}</span>
      <Button size="sm" label="Retry loading more media" disabled={loadingMore} onclick={() => onLoadMore?.()} />
    </div>
  {:else if hasMore}
    <Button class="load-more" label={loadingMore ? 'Loading more…' : 'Load more media'} disabled={loadingMore} onclick={() => onLoadMore?.()} />
  {/if}
</section>

<script lang="ts" module>
  function formatDate(value: string): string {
    const date = new Date(value);
    return Number.isNaN(date.valueOf())
      ? value
      : new Intl.DateTimeFormat(undefined, { dateStyle: 'medium' }).format(date);
  }

  function relationship(row: PersonFileSearchRow): string {
    const labels: string[] = [];
    if (row.person_provenance.directions?.includes('from_person')) labels.push('From them');
    if (row.person_provenance.directions?.includes('to_person')) {
      const roles = (row.person_provenance.roles ?? [])
        .filter((role) => role === 'to' || role === 'cc' || role === 'bcc');
      labels.push(`To them${roles.length ? ` (${roles.join(', ')})` : ''}`);
    }
    if (row.person_provenance.directions?.includes('group')) labels.push('Group conversation');
    return labels.join(' · ') || 'Relationship unavailable';
  }
</script>

<style>
  .media-gallery { display: flex; min-height: 0; flex: 1; flex-direction: column; gap: var(--space-4); overflow: auto; }
  .cards { display: grid; grid-template-columns: repeat(auto-fill, minmax(190px, 1fr)); gap: var(--space-3); align-content: start; }
  .media-card { display: flex; min-width: 0; padding: 0; overflow: hidden; flex-direction: column; border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-surface); color: inherit; cursor: pointer; font: inherit; text-align: left; }
  .media-card:hover { border-color: var(--border-strong); box-shadow: var(--shadow-sm); }
  .media-card:focus-visible { outline: 2px solid var(--accent-blue); outline-offset: 2px; }
  .card-copy { display: flex; min-width: 0; gap: var(--space-1); padding: var(--space-3); flex-direction: column; }
  .card-copy strong, .card-copy span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .card-copy strong { color: var(--text-primary); font-size: var(--font-size-sm); }
  .card-copy span { color: var(--text-muted); font-size: var(--font-size-2xs); }
  .notice { display: grid; min-height: 220px; place-items: center; color: var(--text-secondary); }
  .error, .page-error { color: var(--text-danger); }
  .page-error { display: flex; align-items: center; justify-content: center; gap: var(--space-3); }
  :global(.load-more) { align-self: center; }
</style>
