<script lang="ts">
  import { Button, Chip } from '@kenn-io/kit-ui';
  import BuildIndexAction from '../search/BuildIndexAction.svelte';

  import type { OverviewAction, OverviewRow } from '../../settings/overview';

  let {
    rows,
    working = {},
    onSetUp,
    onStart,
    onSources,
  }: {
    rows: readonly OverviewRow[];
    /** Start actions in progress; each button waits for its own action. */
    working?: Readonly<Record<string, boolean>>;
    onSetUp: (action: Extract<OverviewAction, { kind: 'settings' }>) => void;
    onSources: () => void;
    onStart: (action: Extract<OverviewAction, { kind: 'start' }>) => void;
  } = $props();

</script>

<ul class="overview" aria-label="Features">
  {#each rows as row (row.id)}
    {@const action = row.action}
    <li class="row" data-feature={row.id}>
      <span class="row__name">
        {row.name}
      </span>
      <span class="row__chip"><Chip size="xs" tone={row.chip.tone} uppercase={false}>{row.chip.text}</Chip></span>
      <span class="row__action">
        {#if action?.kind === 'settings'}
          <Button size="sm" label="Set up" ariaLabel={`Set up ${row.name}`} onclick={() => onSetUp(action)} />
        {:else if action?.kind === 'start' && action.id === 'messages'}
          {#if working[action.id]}
            <Button size="sm" surface="soft" tone="info" ariaLabel="Build index, Message search" label="Working…" disabled />
          {:else}
            <BuildIndexAction size="sm" surface="soft" ariaLabel="Build index, Message search" disabled={Boolean(working[action.id])} onconfirm={() => onStart(action)} />
          {/if}
        {:else if action?.kind === 'start'}
          <Button
            size="sm"
            tone="info"
            surface="soft"
            label={working[action.id] ? 'Working…' : action.label}
            ariaLabel={`${action.label}, ${row.name}`}
            disabled={Boolean(working[action.id])}
            onclick={() => onStart(action)}
          />
        {:else if action?.kind === 'sources'}
          <Button size="sm" label="Open Sources status" onclick={onSources} />
        {:else if action?.kind === 'host'}
          <a class="row__guide" href={action.guideHref} target="_blank" rel="noreferrer">{action.guideLabel}</a>
        {/if}
      </span>
      {#if action?.kind === 'host'}<span class="row__consent">{action.text}</span>{/if}
      {#if action?.kind === 'start' && action.consent}<span class="row__consent">{action.consent}</span>{/if}
    </li>
  {/each}
</ul>

<style>
  .overview {
    display: grid;
    grid-template-columns: minmax(max-content, 1fr) max-content minmax(0, max-content);
    margin: 0;
    padding: 0;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    list-style: none;
  }
  .row {
    display: grid;
    grid-column: 1 / -1;
    grid-template-columns: subgrid;
    align-items: center;
    gap: var(--control-gap) var(--section-gap);
    min-height: var(--control-height);
    padding: var(--pane-pad-y) var(--pane-pad-x);
  }
  .row + .row {
    border-top: 1px solid var(--border-default);
  }
  .row__name {
    display: flex;
    align-items: center;
    gap: var(--control-gap);
    min-width: 0;
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-medium);
  }
  .row__chip {
    display: flex;
    align-items: center;
  }
  .row__action {
    display: flex;
    min-width: 0;
    align-items: center;
    justify-content: flex-end;
  }
  .row__guide {
    color: var(--link-ink);
    font-size: var(--font-size-sm);
    line-height: 1.45;
  }
  .row__consent {
    grid-column: 1 / -1;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    line-height: 1.45;
  }

  @media (max-width: 640px) {
    .overview {
      grid-template-columns: minmax(0, 1fr) max-content;
    }
    .row__action {
      grid-column: 1 / -1;
      justify-content: flex-start;
    }
  }
</style>
