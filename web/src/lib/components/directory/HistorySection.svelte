<script lang="ts" module>
  import type { ProvenanceLine } from './history-format';

  /** A value row: the value, then one muted line of its context and provenance. */
  export interface HistoryRow {
    id: number | string;
    value: string;
    detail?: string | null;
    provenance?: ProvenanceLine;
    /** A value that came from elsewhere, such as a merge, opens where it came from. */
    onclick?: () => void;
    actionLabel?: string;
  }
</script>

<script lang="ts">
  import { Disclosure } from '@kenn-io/kit-ui';
  import type { Snippet } from 'svelte';
  import DetailRow from '../people/DetailRow.svelte';

  interface Props {
    title: string;
    /** Current values, each a row. */
    rows: HistoryRow[];
    /** Ended values, folded under "N earlier values". */
    earlier?: HistoryRow[];
    earlierNoun?: string;
    /** A row's own actions, such as Reveal. */
    rowActions?: Snippet<[HistoryRow]>;
    /** Draws a row's value, such as a hidden sensitive value; the default is its text. */
    value?: Snippet<[HistoryRow]>;
  }

  let { title, rows, earlier = [], earlierNoun = 'value', rowActions, value }: Props = $props();
  const headingID = $props.id();
</script>

{#snippet row(item: HistoryRow)}
  {@const line = [item.detail, item.provenance?.text].filter(Boolean).join(' · ')}
  {@const description = line ? { text: line, title: item.provenance?.title ?? '' } : undefined}
  {#if rowActions}
    <DetailRow {description} onclick={item.onclick} actionLabel={item.actionLabel}>
      {#if value}{@render value(item)}{:else}{item.value}{/if}
      {#snippet actions()}{@render rowActions(item)}{/snippet}
    </DetailRow>
  {:else}
    <DetailRow {description} onclick={item.onclick} actionLabel={item.actionLabel}>{#if value}{@render value(item)}{:else}{item.value}{/if}</DetailRow>
  {/if}
{/snippet}

<!-- A group: its heading 8 above its rows, then the fold of earlier values right after the last row. -->
<section class="history-section" aria-labelledby={headingID}>
  <h3 id={headingID}>{title}</h3>
  {#each rows as item (item.id)}{@render row(item)}{/each}
  {#if earlier.length}
    <Disclosure variant="plain" class="fold" title={`${earlier.length} earlier ${earlierNoun}${earlier.length === 1 ? '' : 's'}`}>
      {#each earlier as item (item.id)}{@render row(item)}{/each}
    </Disclosure>
  {/if}
</section>

<style>
  .history-section { display: grid; min-width: 0; }
  h3 {
    margin: 0 0 var(--space-group);
    font-size: var(--type-heading-size);
    line-height: var(--type-heading-line);
  }
</style>
