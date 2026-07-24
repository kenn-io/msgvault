<script lang="ts">
  import type { ExploreFileFact } from '../../explore/models';

  let {
    files,
    totalCount = 0,
    loading = false,
    error = ''
  }: {
    files: ExploreFileFact[];
    totalCount?: number;
    loading?: boolean;
    error?: string;
  } = $props();

  const chronological = $derived([...files].sort((left, right) =>
    Date.parse(left.occurred_at) - Date.parse(right.occurred_at) || left.key.localeCompare(right.key)
  ));

  function bytes(value: number): string {
    if (value < 1024) return `${value} B`;
    if (value < 1024 * 1024) return `${Math.round(value / 1024)} KB`;
    return `${(value / (1024 * 1024)).toFixed(1)} MB`;
  }

  function date(value: string): string {
    const parsed = new Date(value);
    return Number.isNaN(parsed.valueOf()) ? value : parsed.toLocaleDateString();
  }
</script>

<section class="attachment-rail" aria-label="Chronological files">
  <header>
    <h2>Files</h2>
    {#if totalCount > 0}<span>{Math.min(files.length, totalCount)} of {totalCount.toLocaleString()}</span>{/if}
  </header>
  {#if loading}
    <p role="status">Loading file facts…</p>
  {:else if error}
    <p role="alert">{error}</p>
  {:else if chronological.length === 0}
    <p>No files in this context.</p>
  {:else}
    <ol>
      {#each chronological as file (file.key)}
        <li>
          <div><strong>{file.filename || '(unnamed file)'}</strong><span>{bytes(file.size)}</span></div>
          <div><time datetime={file.occurred_at} data-mono>{date(file.occurred_at)}</time><span>{file.title || file.source_identifier}</span></div>
        </li>
      {/each}
    </ol>
    {#if totalCount > files.length}
      <p class="bounded-note">
        Showing a bounded sample of {files.length.toLocaleString()} of {totalCount.toLocaleString()} files.
      </p>
    {/if}
  {/if}
</section>

<style>
  .attachment-rail {
    display: grid;
    gap: var(--space-3);
    padding: var(--space-5);
    border-top: 1px solid var(--border-default);
  }

  header,
  li > div {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
  }

  h2,
  p {
    margin: 0;
  }

  h2 {
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }

  header span,
  p,
  li > div:last-child {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  ol {
    display: grid;
    gap: 0;
    margin: 0;
    padding: 0;
    list-style: none;
  }

  li {
    display: grid;
    gap: var(--space-1);
    padding: var(--space-3) 0;
    border-top: 1px solid var(--border-muted);
  }

  li strong,
  li span {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  li strong {
    color: var(--text-primary);
    font-size: var(--font-size-xs);
  }

  .bounded-note {
    padding-top: var(--space-2);
    border-top: 1px solid var(--border-muted);
  }
</style>
