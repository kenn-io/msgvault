<script lang="ts">
  import { Chip, Table, TableHeaderCell } from '@kenn-io/kit-ui';

  import type { OperationRunSummary } from '../../operations/models';
  import { orderedOperationRows } from '../../operations/focus';
  import {
    OPERATION_KIND_LABELS,
    counterSummary,
    operationDuration,
    operationStateChip,
    triggerLabel
  } from '../../operations/labels';
  import { formatDateTime } from '../../util/format';

  let {
    rows,
    selectedID = null,
    narrow = false,
    onSelect = () => undefined
  }: {
    rows: readonly OperationRunSummary[];
    selectedID?: string | null;
    narrow?: boolean;
    onSelect?: (id: string, button: HTMLButtonElement) => void;
  } = $props();

  const sortedRows = $derived(orderedOperationRows(rows));
</script>

{#snippet runState(run: OperationRunSummary)}
  {@const chip = operationStateChip(run.state)}
  <span class="state">
    <Chip size="sm" tone={chip.tone} uppercase={false}>{chip.label}</Chip>
    {#if run.error && (run.state === 'failed' || run.state === 'partial')}<span class="error">{run.error.message}</span>{/if}
  </span>
{/snippet}

{#if narrow}
  <div class="run-list" role="list" aria-label="Operation history">
    {#each sortedRows as run (run.id)}
      <div role="listitem" class:selected={selectedID === run.id}>
        <button
          type="button"
          data-run-id={run.id}
          aria-label={`Open ${OPERATION_KIND_LABELS[run.kind]} run`}
          aria-current={selectedID === run.id ? 'true' : undefined}
          onclick={(event) => onSelect(run.id, event.currentTarget)}
        >
          <span class="row-title">{OPERATION_KIND_LABELS[run.kind]}{#if run.connection} · {run.connection}{/if}</span>
          {@render runState(run)}
          <span>{triggerLabel(run.trigger)} · <time datetime={run.started_at}>{formatDateTime(run.started_at)}</time></span>
          <span>{operationDuration(run)} · {counterSummary(run.counters)}</span>
        </button>
      </div>
    {/each}
  </div>
{:else}
  <!-- svelte-ignore a11y_no_noninteractive_tabindex (keyboard users need to reach the overflow region) -->
  <div class="table-scroll" role="region" aria-label="Scrollable operation history" tabindex="0">
    <Table ariaLabel="Operation history" zebra={false} class="run-table">
      {#snippet header()}
        <TableHeaderCell label="Kind" />
        <TableHeaderCell label="Trigger" />
        <TableHeaderCell label="State" />
        <TableHeaderCell label="Started" />
        <TableHeaderCell label="Duration" />
        <TableHeaderCell label="Counters" />
      {/snippet}
      {#each sortedRows as run (run.id)}
        <tr aria-selected={selectedID === run.id}>
          <td>
            <button
              class="run-link"
              type="button"
              data-run-id={run.id}
              aria-label={`Open ${OPERATION_KIND_LABELS[run.kind]} run`}
              onclick={(event) => onSelect(run.id, event.currentTarget)}
            >{OPERATION_KIND_LABELS[run.kind]}{#if run.connection} · {run.connection}{/if}</button>
          </td>
          <td>{triggerLabel(run.trigger)}</td>
          <td>{@render runState(run)}</td>
          <td><time datetime={run.started_at}>{formatDateTime(run.started_at)}</time></td>
          <td>{operationDuration(run)}</td>
          <td>{counterSummary(run.counters)}</td>
        </tr>
      {/each}
    </Table>
  </div>
{/if}

<style>
  .table-scroll { min-width: 0; overflow: auto; border: 1px solid var(--border-muted); border-radius: var(--radius-md); }
  .table-scroll:focus-visible { outline: var(--focus-ring); outline-offset: var(--focus-ring-offset, 2px); }
  .table-scroll :global(.run-table) { overflow: visible; flex: none; }
  :global(.run-table .kit-table) { min-width: 900px; }
  :global(.run-table td) { vertical-align: middle; font-size: var(--font-size-xs); }
  .run-link {
    padding: 0;
    border: 0;
    background: transparent;
    color: var(--interactive-text);
    font: inherit;
    font-weight: 700;
    cursor: pointer;
  }
  .run-link:focus-visible { outline: var(--focus-ring); outline-offset: 2px; }
  .state { display: grid; justify-items: start; gap: var(--space-1); }
  .state .error { color: var(--status-error-ink); }
  .run-list { display: grid; gap: var(--space-2); }
  .run-list [role="listitem"] { border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-surface); }
  .run-list [role="listitem"].selected { border-color: var(--accent-blue); }
  .run-list button { display: grid; width: 100%; gap: var(--space-1); padding: var(--space-3); border: 0; background: transparent; color: inherit; text-align: left; }
  .run-list button span { color: var(--text-muted); font-size: var(--font-size-xs); }
  .run-list button .row-title { color: var(--text-primary); font-size: var(--font-size-sm); font-weight: 700; }
</style>
