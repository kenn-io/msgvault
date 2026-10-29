<script lang="ts">
  import { Chip } from '@kenn-io/kit-ui';

  import type { OperationRunSummary } from '../../operations/models';
  import { orderedOperationRows } from '../../operations/focus';
  import {
    OPERATION_KIND_LABELS,
    counterSummary,
    operationStateChip,
    operationDuration,
    triggerLabel
  } from '../../operations/labels';
  import { formatDateTime } from '../../util/format';

  let {
    rows,
    onSelect = () => undefined
  }: {
    rows: readonly OperationRunSummary[];
    onSelect?: (id: string, button: HTMLButtonElement) => void;
  } = $props();

  const sortedRows = $derived(orderedOperationRows(rows));
</script>

  <ul class="compact-runs" aria-label="Operation history">
    {#each sortedRows as run (run.id)}
      {@const chip = operationStateChip(run.state)}
      <li><button type="button" data-run-id={run.id} aria-label={`Open ${OPERATION_KIND_LABELS[run.kind]} run`} onclick={(event) => onSelect(run.id, event.currentTarget)}>
        <span class="compact-runs__text"><span class="compact-runs__name">{OPERATION_KIND_LABELS[run.kind]} <time datetime={run.started_at}>{formatDateTime(run.started_at)}</time></span>
          <span class="compact-runs__meta">{run.error && run.state !== 'succeeded' ? run.error.message : [run.connection, triggerLabel(run.trigger), operationDuration(run), counterSummary(run.counters)].filter(Boolean).join(' · ')}</span></span>
        <Chip size="sm" tone={chip.tone} uppercase={false}>{chip.label}</Chip>
      </button></li>
    {/each}
  </ul>
<style>
  .compact-runs { margin: 0; padding: 0; list-style: none; }
  .compact-runs li + li { border-top: 1px solid var(--border-muted); }
  .compact-runs button { display: flex; width: 100%; align-items: center; gap: var(--control-gap); padding: var(--space-2) var(--space-1); border: 0; background: transparent; color: var(--text-primary); text-align: left; cursor: pointer; }
  .compact-runs button:hover { background: var(--bg-surface-hover); }
  .compact-runs button:focus-visible { outline: var(--focus-ring); outline-offset: -2px; }
  .compact-runs__text { display: grid; min-width: 0; flex: 1; gap: var(--label-gap); }
  .compact-runs__name { font-size: var(--font-size-sm); font-weight: var(--font-weight-medium); }
  .compact-runs__meta { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .compact-runs__meta, .compact-runs time { color: var(--text-muted); font-size: var(--font-size-sm); font-weight: var(--font-weight-normal); }

</style>
