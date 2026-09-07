<script lang="ts">
  import {
    getCurrentDocumentIndexStatus as generatedGetCurrentDocumentIndexStatus,
    getDocumentVectorStatus as generatedGetDocumentVectorStatus,
    getVisualAttachmentStatus as generatedGetVisualAttachmentStatus,
  } from '../../api/generated/api/api';
  import { Button, StatusDot } from '@kenn-io/kit-ui';
  import { onMount } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type {
    DocumentIndexStatusResponse as GeneratedDocumentIndexStatusResponse,
    DocumentVectorOperationsResponse as GeneratedDocumentVectorOperationsResponse,
    Status as GeneratedVisualStatus,
  } from '../../api/generated/models';
  import type { OperationStatusAuthority } from '../../explore/models';

  type DocumentStatus = GeneratedDocumentIndexStatusResponse;
  type DocumentVectorStatus = GeneratedDocumentVectorOperationsResponse;
  type VisualStatus = GeneratedVisualStatus;

  let {
    client,
    authority,
    configured = undefined,
    onClose = () => undefined,
    onConfigure = () => undefined
  }: {
    client: APIClient;
    authority: OperationStatusAuthority;
    configured?: boolean;
    onClose?: () => void;
    onConfigure?: (authority: OperationStatusAuthority) => void;
  } = $props();

  let loading = $state(true);
  let failed = $state(false);
  let documentStatus = $state<DocumentStatus>();
  let documentVectorStatus = $state<DocumentVectorStatus>();
  let visualStatus = $state<VisualStatus>();

  const labels: Record<OperationStatusAuthority, string> = {
    getDocumentIndexStatus: 'Document index status',
    getDocumentVectorStatus: 'Document vector status',
    getVisualAttachmentStatus: 'Visual attachment status'
  };

  const configurationLabels: Record<OperationStatusAuthority, string> = {
    getDocumentIndexStatus: 'Document index needs configuration',
    getDocumentVectorStatus: 'Document vectors need configuration',
    getVisualAttachmentStatus: 'Visual attachments need configuration'
  };

  const settingsLabels: Record<OperationStatusAuthority, string> = {
    getDocumentIndexStatus: 'Open document index settings',
    getDocumentVectorStatus: 'Open document vector settings',
    getVisualAttachmentStatus: 'Open visual attachment settings'
  };

  onMount(() => {
    if (configured === false) {
      loading = false;
      return;
    }
    void load();
  });

  async function load(): Promise<void> {
    loading = true;
    failed = false;
    documentStatus = undefined;
    documentVectorStatus = undefined;
    visualStatus = undefined;
    try {
      if (authority === 'getDocumentIndexStatus') {
        const result = await generatedGetCurrentDocumentIndexStatus(client);
        if (!result.data) throw new Error('document status unavailable');
        documentStatus = result.data;
      } else if (authority === 'getDocumentVectorStatus') {
        const result = await generatedGetDocumentVectorStatus(undefined, client);
        if (!result.data) throw new Error('document vector status unavailable');
        documentVectorStatus = result.data;
      } else {
        const result = await generatedGetVisualAttachmentStatus(client);
        if (!result.data) throw new Error('visual status unavailable');
        visualStatus = result.data;
      }
    } catch {
      failed = true;
    } finally {
      loading = false;
    }
  }

  function count(value: number): string {
    return value.toLocaleString();
  }
</script>

<section class="related-status" aria-label={labels[authority]}>
  <header>
    <div><p>Live authority</p><h1>{labels[authority]}</h1></div>
    <Button size="sm" surface="soft" label="Back to operations" onclick={onClose} />
  </header>

  {#if loading}
    <p role="status">Loading {labels[authority].toLowerCase()}…</p>
  {:else if configured === false}
    <div class="summary" aria-label={`${labels[authority]} configuration`}>
      <p><span aria-hidden="true"><StatusDot status="unclean" /></span>{configurationLabels[authority]}</p>
    </div>
    <Button label={settingsLabels[authority]} onclick={() => onConfigure(authority)} />
  {:else if failed}
    <div class="notice notice--error" role="alert">
      <span>Unable to load {labels[authority].toLowerCase()}.</span>
      <Button size="sm" label={`Retry ${labels[authority].toLowerCase()}`} onclick={() => void load()} />
    </div>
  {:else if documentStatus}
    {@const status = documentStatus.status}
    <div class="summary" aria-label="Document index status summary">
      <p><span aria-hidden="true"><StatusDot status={status.profile_exists && status.profile_enabled ? 'idle' : 'unclean'} /></span>
        {status.profile_exists && status.profile_enabled ? 'Extraction profile enabled' : 'Extraction profile needs configuration'}</p>
      <p><span aria-hidden="true"><StatusDot status={status.exact_consent ? 'idle' : 'unclean'} /></span>
        {status.exact_consent ? 'Exact provider consent recorded' : 'Provider consent needs attention'}</p>
      <strong>{count(status.ready_owners)} of {count(status.eligible_owners)} owners ready</strong>
      <p>{count(status.missing_owners)} missing · {count(status.retry_owners)} retryable · {count(status.terminal_owners)} terminal</p>
      <p>{count(status.stored_plaintext_chunks)} stored plaintext chunks · {count(status.provider_requests)} provider requests</p>
      {#if documentStatus.active_rebuild}
        <p>{count(documentStatus.active_rebuild.remaining_owners)} of {count(documentStatus.active_rebuild.snapshot_owners)} rebuild owners remaining</p>
      {/if}
    </div>
    {#if !status.profile_exists || !status.profile_enabled || !status.exact_consent}
      <Button label="Open document index settings" onclick={() => onConfigure(authority)} />
    {/if}
  {:else if documentVectorStatus}
    <div class="summary" aria-label="Document vector status summary">
      <p><span aria-hidden="true"><StatusDot status={documentVectorStatus.enabled && documentVectorStatus.configured ? 'idle' : 'unclean'} /></span>
        {documentVectorStatus.enabled && documentVectorStatus.configured ? 'Document vectors configured' : 'Document vectors need configuration'}</p>
      {#if documentVectorStatus.status?.coverage}
        <strong>{count(documentVectorStatus.status.coverage.ready)} of {count(documentVectorStatus.status.coverage.required)} chunks ready</strong>
      {:else}<strong>Coverage is not available.</strong>{/if}
      {#if documentVectorStatus.status?.selected}
        {@const selected = documentVectorStatus.status.selected}
        <p>{count(selected.pending)} pending · {count(selected.retryable)} retryable · {count(selected.terminal)} terminal · {count(selected.cleanup_pending)} cleanup pending</p>
      {/if}
      {#if documentVectorStatus.scheduled_registration_requires_restart}
        <p class="notice">Scheduled query registration requires a daemon restart.</p>
      {/if}
    </div>
    {#if !documentVectorStatus.enabled || !documentVectorStatus.configured}
      <Button label="Open document vector settings" onclick={() => onConfigure(authority)} />
    {/if}
  {:else if visualStatus}
    <div class="summary" aria-label="Visual attachment status summary">
      <strong>{count(visualStatus.current)} of {count(visualStatus.eligible)} attachments current</strong>
      <p>{count(visualStatus.retryable)} retryable · {count(visualStatus.terminal)} terminal · {count(visualStatus.unavailable)} unavailable</p>
      <p>{count(visualStatus.active_leases)} active leases · {count(visualStatus.journal_lag)} journal lag</p>
      <p>{visualStatus.reconciliation_complete ? 'Reconciliation complete' : 'Reconciliation in progress'}</p>
    </div>
  {/if}
</section>

<style>
  .related-status { display: grid; align-content: start; gap: var(--space-4); min-height: 0; padding: var(--space-5) var(--space-6); overflow: auto; }
  header { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); }
  header p, h1, .summary p { margin: 0; }
  header p { color: var(--status-warning-ink); font-size: var(--font-size-2xs); font-weight: 800; letter-spacing: .1em; text-transform: uppercase; }
  .summary { display: grid; gap: var(--space-3); padding: var(--space-4); border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-surface); }
  .summary p { display: flex; align-items: center; gap: var(--space-2); color: var(--text-muted); }
  .notice { padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); }
  .notice--error { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); border-color: var(--status-error-ink); background: var(--status-error-bg); color: var(--status-error-ink); }
  @media (max-width: 760px) { .related-status { padding: var(--space-3); } header { align-items: stretch; flex-direction: column; } }
</style>
