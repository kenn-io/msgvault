<script lang="ts" module>
  import type { ChipTone } from '@kenn-io/kit-ui';
  import { sentenceCase } from '../../explore/labels';

  const MANIFEST_STATUS: Readonly<Record<string, { label: string; tone: ChipTone }>> = {
    pending: { label: 'Pending', tone: 'info' },
    in_progress: { label: 'In progress', tone: 'info' },
    completed: { label: 'Completed', tone: 'success' },
    failed: { label: 'Failed', tone: 'danger' },
    cancelled: { label: 'Cancelled', tone: 'muted' },
  };

  function manifestStatusChip(status: string): { label: string; tone: ChipTone } {
    return Object.hasOwn(MANIFEST_STATUS, status)
      ? MANIFEST_STATUS[status]!
      : { label: sentenceCase(status), tone: 'neutral' };
  }
</script>

<script lang="ts">
  import {
    cancelDeletion as generatedCancelDeletion,
    getDeletion as generatedGetDeletion,
    listDeletions as generatedListDeletions,
    stageDeletion as generatedStageDeletion,
  } from '../../api/generated/api/api';
  import { preflightExploreSelection as generatedPreflightExploreSelection } from '../../api/generated/exploration/exploration';
  import {
    Button,
    Card,
    Chip,
    EmptyState,
    Modal,
    Table,
    TableHeaderCell,
    appShortcuts,
  } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick } from 'svelte';
  import type { APIClient } from '../../api/client';
  import { preflightReasonLabel } from '../../explore/labels';
  import { formatBytes, formatDateTime, formatRelativeTime } from '../../util/format';
  import PageHeader from '../shell/PageHeader.svelte';
  import type {
    DeletionManifestDetail as GeneratedDeletionManifestDetail,
    DeletionManifestSummary as GeneratedDeletionManifestSummary,
    ExplorePreflightResponse as GeneratedExplorePreflightResponse,
    ExploreSelection as GeneratedExploreSelection,
    StageDeletionResponse as GeneratedStageDeletionResponse,
  } from '../../api/generated/models';
  type ExploreSelection = GeneratedExploreSelection;
  type Preflight = GeneratedExplorePreflightResponse;
  type ManifestSummary = GeneratedDeletionManifestSummary;
  type ManifestDetail = GeneratedDeletionManifestDetail;
  type StageDeletionResponse = GeneratedStageDeletionResponse;
  let {
    client,
    selection = undefined,
    reviewOnMount = false,
    onReviewStarted = () => undefined,
    now = () => new Date(),
  }: {
    client: APIClient;
    selection?: ExploreSelection;
    reviewOnMount?: boolean;
    onReviewStarted?: () => void;
    now?: () => Date;
  } = $props();
  let manifests = $state<ManifestSummary[]>([]);
  let detail = $state<ManifestDetail>();
  let manifestsElement = $state<HTMLElement>();
  let reviewed = $state<Preflight>();
  let reviewedFingerprint = '';
  let loading = $state(true);
  let pending = $state(false);
  let error = $state('');
  let preview = $state<StageDeletionResponse>();
  let confirmStage = $state<'explicit' | 'all_matching'>();
  let confirmCancel = $state<ManifestSummary>();
  let listController: AbortController | undefined;
  onMount(() => {
    void loadManifests();
    if (reviewOnMount && selection) {
      onReviewStarted();
      void reviewSelection(true);
    }
    const releaseScope = appShortcuts.pushScope('deletions');
    const unregister = [
      appShortcuts.register(
        'd',
        () => {
          if (selection?.mode === 'explicit') void reviewSelection(true);
        },
        { scope: 'deletions', description: 'Stage selected messages for deletion' },
      ),
      appShortcuts.register(
        'shift+d',
        () => {
          if (selection?.mode === 'all_matching') void reviewSelection(true);
        },
        { scope: 'deletions', description: 'Stage all matching messages for deletion' },
      ),
    ];
    return () => {
      for (const remove of unregister.reverse()) remove();
      releaseScope();
    };
  });
  onDestroy(() => listController?.abort());
  function fingerprint(value: ExploreSelection | undefined): string {
    return value ? JSON.stringify(value) : '';
  }
  function reviewedIsCurrent(): boolean {
    if (!reviewed || reviewedFingerprint !== fingerprint(selection)) {
      reviewed = undefined;
      preview = undefined;
      error = 'The selection changed. Review it again before continuing.';
      return false;
    }
    return true;
  }
  async function loadManifests(): Promise<void> {
    listController?.abort();
    const controller = new AbortController();
    listController = controller;
    try {
      const { data, error: responseError } = await generatedListDeletions(undefined, {
        ...client,
        signal: controller.signal,
      });
      if (!data) throw new Error(messageFor(responseError, 'Unable to load deletion manifests.'));
      manifests = data.manifests ?? [];
      error = '';
    } catch (cause) {
      if (!controller.signal.aborted)
        error = cause instanceof Error ? cause.message : 'Unable to load deletion manifests.';
    } finally {
      if (listController === controller) {
        listController = undefined;
        loading = false;
      }
    }
  }
  async function reviewSelection(openConfirmation = false): Promise<void> {
    if (!selection || pending) return;
    pending = true;
    error = '';
    preview = undefined;
    const candidate = selection;
    const candidateFingerprint = fingerprint(candidate);
    try {
      const { data, error: responseError } = await generatedPreflightExploreSelection({ selection: candidate }, client);
      if (!data) throw new Error(messageFor(responseError, 'Unable to review this selection.'));
      if (typeof data.deletable_count !== 'number') {
        throw new Error('Deletion review requires daemon API schema 2.18.0 or newer. Upgrade the daemon and review again.');
      }
      if (candidateFingerprint !== fingerprint(selection)) {
        throw new Error('The selection changed while it was being reviewed. Review it again.');
      }
      reviewed = data;
      reviewedFingerprint = candidateFingerprint;
      if (openConfirmation && !unavailableReason('stage_deletion')) confirmStage = candidate.mode;
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to review this selection.';
    } finally {
      pending = false;
    }
  }
  function unavailableReason(action: string): string | undefined {
    return reviewed?.unavailable_actions.find((item) => item.action === action)?.reason;
  }
  async function dryRun(): Promise<void> {
    if (!selection || !reviewedIsCurrent()) return;
    pending = true;
    error = '';
    const dryRunSelection = selection;
    const dryRunFingerprint = fingerprint(dryRunSelection);
    try {
      const { data, error: responseError } = await generatedStageDeletion(
        {
          selection: dryRunSelection,
          operation_token: reviewed!.operation_token,
          dry_run: true,
        },
        client,
      );
      if (!data) throw new Error(messageFor(responseError, 'Unable to run the deletion preview.'));
      if (dryRunFingerprint !== fingerprint(selection)) {
        throw new Error('The selection changed while it was being reviewed. Review it again.');
      }
      preview = data;
    } catch (cause) {
      preview = undefined;
      error = cause instanceof Error ? cause.message : 'Unable to run the deletion preview.';
    } finally {
      pending = false;
    }
  }
  async function stage(): Promise<void> {
    if (!selection || !reviewedIsCurrent()) return;
    pending = true;
    error = '';
    const stagedSelection = selection;
    try {
      const {
        data,
        error: responseError,
        response,
      } = await generatedStageDeletion(
        {
          selection: stagedSelection,
          operation_token: reviewed!.operation_token,
          description:
            stagedSelection.mode === 'all_matching' ? 'reviewed matching selection' : 'reviewed explicit selection',
          dry_run: false,
        },
        client,
      );
      if (!data || response.status !== 201)
        throw new Error(messageFor(responseError, 'Unable to stage this deletion.'));
      reviewed = undefined;
      preview = data;
      confirmStage = undefined;
      await loadManifests();
    } catch (cause) {
      preview = undefined;
      error = cause instanceof Error ? cause.message : 'Unable to stage this deletion.';
    } finally {
      pending = false;
    }
  }
  async function closeDetail(): Promise<void> {
    const id = detail?.id;
    detail = undefined;
    await tick();
    const rows = manifestsElement?.querySelectorAll<HTMLTableRowElement>('tr[data-manifest-id]') ?? [];
    const row = Array.from(rows).find((candidate) => candidate.dataset['manifestId'] === id);
    row?.querySelector<HTMLButtonElement>('.inspect-action')?.focus();
  }
  async function inspect(manifest: ManifestSummary): Promise<void> {
    pending = true;
    error = '';
    try {
      const { data, error: responseError } = await generatedGetDeletion({ id: manifest.id }, client);
      if (!data) throw new Error(messageFor(responseError, 'Unable to inspect this deletion manifest.'));
      detail = data;
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to inspect this deletion manifest.';
    } finally {
      pending = false;
    }
  }
  async function cancelManifest(): Promise<void> {
    if (!confirmCancel) return;
    const target = confirmCancel;
    pending = true;
    error = '';
    try {
      const { data, error: responseError } = await generatedCancelDeletion({ id: target.id }, client);
      if (!data) throw new Error(messageFor(responseError, 'Unable to cancel this deletion manifest.'));
      manifests = manifests.map((manifest) =>
        manifest.id === data.id ? { ...manifest, status: data.status } : manifest,
      );
      if (detail?.id === data.id) detail = { ...detail, status: data.status };
      confirmCancel = undefined;
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to cancel this deletion manifest.';
    } finally {
      pending = false;
    }
  }
  function stageCounts(value: StageDeletionResponse): { matched: number; staged: number; skipped: number } {
    const staged = value.message_count;
    const skipped = value.skipped_count ?? Math.max((value.matched_count ?? staged) - staged, 0);
    const matched = value.matched_count ?? staged + skipped;
    return { matched, staged, skipped };
  }
  function resultSummary(value: StageDeletionResponse): string {
    const { matched, staged, skipped } = stageCounts(value);
    const prefix = value.dry_run ? 'Dry run' : 'Staged';
    const account = value.account ? ` in ${value.account}` : '';
    const batch = !value.dry_run && value.id ? ` · Batch ID: ${value.id}` : '';
    return `${prefix}: Matched: ${matched.toLocaleString()} · Staged: ${staged.toLocaleString()} · Skipped: ${skipped.toLocaleString()}${account}${batch}`;
  }
  function partialWarning(value: StageDeletionResponse): string {
    const { staged, skipped } = stageCounts(value);
    if (value.dry_run) {
      return `Partial staging: only the deletable Gmail and Microsoft Graph mail subset (${staged.toLocaleString()}) will be staged; ${skipped.toLocaleString()} unsupported ${skipped === 1 ? 'match will be' : 'matches will be'} skipped.`;
    }
    return `Partial staging: only the deletable Gmail and Microsoft Graph mail subset (${staged.toLocaleString()}) was staged; ${skipped.toLocaleString()} unsupported ${skipped === 1 ? 'match was' : 'matches were'} skipped.`;
  }
  function selectionExclusions(): string {
    const count = selection?.exclusions?.length ?? 0;
    return selection?.mode === 'all_matching' && count > 0
      ? ` After ${count.toLocaleString()} ${count === 1 ? 'exclusion' : 'exclusions'}.`
      : '';
  }
  function confirmationDescription(): string {
    const counts = preview?.dry_run
      ? resultSummary(preview)
      : `Matched: ${reviewed!.count.toLocaleString()} · Will stage: ${reviewed!.deletable_count.toLocaleString()} · Will skip: ${(reviewed!.count - reviewed!.deletable_count).toLocaleString()}`;
    return `${counts}.${selectionExclusions()} Only deletable Gmail and Microsoft Graph mail messages will be staged. This creates a staged manifest; it does not execute deletion.`;
  }
  function messageFor(value: unknown, fallback: string): string {
    return typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string'
      ? value.message
      : fallback;
  }
</script>

<main class="deletions" aria-label="Deletions">
  <PageHeader title="Deletions">
    {#snippet descriptionContent()}Deletions you've staged. Nothing is deleted until you run <code>msgvault delete-staged</code>.{/snippet}
  </PageHeader>
  {#if error}<p class="notice notice--error" role="alert">{error}</p>{/if}

  <section class="selection-review" aria-label="Deletion selection">
    {#if !selection}
      <EmptyState
        title="Nothing selected for deletion"
        description="Select items in Everything, then choose Review for deletion…"
      />
    {:else}
      <Card padding="sm">
        <section class="staging" aria-labelledby="deletion-review-title">
        <div class="staging-header">
          <h2 id="deletion-review-title">Review selection</h2>
          <Button
            tone="info"
            surface="soft"
            label="Review selection"
            disabled={pending}
            onclick={() => void reviewSelection()}
          />
        </div>
        {#if reviewed}
          {@const stageReason = unavailableReason('stage_deletion')}
          <div class="review" role="status">
            <strong
              >{reviewed.count.toLocaleString()}
              {reviewed.count === 1 ? 'item' : 'items'} · {formatBytes(reviewed.estimated_bytes)}</strong
            >
            <span>
              {reviewed.deletable_count.toLocaleString()} can be staged · {(reviewed.count - reviewed.deletable_count).toLocaleString()} will be skipped.{selectionExclusions()}
            </span>
            <span
              >Review expires <time datetime={reviewed.expires_at} title={formatDateTime(reviewed.expires_at, 'long')}
                >{formatRelativeTime(reviewed.expires_at, now())}</time
              ></span
            >
            {#if reviewed.search_deletion_scope === 'active'}
              <span>Semantic search covers active messages only.</span>
            {/if}
            {#if stageReason}
              <span class="reason" title={stageReason}>{preflightReasonLabel('stage_deletion', stageReason)}</span>
            {/if}
          </div>
          <div class="actions">
            <Button surface="outline" label="Dry run" disabled={pending} onclick={() => void dryRun()} />
            <Button
              tone="info"
              surface="solid"
              label="Stage deletion…"
              disabled={pending || Boolean(stageReason)}
              onclick={() => {
                if (reviewedIsCurrent()) confirmStage = selection?.mode;
              }}
            />
          </div>
        {/if}
        {#if preview}
          <p class="result" role="status">{resultSummary(preview)}</p>
          {#if stageCounts(preview).skipped > 0}<p class="warning" role="alert">{partialWarning(preview)}</p>{/if}
        {/if}
        </section>
      </Card>
    {/if}
  </section>

  {#if loading}<p role="status">Loading deletion manifests…</p>
  {:else if manifests.length === 0}
    <EmptyState
      title="No staged deletions"
      description="Deletions you stage appear here, along with their execution status."
    />
  {:else}
    <div class="manifests" class:has-detail={Boolean(detail)} bind:this={manifestsElement}>
      <Table ariaLabel="Deletion manifests" zebra={false}>
        {#snippet header()}
          <TableHeaderCell label="ID" />
          <TableHeaderCell label="Description" />
          <TableHeaderCell label="Items" />
          <TableHeaderCell label="Status" />
          <TableHeaderCell label="Created" />
          <TableHeaderCell label="Actions" />
        {/snippet}
        {#each manifests as manifest (manifest.id)}
          {@const chip = manifestStatusChip(manifest.status)}
          <tr data-manifest-id={manifest.id}>
            <td><code>{manifest.id}</code></td>
            <td>{manifest.description}</td>
            <td>{manifest.message_count.toLocaleString()} {manifest.message_count === 1 ? 'item' : 'items'}</td>
            <td><Chip size="sm" tone={chip.tone} uppercase={false}>{chip.label}</Chip></td>
            <td><time datetime={manifest.created_at} title={formatDateTime(manifest.created_at, 'long')}>{formatDateTime(manifest.created_at)}</time></td>
            <td class="row-actions">
              <Button
                size="sm"
                surface="outline"
                class="inspect-action"
                label={`Inspect ${manifest.id}`}
                onclick={() => void inspect(manifest)}
              />
              {#if manifest.status === 'pending' || manifest.status === 'in_progress'}
                <Button
                  size="sm"
                  surface="outline"
                  label={`Cancel ${manifest.id}`}
                  onclick={() => {
                    confirmCancel = manifest;
                  }}
                />
              {/if}
            </td>
          </tr>
        {/each}
      </Table>
      {#if detail}
        {@const detailChip = manifestStatusChip(detail.status)}
        <Card padding="sm" ariaLabel={`Deletion manifest ${detail.id}`}>
          <aside class="detail">
            <div class="detail-header">
              <h2><code>{detail.id}</code></h2>
              <Button
                size="sm"
                surface="soft"
                label="Close manifest detail"
                onclick={() => void closeDetail()}
              />
            </div>
            <Chip size="sm" tone={detailChip.tone} uppercase={false}>{detailChip.label}</Chip>
            <span>{detail.account || 'Account unavailable'}</span>
            <span>{detail.message_count.toLocaleString()} items · {detail.description}</span>
            {#if detail.execution}
              <span>{detail.execution.succeeded} succeeded · {detail.execution.failed} failed</span>
              {#each detail.execution.failed_ids ?? [] as id}<code>{id}</code>{/each}
            {/if}
          </aside>
        </Card>
      {/if}
    </div>
  {/if}
</main>

{#if confirmStage && reviewed}
  <Modal
    title={confirmStage === 'all_matching' ? 'Confirm matching deletion' : 'Confirm selected deletion'}
    tone="danger"
    onclose={() => {
      confirmStage = undefined;
    }}
  >
    <p>{confirmationDescription()}</p>
    {#snippet footer()}
      <Button
        surface="soft"
        label="Cancel"
        onclick={() => {
          confirmStage = undefined;
        }}
      />
      <Button
        tone="danger"
        surface="solid"
        label="Confirm stage deletion"
        disabled={pending}
        onclick={() => void stage()}
      />
    {/snippet}
  </Modal>
{/if}

{#if confirmCancel}
  <Modal
    title="Cancel deletion manifest?"
    tone="warning"
    onclose={() => {
      confirmCancel = undefined;
    }}
  >
    <p>Cancel {confirmCancel.id}? Completed and failed manifests cannot be cancelled.</p>
    {#snippet footer()}
      <Button
        surface="soft"
        label="Keep manifest"
        onclick={() => {
          confirmCancel = undefined;
        }}
      />
      <Button
        tone="danger"
        surface="solid"
        label="Confirm cancel manifest"
        disabled={pending}
        onclick={() => void cancelManifest()}
      />
    {/snippet}
  </Modal>
{/if}

<style>
  .deletions {
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    gap: var(--space-4);
    padding: var(--space-5) var(--page-gutter) var(--space-4);
  }
  .actions,
  .staging-header,
  .detail-header,
  .row-actions {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }
  .selection-review {
    flex: none;
    overflow: hidden;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
  }
  .selection-review :global(.kit-empty-state) {
    margin: 0;
    padding: var(--space-6);
  }
  .selection-review :global(.kit-card) {
    border: 0;
    border-radius: 0;
    box-shadow: none;
  }
  .staging {
    display: grid;
    gap: var(--space-3);
  }
  .staging-header,
  .detail-header {
    justify-content: space-between;
  }
  h2,
  .staging p {
    margin: 0;
  }
  .detail span {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .review {
    display: grid;
    gap: var(--space-1);
  }
  .reason {
    color: var(--text-secondary);
  }
  .notice--error {
    color: var(--text-danger);
  }
  .manifests.has-detail {
    display: grid;
    grid-template-columns: minmax(0, 2fr) minmax(16rem, 1fr);
    gap: var(--space-4);
    align-items: start;
  }
  .detail {
    display: grid;
    gap: var(--space-2);
  }
  .notice,
  .result,
  .warning {
    padding: var(--space-3);
    border: 1px solid var(--accent-amber);
    background: var(--bg-subtle);
  }
  .notice,
  .warning {
    border-radius: var(--radius-md);
  }
  .result {
    border-color: var(--border-default);
  }
  .notice--error {
    border-color: var(--accent-red);
  }
  @media (max-width: 900px) {
    .manifests.has-detail {
      grid-template-columns: 1fr;
    }
  }
  @media (max-width: 760px) {
    .staging-header {
      align-items: stretch;
      flex-direction: column;
    }
  }
</style>
