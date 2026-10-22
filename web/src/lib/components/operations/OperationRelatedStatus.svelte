<script lang="ts">
  import {
    getVisualAttachmentStatus as generatedGetVisualAttachmentStatus,
  } from '../../api/generated/api/api';
  import { Button, Chip, StatusDot } from '@kenn-io/kit-ui';
  import { onMount } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type {
    Status as GeneratedVisualStatus,
  } from '../../api/generated/models';
  import type { OperationStatusAuthority } from '../../explore/models';

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
    onConfigure?: () => void;
  } = $props();

  let loading = $state(true);
  let failed = $state(false);
  let visualStatus = $state<VisualStatus>();

  const labels: Record<OperationStatusAuthority, string> = {
    getVisualAttachmentStatus: 'Visual attachment status'
  };

  const configurationLabels: Record<OperationStatusAuthority, string> = {
    getVisualAttachmentStatus: 'Visual attachments need configuration'
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
    visualStatus = undefined;
    try {
      const result = await generatedGetVisualAttachmentStatus(client);
      if (!result.data) throw new Error('visual status unavailable');
      visualStatus = result.data;
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
    <h1>{labels[authority]}</h1>
    <Button size="sm" surface="soft" label="Back to operations" onclick={onClose} />
  </header>

  {#if loading}
    <p role="status">Loading {labels[authority].toLowerCase()}…</p>
  {:else if configured === false}
    <div class="summary" aria-label={`${labels[authority]} configuration`}>
      <p><Chip size="sm" tone="muted" uppercase={false}>Off</Chip> {configurationLabels[authority]}</p>
    </div>
    <Button label="Open visual attachment settings" onclick={() => onConfigure()} />
  {:else if failed}
    <div class="notice notice--error" role="alert">
      <span>Unable to load {labels[authority].toLowerCase()}.</span>
      <Button size="sm" label={`Retry ${labels[authority].toLowerCase()}`} onclick={() => void load()} />
    </div>
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
  .related-status { display: grid; align-content: start; gap: var(--space-4); min-height: 0; padding: var(--space-5) var(--page-gutter) var(--space-4); overflow: auto; }
  header { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); }
  h1, .summary p { margin: 0; }
  h1 { color: var(--text-primary); font-size: var(--font-size-xl); font-weight: 650; line-height: 1.25; }
  .summary { display: grid; gap: var(--space-3); padding: var(--space-4); border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-surface); }
  .summary p { display: flex; align-items: center; gap: var(--space-2); color: var(--text-muted); }
  .notice { padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); }
  .notice--error { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); border-color: var(--status-error-ink); background: var(--status-error-bg); color: var(--status-error-ink); }
  @media (max-width: 760px) { header { align-items: stretch; flex-direction: column; } }
</style>
