<script lang="ts">
  import { Button, KbdBadge, Modal, appShortcuts } from '@kenn-io/kit-ui';
  import { onDestroy, onMount } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { components } from '../../api/generated/schema';

  type ExploreSelection = components['schemas']['ExploreSelection'];
  type Preflight = components['schemas']['ExplorePreflightResponse'];
  type ManifestSummary = components['schemas']['DeletionManifestSummary'];
  type ManifestDetail = components['schemas']['DeletionManifestDetail'];

  let {
    client, selection = undefined, reviewOnMount = false, onReviewStarted = () => undefined
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
  let preview = $state('');
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
      appShortcuts.register('d', () => {
        if (selection?.mode === 'explicit') void reviewSelection(true);
      }, { scope: 'deletions', description: 'Stage selected messages for deletion' }),
      appShortcuts.register('shift+d', () => {
        if (selection?.mode === 'all_matching') void reviewSelection(true);
      }, { scope: 'deletions', description: 'Stage all matching messages for deletion' })
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
      preview = '';
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
      const { data, error: responseError } = await client.GET('/api/v1/deletions', { signal: controller.signal });
      if (!data) throw new Error(messageFor(responseError, 'Unable to load deletion manifests.'));
      manifests = data.manifests ?? [];
      error = '';
    } catch (cause) {
      if (!controller.signal.aborted) error = cause instanceof Error ? cause.message : 'Unable to load deletion manifests.';
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
    preview = '';
    const candidate = selection;
    const candidateFingerprint = fingerprint(candidate);
    try {
      const { data, error: responseError } = await client.POST('/api/v1/explore/preflight', {
        body: { selection: candidate }
      });
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
    try {
      const { data, error: responseError } = await client.POST('/api/v1/deletions', {
        body: {
          selection,
          operation_token: reviewed!.operation_token,
          dry_run: true
        }
      });
      if (!data) throw new Error(messageFor(responseError, 'Unable to run the deletion preview.'));
      preview = `Dry run: ${data.message_count.toLocaleString()} ${data.message_count === 1 ? 'item' : 'items'} in ${data.account ?? 'the reviewed source'}`;
    } catch (cause) {
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
      const { data, error: responseError, response } = await client.POST('/api/v1/deletions', {
        body: {
          selection: stagedSelection,
          operation_token: reviewed!.operation_token,
          description: stagedSelection.mode === 'all_matching' ? 'reviewed matching selection' : 'reviewed explicit selection',
          dry_run: false
        }
      });
      if (!data || response.status !== 201) throw new Error(messageFor(responseError, 'Unable to stage this deletion.'));
      reviewed = undefined;
      preview = '';
      confirmStage = undefined;
      await loadManifests();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to stage this deletion.';
    } finally {
      pending = false;
    }
  }

  async function inspect(manifest: ManifestSummary): Promise<void> {
    pending = true;
    error = '';
    try {
      const { data, error: responseError } = await client.GET('/api/v1/deletions/{id}', {
        params: { path: { id: manifest.id } }
      });
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
      const { data, error: responseError } = await client.DELETE('/api/v1/deletions/{id}', {
        params: { path: { id: target.id } }
      });
      if (!data) throw new Error(messageFor(responseError, 'Unable to cancel this deletion manifest.'));
      manifests = manifests.map((manifest) => manifest.id === data.id ? { ...manifest, status: data.status } : manifest);
      if (detail?.id === data.id) detail = { ...detail, status: data.status };
      confirmCancel = undefined;
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to cancel this deletion manifest.';
    } finally {
      pending = false;
    }
  }

  function selectionDescription(): string {
    if (!selection || !reviewed) return '';
    if (selection.mode === 'all_matching') {
      const excluded = selection.exclusions?.length ?? 0;
      return `${reviewed.count.toLocaleString()} matching items minus ${excluded.toLocaleString()} ${excluded === 1 ? 'exclusion' : 'exclusions'}`;
    }
    return `${reviewed.count.toLocaleString()} selected ${reviewed.count === 1 ? 'item' : 'items'}`;
  }

  function messageFor(value: unknown, fallback: string): string {
    return typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string'
      ? value.message : fallback;
  }
</script>

<main class="deletions" aria-label="Deletions">
  <header><div><p>Archive workspace</p><h1>Deletions</h1></div><span>Staged manifest lifecycle</span></header>
  {#if error}<p class="notice notice--error" role="alert">{error}</p>{/if}

  <section class="staging" aria-labelledby="deletion-staging-title">
    <div><h2 id="deletion-staging-title">Reviewed staging</h2><p>Preflight the current session selection before creating a manifest.</p></div>
    <div class="actions">
      <Button tone="info" surface="soft" label="Review selection" disabled={!selection || pending} onclick={() => void reviewSelection()} />
      <span><KbdBadge keys={['d']} /> selected</span><span><KbdBadge keys={['D']} /> matching</span>
    </div>
    {#if reviewed}
      <div class="review" role="status">
        <strong>{reviewed.count.toLocaleString()} {reviewed.count === 1 ? 'item' : 'items'} · {reviewed.estimated_bytes.toLocaleString()} bytes</strong>
        <span>Authority expires {reviewed.expires_at}</span>
        {#each reviewed.unavailable_actions as unavailable (`${unavailable.action}:${unavailable.reason}`)}
          <span class="reason">{unavailable.action}: {unavailable.reason}</span>
        {/each}
      </div>
      <div class="actions">
        <Button surface="soft" label="Dry run" disabled={pending} onclick={() => void dryRun()} />
        <Button tone="danger" surface="solid" label="Stage deletion" disabled={pending || Boolean(unavailableReason('stage_deletion'))} onclick={() => { if (reviewedIsCurrent()) confirmStage = selection?.mode; }} />
      </div>
    {/if}
    {#if preview}<p class="preview" role="status">{preview}</p>{/if}
  </section>

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
              <Button size="sm" tone="danger" surface="soft" label={`Cancel ${manifest.id}`} onclick={() => { confirmCancel = manifest; }} />
            {/if}
          </div>
        </article>
      {/each}
    </section>
  {/if}

  {#if detail}
    <aside aria-label={`Deletion manifest ${detail.id}`}>
      <h2>{detail.id}</h2><strong>{detail.status}</strong>
      <span>{detail.account || 'Account unavailable'}</span>
      <span>{detail.message_count.toLocaleString()} items · {detail.description}</span>
      {#if detail.execution}
        <span>{detail.execution.succeeded} succeeded · {detail.execution.failed} failed</span>
        {#each detail.execution.failed_ids ?? [] as id}<code>{id}</code>{/each}
      {/if}
    </aside>
  {/if}
</main>

{#if confirmStage && reviewed}
  <Modal title={confirmStage === 'all_matching' ? 'Confirm matching deletion' : 'Confirm selected deletion'} tone="danger" onclose={() => { confirmStage = undefined; }}>
    <p>{selectionDescription()}. This creates a staged manifest; it does not execute deletion.</p>
    {#snippet footer()}
      <Button surface="soft" label="Cancel" onclick={() => { confirmStage = undefined; }} />
      <Button tone="danger" surface="solid" label="Confirm stage deletion" disabled={pending} onclick={() => void stage()} />
    {/snippet}
  </Modal>
{/if}

{#if confirmCancel}
  <Modal title="Cancel deletion manifest?" tone="warning" onclose={() => { confirmCancel = undefined; }}>
    <p>Cancel {confirmCancel.id}? Completed and failed manifests cannot be cancelled.</p>
    {#snippet footer()}
      <Button surface="soft" label="Keep manifest" onclick={() => { confirmCancel = undefined; }} />
      <Button tone="danger" surface="solid" label="Confirm cancel manifest" disabled={pending} onclick={() => void cancelManifest()} />
    {/snippet}
  </Modal>
{/if}

<style>
  .deletions { display: flex; min-height: 0; flex: 1; flex-direction: column; gap: var(--space-4); padding: var(--space-5) var(--space-6); }
  header, article, .actions, .staging { display: flex; align-items: center; gap: var(--space-3); }
  header { justify-content: space-between; }
  header p, h1, h2, .staging p { margin: 0; }
  header p { color: var(--accent-amber); font-size: var(--font-size-2xs); font-weight: 800; letter-spacing: .1em; text-transform: uppercase; }
  header span, article span, .staging p, .actions span, aside span { color: var(--text-muted); font-size: var(--font-size-xs); }
  .staging { flex-wrap: wrap; justify-content: space-between; padding: var(--space-3); border: 1px solid var(--border-muted); border-radius: var(--radius-md); background: var(--bg-surface); }
  .review { display: grid; gap: var(--space-1); }
  .reason, .notice--error { color: var(--text-danger); }
  .manifest-list { display: grid; border-top: 1px solid var(--border-muted); }
  article { justify-content: space-between; padding: var(--space-3); border-bottom: 1px solid var(--border-muted); }
  article > div:first-child, aside { display: grid; gap: var(--space-1); }
  .notice, .preview { padding: var(--space-3); border-left: 3px solid var(--accent-amber); background: var(--bg-subtle); }
  .notice--error { border-color: var(--accent-red); }
  aside { padding: var(--space-3); border: 1px solid var(--border-muted); border-radius: var(--radius-md); background: var(--bg-surface); }
  @media (max-width: 760px) { article, .staging { align-items: stretch; flex-direction: column; } }
</style>
