<script lang="ts">
  import { StatusDot, Table, TableHeaderCell } from '@kenn-io/kit-ui';

  import type { OperationKind, OperationRunSummary } from '../../operations/models';
  import { orderedOperationRows } from '../../operations/focus';

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

  const kindLabels: Record<OperationKind, string> = {
    source_sync: 'Source sync',
    message_embedding: 'Message embedding',
    person_sweep: 'Person fact sweep',
    person_embedding: 'Person embedding',
    person_enrichment: 'Person enrichment',
    carddav_sync: 'CardDAV sync',
    document_extraction: 'Document extraction',
    document_embedding: 'Document embedding',
    visual_embedding: 'Visual embedding'
  };
  const sortedRows = $derived(orderedOperationRows(rows));

  function titleCase(value: string | undefined): string {
    if (!value) return 'Unspecified';
    return value.charAt(0).toUpperCase() + value.slice(1);
  }

  function dotStatus(run: OperationRunSummary) {
    if (run.state === 'running') return 'working' as const;
    if (run.state === 'queued') return 'waiting' as const;
    if (run.state === 'succeeded') return 'idle' as const;
    if (run.state === 'failed') return 'unclean' as const;
    return 'stale' as const;
  }

  function formatTimestamp(value: string): string {
    const parsed = new Date(value);
    if (!Number.isFinite(parsed.getTime())) return 'Time unavailable';
    return new Intl.DateTimeFormat(undefined, {
      dateStyle: 'medium', timeStyle: 'short'
    }).format(parsed);
  }

  function duration(run: OperationRunSummary): string {
    if (!run.finished_at) return run.state === 'running' ? 'In progress' : 'Not available';
    const milliseconds = Date.parse(run.finished_at) - Date.parse(run.started_at);
    if (!Number.isFinite(milliseconds) || milliseconds < 0) return 'Not available';
    const totalSeconds = Math.floor(milliseconds / 1_000);
    const minutes = Math.floor(totalSeconds / 60);
    const seconds = totalSeconds % 60;
    if (minutes === 0) return `${seconds} ${seconds === 1 ? 'second' : 'seconds'}`;
    return `${minutes} ${minutes === 1 ? 'minute' : 'minutes'}${seconds ? ` ${seconds} ${seconds === 1 ? 'second' : 'seconds'}` : ''}`;
  }

  function counters(run: OperationRunSummary): string {
    if (run.counters.length === 0) return 'No counters';
    return run.counters.map((counter) =>
      `${counter.value.toLocaleString()} ${counter.name.replaceAll('_', ' ')} ${counter.unit}`
    ).join(' · ');
  }
</script>

{#if narrow}
  <div class="run-list" role="list" aria-label="Operation history">
    {#each sortedRows as run (run.id)}
      <div role="listitem" class:selected={selectedID === run.id}>
        <button
          type="button"
          data-run-id={run.id}
          aria-label={`Open ${kindLabels[run.kind]} run`}
          aria-current={selectedID === run.id ? 'true' : undefined}
          onclick={(event) => onSelect(run.id, event.currentTarget)}
        >
          <span class="row-title">{kindLabels[run.kind]}</span>
          <span><span aria-hidden="true"><StatusDot status={dotStatus(run)} /></span> {titleCase(run.state)}</span>
          <span>{titleCase(run.trigger)} · <time datetime={run.started_at}>{formatTimestamp(run.started_at)}</time></span>
          <span>{duration(run)} · {counters(run)}</span>
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
              aria-label={`Open ${kindLabels[run.kind]} run`}
              onclick={(event) => onSelect(run.id, event.currentTarget)}
            >{kindLabels[run.kind]}</button>
          </td>
          <td>{titleCase(run.trigger)}</td>
          <td><span class="state"><span aria-hidden="true"><StatusDot status={dotStatus(run)} /></span> {titleCase(run.state)}</span></td>
          <td><time datetime={run.started_at}>{formatTimestamp(run.started_at)}</time></td>
          <td>{duration(run)}</td>
          <td>{counters(run)}</td>
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
  .state { display: inline-flex; align-items: center; gap: var(--space-2); white-space: nowrap; }
  .run-list { display: grid; gap: var(--space-2); }
  .run-list [role="listitem"] { border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-surface); }
  .run-list [role="listitem"].selected { border-color: var(--accent-blue); }
  .run-list button { display: grid; width: 100%; gap: var(--space-1); padding: var(--space-3); border: 0; background: transparent; color: inherit; text-align: left; }
  .run-list button span { color: var(--text-muted); font-size: var(--font-size-xs); }
  .run-list button .row-title { color: var(--text-primary); font-size: var(--font-size-sm); font-weight: 700; }
</style>
