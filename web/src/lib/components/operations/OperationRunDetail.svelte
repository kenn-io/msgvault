<script lang="ts">
  import { Button, Chip } from '@kenn-io/kit-ui';

  import {
    OPERATION_ACTION_LABELS,
    OPERATION_KIND_LABELS,
    RELATED_STATUS_LABELS,
    counterLabel,
    counterValue,
    operationDuration,
    operationStateChip,
    triggerLabel,
    type RelatedStatus
  } from '../../operations/labels';
  import type { OperationAction, OperationRunDetail } from '../../operations/models';
  import { formatDateTime } from '../../util/format';

  let {
    detail,
    actionPending = null,
    showClose = true,
    onClose = () => undefined,
    onNavigate = () => undefined,
    onAction = () => undefined
  }: {
    detail: OperationRunDetail;
    actionPending?: OperationAction | null;
    showClose?: boolean;
    onClose?: () => void;
    onNavigate?: (target: RelatedStatus, button: HTMLButtonElement) => void;
    onAction?: (action: OperationAction) => void;
  } = $props();

  const stateChip = $derived(operationStateChip(detail.state));
</script>

<section class="detail" aria-label="Operation run detail">
  <header>
    <div>
      <p>Run detail</p>
      <h2>{OPERATION_KIND_LABELS[detail.kind]}</h2>
    </div>
    {#if showClose}<Button size="sm" surface="soft" label="Close operation detail" onclick={onClose} />{/if}
  </header>

  <dl class="facts">
    {#if detail.connection}<div><dt>Connection</dt><dd>{detail.connection}</dd></div>{/if}
    <div><dt>State</dt><dd><Chip size="sm" tone={stateChip.tone} uppercase={false}>{stateChip.label}</Chip></dd></div>
    <div><dt>Trigger</dt><dd>{triggerLabel(detail.trigger)}</dd></div>
    <div><dt>Started</dt><dd><time datetime={detail.started_at}>{formatDateTime(detail.started_at, 'long')}</time></dd></div>
    <div><dt>Finished</dt><dd>{#if detail.finished_at}<time datetime={detail.finished_at}>{formatDateTime(detail.finished_at, 'long')}</time>{:else}Not available{/if}</dd></div>
    <div><dt>Duration</dt><dd>{operationDuration(detail)}</dd></div>
  </dl>

  <section aria-labelledby="detail-counters-heading">
    <h3 id="detail-counters-heading">Counters</h3>
    {#if detail.counters.length === 0}
      <p class="muted">No counters reported.</p>
    {:else}
      <dl class="counters">
        {#each detail.counters as counter (`${counter.name}:${counter.unit}`)}
          <div><dt>{counterLabel(counter.name)}</dt><dd>{counterValue(counter)}</dd></div>
        {/each}
      </dl>
    {/if}
  </section>

  {#if detail.error}
    <section class="error" role="alert" aria-label="Operation error">
      <p>{detail.error.message}</p>
      <code>Code: {detail.error.code}</code>
    </section>
  {/if}

  {#if detail.related_status || detail.supported_actions.length > 0}
    <div class="actions">
      {#if detail.related_status}
        <Button
          size="sm"
          surface="soft"
          label={`Open ${RELATED_STATUS_LABELS[detail.related_status]}`}
          onclick={(event) => onNavigate(detail.related_status!, event.currentTarget as HTMLButtonElement)}
        />
      {/if}
      {#each detail.supported_actions as action (action)}
        <Button
          size="sm"
          tone="info"
          label={actionPending === action ? `${OPERATION_ACTION_LABELS[action]}…` : OPERATION_ACTION_LABELS[action]}
          disabled={actionPending !== null}
          onclick={() => onAction(action)}
        />
      {/each}
    </div>
  {/if}
</section>

<style>
  .detail { display: grid; align-content: start; gap: var(--space-4); min-width: 0; padding: var(--space-4); }
  header { display: flex; align-items: start; justify-content: space-between; gap: var(--space-3); }
  h2, h3, p, dl, dd { margin: 0; }
  header p { color: var(--text-muted); font-size: var(--font-size-2xs); font-weight: 600; }
  h2 { font-size: var(--font-size-lg); }
  h3 { margin-bottom: var(--space-2); font-size: var(--font-size-sm); }
  .facts, .counters { display: grid; gap: var(--space-2); }
  .facts div, .counters div { display: grid; grid-template-columns: minmax(100px, .45fr) 1fr; gap: var(--space-3); }
  dt { color: var(--text-muted); font-size: var(--font-size-xs); }
  dd { display: flex; align-items: center; gap: var(--space-2); font-size: var(--font-size-sm); }
  .error { display: grid; gap: var(--space-1); padding: var(--space-3); border: 1px solid var(--status-error-ink); border-radius: var(--radius-md); background: var(--status-error-bg); color: var(--status-error-ink); }
  .actions { display: flex; flex-wrap: wrap; gap: var(--space-2); }
  .muted { color: var(--text-muted); font-size: var(--font-size-sm); }
</style>
