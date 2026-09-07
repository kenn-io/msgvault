<script lang="ts">
  import { Button, StatusDot } from '@kenn-io/kit-ui';

  import type {
    OperationAction,
    OperationKind,
    OperationLane,
    OperationRunSummary,
    OperationStatusLane
  } from '../../operations/models';

  type RelatedStatus = NonNullable<OperationStatusLane['kinds'][number]['related_status']>;

  let {
    lanes,
    actionPending = null,
    onNavigate = () => undefined,
    onAction = () => undefined
  }: {
    lanes: readonly OperationStatusLane[];
    actionPending?: OperationAction | null;
    onNavigate?: (target: RelatedStatus, button: HTMLButtonElement) => void;
    onAction?: (action: OperationAction) => void;
  } = $props();

  const laneLabels: Record<OperationLane, string> = {
    messages: 'Messages',
    person_facts: 'Facts',
    contacts: 'Contacts',
    documents: 'Documents',
    visual_attachments: 'Attachments'
  };
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

  function stateLabel(run: OperationRunSummary): string {
    return run.state.charAt(0).toUpperCase() + run.state.slice(1);
  }

  function statusDot(run: OperationRunSummary) {
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
</script>

<section class="lane-cards" aria-label="Operation lanes">
  {#each lanes as lane (lane.lane)}
    <article class="lane-card" aria-label={`${laneLabels[lane.lane]} operations`}>
      <h2>{laneLabels[lane.lane]}</h2>
      {#if lane.kinds.length === 0}
        <p class="availability"><span aria-hidden="true"><StatusDot status="quiet" /></span> Status unavailable</p>
      {:else}
        <div class="kind-list">
          {#each lane.kinds as kind (kind.kind)}
            <section class="kind" aria-label={kindLabels[kind.kind]}>
              <h3>{kindLabels[kind.kind]}</h3>
              <div class="availability">
                <span>
                  <span aria-hidden="true"><StatusDot status={kind.configured ? 'idle' : 'unclean'} /></span>
                  {kind.configured ? 'Configured' : 'Not configured'}
                </span>
                <span>
                  <span aria-hidden="true"><StatusDot status={kind.history_availability === 'available' ? 'idle' : 'stale'} /></span>
                  {kind.history_availability === 'available' ? 'History available' : 'History unavailable'}
                </span>
              </div>
              <dl class="run-facts">
                {#if kind.active}
                  <div>
                    <dt>Active</dt>
                    <dd>
                      <span aria-hidden="true"><StatusDot status={statusDot(kind.active)} /></span>
                      <span>{stateLabel(kind.active)}</span>
                      <time datetime={kind.active.started_at}>{formatTimestamp(kind.active.started_at)}</time>
                    </dd>
                  </div>
                {/if}
                {#if kind.latest}
                  <div>
                    <dt>Latest</dt>
                    <dd>
                      <span aria-hidden="true"><StatusDot status={statusDot(kind.latest)} /></span>
                      <span>{stateLabel(kind.latest)}</span>
                      <time datetime={kind.latest.started_at}>{formatTimestamp(kind.latest.started_at)}</time>
                    </dd>
                  </div>
                {/if}
                {#if kind.latest_successful}
                  <div>
                    <dt>Last successful</dt>
                    <dd>
                      <span aria-hidden="true"><StatusDot status="idle" /></span>
                      <span>Succeeded</span>
                      <time datetime={kind.latest_successful.started_at}>{formatTimestamp(kind.latest_successful.started_at)}</time>
                    </dd>
                  </div>
                {/if}
              </dl>
              {#if kind.history_availability === 'available' && !kind.active && !kind.latest && !kind.latest_successful}
                <p class="no-run">No recorded runs</p>
              {/if}
              {#if kind.related_status || kind.supported_actions.length > 0}
                <div class="actions">
                  {#if kind.related_status}
                    <Button
                      size="sm"
                      surface="soft"
                      label={`Open ${relatedLabels[kind.related_status]}`}
                      onclick={(event) => onNavigate(kind.related_status!, event.currentTarget as HTMLButtonElement)}
                    />
                  {/if}
                  {#each kind.supported_actions as action (action)}
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
          {/each}
        </div>
      {/if}
    </article>
  {/each}
</section>

<style>
  .lane-cards {
    display: grid;
    grid-template-columns: repeat(5, minmax(190px, 1fr));
    gap: var(--space-3);
    min-width: 0;
  }
  .lane-card {
    min-width: 0;
    padding: var(--space-3);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
  }
  h2, h3, p, dl, dd { margin: 0; }
  h2 { font-size: var(--font-size-sm); }
  h3 {
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    font-weight: 700;
  }
  .kind-list { display: grid; gap: var(--space-3); margin-top: var(--space-3); }
  .kind { display: grid; gap: var(--space-2); min-width: 0; }
  .kind + .kind { padding-top: var(--space-3); border-top: 1px solid var(--border-muted); }
  .availability { display: flex; flex-wrap: wrap; gap: var(--space-1) var(--space-3); color: var(--text-muted); font-size: var(--font-size-2xs); }
  .availability span, dd { display: flex; align-items: center; gap: var(--space-1); }
  .run-facts { display: grid; gap: var(--space-1); }
  .run-facts div { display: grid; grid-template-columns: minmax(72px, auto) 1fr; gap: var(--space-2); font-size: var(--font-size-2xs); }
  dt { color: var(--text-muted); }
  dd { min-width: 0; }
  dd time { overflow: hidden; color: var(--text-muted); text-overflow: ellipsis; white-space: nowrap; }
  .no-run { color: var(--text-muted); font-size: var(--font-size-2xs); }
  .actions { display: flex; flex-wrap: wrap; gap: var(--space-1); }

  @media (max-width: 900px) {
    .lane-cards { grid-template-columns: repeat(2, minmax(220px, 1fr)); }
  }
  @media (max-width: 760px) {
    .lane-cards { grid-template-columns: 1fr; }
  }
</style>
