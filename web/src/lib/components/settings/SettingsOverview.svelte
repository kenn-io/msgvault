<script lang="ts">
  import { Button, Chip, SearchByMeaningRow } from '@kenn-io/kit-ui';
  import type { SearchByMeaningState, SearchByMeaningLabels } from '@kenn-io/kit-ui';
  import BuildIndexAction from '../search/BuildIndexAction.svelte';

  import type { OperationStatusAuthority } from '../../explore/models';
  import type { OverviewAction, OverviewRow } from '../../settings/overview';
  import { sentence } from '../../settings/local-embeddings';

  let {
    rows,
    localSearch,
    localLabels,
    localError = '',
    onLocalSearch = () => undefined,
    working = {},
    onSetUp,
    onStart,
    onSources,
    onDetails = () => undefined,
  }: {
    rows: readonly OverviewRow[];
    localSearch?: SearchByMeaningState;
    localLabels?: Partial<SearchByMeaningLabels>;
    localError?: string;
    onLocalSearch?: () => void;
    onDetails?: (authority: OperationStatusAuthority) => void;
    /** Start actions in progress; each button waits for its own action. */
    working?: Readonly<Record<string, boolean>>;
    onSetUp: (action: Extract<OverviewAction, { kind: 'settings' }>) => void;
    onSources: () => void;
    onStart: (action: Extract<OverviewAction, { kind: 'start' }>) => void;
  } = $props();

</script>

{#snippet rowActions(action: OverviewAction | undefined, authority: OverviewRow['relatedStatus'], name: string)}
      <span class="row__action">
        {#if authority}<Button size="sm" surface="soft" label="Details" ariaLabel={`Details, ${name}`} onclick={() => onDetails(authority)} />{/if}
        {#if action?.kind === 'settings'}
          <Button size="sm" label="Set up" ariaLabel={`Set up ${name}`} onclick={() => onSetUp(action)} />
        {:else if action?.kind === 'start' && action.id === 'messages'}
          {#if working[action.id]}
            <Button size="sm" surface="soft" tone="info" ariaLabel={`Build index, ${name}`} label="Working…" disabled />
          {:else}
            <BuildIndexAction size="sm" surface="soft" ariaLabel={`Build index, ${name}`} onconfirm={() => onStart(action)} />
          {/if}
        {:else if action?.kind === 'start'}
          <Button
            size="sm"
            tone="info"
            surface="soft"
            label={working[action.id] ? 'Working…' : action.label}
            ariaLabel={`${action.label}, ${name}`}
            disabled={Boolean(working[action.id])}
            onclick={() => onStart(action)}
          />
        {:else if action?.kind === 'sources'}
          <Button size="sm" label="Open Sources status" onclick={onSources} />
        {:else if action?.kind === 'host'}
          <a class="row__guide" href={action.guideHref} target="_blank" rel="noreferrer">{action.guideLabel}</a>
        {/if}
      </span>
{/snippet}

<ul class="overview" aria-label="Features">
  {#each rows as row (row.id)}
    {@const action = row.action}
    {@const authority = row.relatedStatus}
    {@const local = row.id === 'message_embedding' && localSearch}
    {@const enabled = local && (local.kind === 'on' || local.kind === 'ollama')}
    {@const recovery = enabled && (row.chip.text !== 'On' || action?.kind === 'start' || authority)}
    {@const error = row.id === 'message_embedding' && (!local || local.kind === 'off' || local.kind === 'downloaded' || (enabled && row.chip.text !== 'On')) ? sentence(localError) : ''}
    <li class="row" data-feature={row.id}>
      {#if local}
        <div class="row__local">
          <SearchByMeaningRow state={local} itemNoun="messages" labels={localLabels} readonly={local.kind === 'ollama'} onturnon={() => onLocalSearch()} onusebuiltin={() => undefined} />
          <span class="row__chip-sizer" aria-hidden="true">
            {#each [localLabels?.off ?? 'Off', localLabels?.on ?? 'On', localLabels?.ollama ?? 'On · Ollama', localLabels?.turningOn ?? 'Turning on'] as text}<Chip size="xs" uppercase={false}><span data-label={text}></span></Chip>{/each}
          </span>
        </div>
        {#if recovery}
          <div class="row__recovery">
            <div class="row__recovery-copy">
              <span class="row__coverage">Message index <Chip size="xs" tone={row.chip.tone} uppercase={false}>{row.chip.text}</Chip></span>
              {#if error}<p class="row__error" role="alert">{error}</p>{/if}
            </div>
            {@render rowActions(action?.kind === 'settings' ? undefined : action, authority, row.name)}
          </div>
        {/if}
      {:else}
      <span class="row__name">
        {row.name}
      </span>
      <span class="row__chip"><Chip size="xs" tone={row.chip.tone} uppercase={false}>{row.chip.text}</Chip></span>
      {@render rowActions(action, authority, row.name)}
      {#if action?.kind === 'host'}<span class="row__consent">{action.text}</span>{/if}
      {#if action?.kind === 'start' && action.consent}<span class="row__consent">{action.consent}</span>{/if}
      {/if}
      {#if error && !recovery}<p class="row__error" role="alert">{error}</p>{/if}
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
    /* Text stacked in a row sits closer than the row padding, so it reads as one group. */
    gap: var(--space-3) var(--section-gap);
    min-height: var(--control-height);
    padding: var(--pane-pad-y) var(--pane-pad-x);
  }
  .row + .row {
    border-top: 1px solid var(--border-default);
  }
  .row__local { display: grid; grid-column: 1 / -1; grid-row: 1; grid-template-columns: subgrid; min-width: 0; }
  .row__local :global(.kit-search-by-meaning) { grid-column: 1 / -1; grid-row: 1; grid-template-columns: subgrid; grid-template-rows: minmax(var(--control-h), auto); min-height: var(--control-h); gap: var(--control-gap) var(--section-gap); }
  .row__local :global(.kit-chip) { grid-column: 2; grid-row: 1; justify-self: start; }
  /* Reserves the widest local status so activation never shifts the status column. */
  .row__chip-sizer { display: flex; flex-direction: column; grid-column: 2; grid-row: 1; height: 0; overflow: hidden; visibility: hidden; }
  .row__chip-sizer [data-label]::before { content: attr(data-label); }
  .row__local :global(.kit-search-by-meaning__action) { grid-column: 3; grid-row: 1; justify-self: end; }
  .row__local :global(.kit-search-by-meaning__line), .row__local :global(.kit-search-by-meaning__warning) { grid-column: 1; contain: inline-size; text-wrap: balance; }
  .row__local :global(.kit-search-by-meaning:not(.kit-search-by-meaning--no-action) .kit-search-by-meaning__line), .row__local :global(.kit-search-by-meaning__warning) { margin-top: calc(var(--control-gap) / -2); }
  .row__recovery-copy { display: grid; grid-column: 1 / 3; gap: calc(var(--control-gap) / 2); }
  .row__recovery { margin-top: var(--control-gap); display: grid; grid-column: 1 / -1; grid-template-columns: subgrid; align-items: start; gap: var(--control-gap) var(--section-gap); }
  .row:has(:global(.kit-search-by-meaning--no-action)) .row__recovery { margin-top: calc((var(--font-size-md) * 1.45 - var(--control-h)) / 2); }
  .row__coverage { display: flex; align-items: center; gap: var(--control-gap); color: var(--text-muted); font-size: var(--font-size-xs); }
  .row__recovery .row__action { grid-column: 3; margin-top: calc((var(--font-size-xs) * 1.45 - var(--control-h)) / 2); }
  .row__error { grid-column: 1 / -1; margin: 0; color: var(--text-danger); font-size: var(--font-size-xs); contain: inline-size; }
  .row > .row__error { margin-top: calc(var(--space-3) / -2); }
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
    gap: var(--control-gap);
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
    .row__chip { justify-content: flex-end; }
    .row__local :global(.kit-search-by-meaning) { min-height: 0; grid-template-rows: none; }
    .row__local :global(.kit-chip) { justify-self: end; }
    .row__chip-sizer { display: none; }
    .row:has(:global(.kit-search-by-meaning--no-action)) .row__recovery { margin-top: 0; }
    .row__recovery-copy { grid-column: 1 / -1; }
    .row__recovery .row__action { grid-column: 1 / -1; margin-top: 0; }
    .row__local :global(.kit-search-by-meaning__action) { grid-column: 1 / -1; grid-row: 2; justify-self: start; }
    .row__local :global(.kit-search-by-meaning__line), .row__local :global(.kit-search-by-meaning__warning) { grid-column: 1 / -1; grid-row: 3; }
    .row__local :global(.kit-search-by-meaning__line), .row__local :global(.kit-search-by-meaning__warning) { text-wrap: pretty; }
    .row__local :global(.kit-search-by-meaning:not(.kit-search-by-meaning--no-action) .kit-search-by-meaning__line), .row__local :global(.kit-search-by-meaning__warning) { margin-top: 0; }
  }
</style>
