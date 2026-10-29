<script lang="ts">
  import { Chip, MenuItem } from '@kenn-io/kit-ui';

  import type { JobsStore } from '../../jobs/jobs.svelte';
  import type { JobTarget } from '../../jobs/jobs';
  import { formatRelativeTime } from '../../util/format';
  import { operationStateChip } from '../../operations/labels';

  let {
    store,
    onOpen,
  }: {
    store: JobsStore;
    /** Opens the source or setting a job belongs to. */
    onOpen: (target: JobTarget) => void;
  } = $props();
</script>

{#if store.error}<p class="empty" role="alert">{store.error}</p>{/if}
{#if store.items.length === 0 && !store.error}
  <p class="empty">{store.loaded ? 'Nothing running, nothing failed' : 'Loading…'}</p>
{:else}
  {#each store.items as item (item.id)}
    {@const chip = operationStateChip(item.state)}
    <MenuItem
      textValue={item.name}
      onselect={() => onOpen(item.target)}
    >
      <span class="job">
        <span class="job__text">
          <span class="job__name">{item.name}</span>
          <span class="job__detail">{item.at ? `${item.detail} · ${formatRelativeTime(item.at)}` : item.detail}</span>
        </span>
        <Chip size="sm" tone={chip.tone} uppercase={false}>{chip.label}</Chip>
      </span>
    </MenuItem>
  {/each}
{/if}

<style>
  .empty {
    margin: 0;
    padding: var(--pane-pad-y) var(--pane-pad-x);
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  .job {
    display: flex;
    min-width: 260px;
    max-width: 340px;
    align-items: center;
    gap: var(--control-gap);
    padding: var(--space-2) 0;
  }
  .job__text {
    display: grid;
    min-width: 0;
    flex: 1;
    gap: var(--label-gap);
  }
  .job__name {
    overflow: hidden;
    color: var(--text-primary);
    font-weight: var(--font-weight-medium);
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .job__detail {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    white-space: normal;
  }
</style>
