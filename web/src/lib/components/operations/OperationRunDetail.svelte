<script lang="ts">
  import { Button, StatusDot } from '@kenn-io/kit-ui';

  import type {
    OperationAction,
    OperationKind,
    OperationRunDetail,
    OperationRunSummary
  } from '../../operations/models';

  type RelatedStatus = NonNullable<OperationRunDetail['related_status']>;

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
  const relatedLabels: Record<RelatedStatus, string> = {
    listSourceStatus: 'Sources status',
    getDocumentIndexStatus: 'Document index status',
    getDocumentVectorStatus: 'Document vector status',
    getVisualAttachmentStatus: 'Visual attachment status',
    getCardDAVStatus: 'CardDAV settings'
  };
  const actionLabels: Record<OperationAction, string> = {
    carddav_sync: 'Start CardDAV sync',
    visual_build: 'Build visual index',
    visual_resume: 'Resume visual index'
  };

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

  function formatTimestamp(value: string | undefined): string {
    if (!value) return 'Not available';
    const parsed = new Date(value);
    if (!Number.isFinite(parsed.getTime())) return 'Not available';
    return new Intl.DateTimeFormat(undefined, {
      dateStyle: 'medium', timeStyle: 'long'
    }).format(parsed);
  }

  function duration(run: OperationRunDetail): string {
    if (!run.finished_at) return run.state === 'running' ? 'In progress' : 'Not available';
    const milliseconds = Date.parse(run.finished_at) - Date.parse(run.started_at);
    if (!Number.isFinite(milliseconds) || milliseconds < 0) return 'Not available';
    const totalSeconds = Math.floor(milliseconds / 1_000);
    const minutes = Math.floor(totalSeconds / 60);
    const seconds = totalSeconds % 60;
    return minutes
      ? `${minutes} ${minutes === 1 ? 'minute' : 'minutes'}${seconds ? ` ${seconds} ${seconds === 1 ? 'second' : 'seconds'}` : ''}`
      : `${seconds} ${seconds === 1 ? 'second' : 'seconds'}`;
  }
</script>

<section class="detail" aria-label="Operation run detail">
  <header>
    <div>
      <p>Run detail</p>
      <h2>{kindLabels[detail.kind]}</h2>
    </div>
    {#if showClose}<Button size="sm" surface="soft" label="Close operation detail" onclick={onClose} />{/if}
  </header>

  <dl class="facts">
    <div><dt>State</dt><dd><span aria-hidden="true"><StatusDot status={dotStatus(detail)} /></span> {titleCase(detail.state)}</dd></div>
    <div><dt>Trigger</dt><dd>{titleCase(detail.trigger)}</dd></div>
    <div><dt>Started</dt><dd><time datetime={detail.started_at}>{formatTimestamp(detail.started_at)}</time></dd></div>
    <div><dt>Finished</dt><dd>{#if detail.finished_at}<time datetime={detail.finished_at}>{formatTimestamp(detail.finished_at)}</time>{:else}Not available{/if}</dd></div>
    <div><dt>Duration</dt><dd>{duration(detail)}</dd></div>
  </dl>

  <section aria-labelledby="detail-counters-heading">
    <h3 id="detail-counters-heading">Counters</h3>
    {#if detail.counters.length === 0}
      <p class="muted">No counters reported.</p>
    {:else}
      <dl class="counters">
        {#each detail.counters as counter (`${counter.name}:${counter.unit}`)}
          <div><dt>{counter.name.replaceAll('_', ' ')}</dt><dd>{counter.value.toLocaleString()} {counter.unit}</dd></div>
        {/each}
      </dl>
    {/if}
  </section>

  {#if detail.error}
    <section class="error" role="alert" aria-label="Operation error">
      <strong>{detail.error.code}</strong>
      <span>{detail.error.message}</span>
    </section>
  {/if}

  {#if detail.related_status || detail.supported_actions.length > 0}
    <div class="actions">
      {#if detail.related_status}
        <Button
          size="sm"
          surface="soft"
          label={`Open ${relatedLabels[detail.related_status]}`}
          onclick={(event) => onNavigate(detail.related_status!, event.currentTarget as HTMLButtonElement)}
        />
      {/if}
      {#each detail.supported_actions as action (action)}
        <Button
          size="sm"
          tone="info"
          label={actionPending === action ? `${actionLabels[action]}…` : actionLabels[action]}
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
  header p { color: var(--text-muted); font-size: var(--font-size-2xs); font-weight: 800; letter-spacing: .08em; text-transform: uppercase; }
  h2 { font-size: var(--font-size-lg); }
  h3 { margin-bottom: var(--space-2); font-size: var(--font-size-sm); }
  .facts, .counters { display: grid; gap: var(--space-2); }
  .facts div, .counters div { display: grid; grid-template-columns: minmax(100px, .45fr) 1fr; gap: var(--space-3); }
  dt { color: var(--text-muted); font-size: var(--font-size-xs); text-transform: capitalize; }
  dd { display: flex; align-items: center; gap: var(--space-2); font-size: var(--font-size-sm); }
  .error { display: grid; gap: var(--space-1); padding: var(--space-3); border: 1px solid var(--status-error-ink); border-radius: var(--radius-md); background: var(--status-error-bg); color: var(--status-error-ink); }
  .actions { display: flex; flex-wrap: wrap; gap: var(--space-2); }
  .muted { color: var(--text-muted); font-size: var(--font-size-sm); }
</style>
