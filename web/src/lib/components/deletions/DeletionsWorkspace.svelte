<script lang="ts">
  import {
    cancelDeletion as generatedCancelDeletion,
    getDeletion as generatedGetDeletion,
    listDeletions as generatedListDeletions,
    stageDeletion as generatedStageDeletion,
  } from '../../api/generated/api/api';
  import { preflightExploreSelection as generatedPreflightExploreSelection } from '../../api/generated/exploration/exploration';
  import { Button, Card, KbdBadge, Modal, appShortcuts } from '@kenn-io/kit-ui';
  import { onDestroy, onMount } from 'svelte';
  import type { APIClient } from '../../api/client';
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
  }: {
    client: APIClient;
    selection?: ExploreSelection;
    reviewOnMount?: boolean;
    onReviewStarted?: () => void;
  } = $props();
  let manifests = $state<ManifestSummary[]>([]);
  let detail = $state<ManifestDetail>();
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
    const stagedFingerprint = fingerprint(stagedSelection);
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
      if (stagedFingerprint !== fingerprint(selection)) {
        throw new Error('The selection changed while it was being staged. Review it again.');
      }
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
    return `${prefix}: Matched: ${matched.toLocaleString()} · Staged: ${staged.toLocaleString()} · Skipped: ${skipped.toLocaleString()}${account}`;
  }
  function partialWarning(value: StageDeletionResponse): string {
    const { staged, skipped } = stageCounts(value);
    if (value.dry_run) {
      return `Partial staging: only the deletable Gmail subset (${staged.toLocaleString()}) will be staged; ${skipped.toLocaleString()} unsupported ${skipped === 1 ? 'match may be' : 'matches may be'} skipped.`;
    }
    return `Partial staging: only the deletable Gmail subset (${staged.toLocaleString()}) was staged; ${skipped.toLocaleString()} unsupported ${skipped === 1 ? 'match was' : 'matches were'} skipped.`;
  }
  function confirmationDescription(): string {
    if (preview?.dry_run) {
      const partial = stageCounts(preview).skipped > 0;
      return `${resultSummary(preview)} ${partial ? 'Only the deletable Gmail subset will be staged.' : 'These reviewed matches will be staged.'} This creates a staged manifest; it does not execute deletion.`;
    }
    return 'The server stages only deletable Gmail matches and may skip unsupported matches. This creates a staged manifest; it does not execute deletion.';
  }
  function messageFor(value: unknown, fallback: string): string {
    return typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string'
      ? value.message
      : fallback;
  }
</script>

<main class="deletions" aria-label="Deletions">
  <header>
    <div>
      <p>Archive workspace</p>
      <h1>Deletions</h1>
    </div>
    <span>Staged manifest lifecycle</span>
  </header>
  {#if error}<p class="notice notice--error" role="alert">{error}</p>{/if}

  <Card padding="sm">
    <section class="staging" aria-labelledby="deletion-staging-title">
      <div>
        <h2 id="deletion-staging-title">Reviewed staging</h2>
        <p>Preflight the current session selection before creating a manifest.</p>
      </div>
      <div class="actions">
        <Button
          tone="info"
          surface="soft"
          label="Review selection"
          disabled={!selection || pending}
          onclick={() => void reviewSelection()}
        />
        <span><KbdBadge keys={['d']} /> selected</span><span><KbdBadge keys={['D']} /> matching</span>
      </div>
      {#if reviewed}
        <div class="review" role="status">
          <strong
            >{reviewed.count.toLocaleString()}
            {reviewed.count === 1 ? 'item' : 'items'} · {reviewed.estimated_bytes.toLocaleString()} bytes</strong
          >
          <span>Authority expires {reviewed.expires_at}</span>
          {#if reviewed.search_deletion_scope === 'active'}
            <span>Semantic search covers active messages only.</span>
          {/if}
          {#each reviewed.unavailable_actions as unavailable (`${unavailable.action}:${unavailable.reason}`)}
            <span class="reason">{unavailable.action}: {unavailable.reason}</span>
          {/each}
        </div>
        <div class="actions">
          <Button surface="soft" label="Dry run" disabled={pending} onclick={() => void dryRun()} />
          <Button
            tone="danger"
            surface="solid"
            label="Stage deletion"
            disabled={pending || Boolean(unavailableReason('stage_deletion'))}
            onclick={() => {
              if (reviewedIsCurrent()) confirmStage = selection?.mode;
            }}
          />
        </div>
      {/if}
      {#if preview}
        <p class="preview" role="status">{resultSummary(preview)}</p>
        {#if stageCounts(preview).skipped > 0}<p class="notice" role="alert">{partialWarning(preview)}</p>{/if}
      {/if}
    </section>
  </Card>

  {#if loading}<p role="status">Loading deletion manifests…</p>
  {:else if manifests.length === 0}<p class="notice" role="status">No deletion manifests yet.</p>
  {:else}
    <section class="manifest-list" aria-label="Deletion manifests">
      {#each manifests as manifest (manifest.id)}
        <article>
          <div><strong>{manifest.id}</strong><span>{manifest.description}</span></div>
          <span>{manifest.message_count.toLocaleString()} {manifest.message_count === 1 ? 'item' : 'items'}</span>
          <span>{manifest.status}</span>
          <div class="actions">
            <Button size="sm" surface="soft" label={`Inspect ${manifest.id}`} onclick={() => void inspect(manifest)} />
            {#if manifest.status === 'pending' || manifest.status === 'in_progress'}
              <Button
                size="sm"
                tone="danger"
                surface="soft"
                label={`Cancel ${manifest.id}`}
                onclick={() => {
                  confirmCancel = manifest;
                }}
              />
            {/if}
          </div>
        </article>
      {/each}
    </section>
  {/if}

  {#if detail}
    <Card padding="sm" ariaLabel={`Deletion manifest ${detail.id}`}>
      <aside>
        <h2>{detail.id}</h2>
        <strong>{detail.status}</strong>
        <span>{detail.account || 'Account unavailable'}</span>
        <span>{detail.message_count.toLocaleString()} items · {detail.description}</span>
        {#if detail.execution}
          <span>{detail.execution.succeeded} succeeded · {detail.execution.failed} failed</span>
          {#each detail.execution.failed_ids ?? [] as id}<code>{id}</code>{/each}
        {/if}
      </aside>
    </Card>
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
    padding: var(--space-5) var(--space-6);
  }
  header,
  article,
  .actions,
  .staging {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }
  header {
    justify-content: space-between;
  }
  header p,
  h1,
  h2,
  .staging p {
    margin: 0;
  }
  header p {
    color: var(--status-warning-ink);
    font-size: var(--font-size-2xs);
    font-weight: 800;
    letter-spacing: 0.1em;
    text-transform: uppercase;
  }
  header span,
  article span,
  .staging p,
  .actions span,
  aside span {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .staging {
    flex-wrap: wrap;
    justify-content: space-between;
  }
  .review {
    display: grid;
    gap: var(--space-1);
  }
  .reason,
  .notice--error {
    color: var(--text-danger);
  }
  .manifest-list {
    display: grid;
    border-top: 1px solid var(--border-muted);
  }
  article {
    justify-content: space-between;
    padding: var(--space-3);
    border-bottom: 1px solid var(--border-muted);
  }
  article > div:first-child,
  aside {
    display: grid;
    gap: var(--space-1);
  }
  .notice,
  .preview {
    padding: var(--space-3);
    border: 1px solid var(--accent-amber);
    border-radius: var(--radius-md);
    background: var(--bg-subtle);
  }
  .notice--error {
    border-color: var(--accent-red);
  }
  @media (max-width: 760px) {
    article,
    .staging {
      align-items: stretch;
      flex-direction: column;
    }
  }
</style>
