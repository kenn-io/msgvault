<script lang="ts">
  interface Row { id: number | string; value: string; detail?: string | null; meta?: string }
  interface Props { title: string; level?: 3 | 5; rows: Row[]; earlier?: Row[]; earlierNoun?: 'value' | 'decision' }
  let { title, level = 3, rows, earlier = [], earlierNoun = 'value' }: Props = $props();
</script>

{#snippet row(item: Row)}
  <li>
    <span class="value">{item.value}{#if item.detail}<small>{item.detail}</small>{/if}</span>
    {#if item.meta}<span class="meta">{item.meta}</span>{/if}
  </li>
{/snippet}
<section>
  <svelte:element this={`h${level}`}>{title}</svelte:element>
  {#if rows.length}<ul>{#each rows as item (item.id)}{@render row(item)}{/each}</ul>{/if}
  {#if earlier.length}
    <details>
      <summary>{earlier.length} earlier {earlierNoun}{earlier.length === 1 ? '' : 's'}</summary>
      <ul>{#each earlier as item (item.id)}{@render row(item)}{/each}</ul>
    </details>
  {/if}
</section>

<style>
  section {
    display: grid;
    gap: var(--space-2);
  }
  h3, h5, ul {
    margin: 0;
  }
  ul {
    display: grid;
    padding: 0;
    list-style: none;
  }
  li {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--field-gap);
    padding: var(--space-2) 0;
    border-bottom: 1px solid var(--border-muted);
  }
  .value {
    display: grid;
    min-width: 0;
    overflow-wrap: anywhere;
  }
  .meta {
    min-width: 0;
    max-width: 100%;
    overflow-wrap: anywhere;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  small {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  details summary {
    padding: var(--space-2) 0;
    color: var(--text-secondary);
    cursor: pointer;
    font-size: var(--font-size-sm);
  }
  details li {
    flex-direction: column;
    gap: var(--space-1);
    color: var(--text-secondary);
  }
  @media (max-width: 640px) {
    li { flex-direction: column; gap: var(--space-1); }
  }
</style>
