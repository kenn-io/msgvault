<script lang="ts" module>
  import type { ChipTone } from '@kenn-io/kit-ui';
  import { sentenceCase } from '../../explore/labels';

  const BATCH_STATUS: Readonly<Record<string, { label: string; tone: ChipTone }>> = {
    pending: { label: 'Staged', tone: 'info' },
    in_progress: { label: 'Deleting', tone: 'info' },
    completed: { label: 'Deleted', tone: 'success' },
    failed: { label: 'Failed', tone: 'danger' },
    cancelled: { label: 'Cancelled', tone: 'muted' },
  };

  function batchStatus(status: string): { label: string; tone: ChipTone } {
    return Object.hasOwn(BATCH_STATUS, status)
      ? BATCH_STATUS[status]!
      : { label: sentenceCase(status), tone: 'neutral' };
  }
</script>

<script lang="ts">
  import {
    cancelDeletion as generatedCancelDeletion,
    getDeletion as generatedGetDeletion,
    listDeletions as generatedListDeletions,
  } from '../../api/generated/api/api';
  import { Button, Checkbox, Chip, CopyButton, Table, TableHeaderCell } from '@kenn-io/kit-ui';
  import { onDestroy, onMount } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type {
    DeletionManifestDetail as GeneratedDeletionManifestDetail,
    DeletionManifestSummary as GeneratedDeletionManifestSummary,
  } from '../../api/generated/models';
  import { formatBytes, formatDateTime } from '../../util/format';
  import { EmptyState } from '@kenn-io/kit-ui';
  import SidePanel from '../common/SidePanel.svelte';
  import Dialog from '../common/Dialog.svelte';
  import PageHeader from '../shell/PageHeader.svelte';
  import CommandLine from '../sources/CommandLine.svelte';

  type Batch = GeneratedDeletionManifestSummary;
  type Detail = GeneratedDeletionManifestDetail;
  const DELETE_COMMAND = 'msgvault delete-staged';
  // Account lookups share a small worker pool across the batch history.
  const ACCOUNT_LOOKUPS = 4;

  let { client, onChanged = () => undefined }: { client: APIClient; onChanged?: () => void } = $props();

  let batches = $state<Batch[]>([]);
  let accounts = $state<Record<string, string>>({});
  const resolvedAccounts = new Set<string>();
  let selectedID = $state<string>();
  let detail = $state<Detail>();
  let showCancelled = $state(false);
  let loading = $state(true);
  let error = $state('');
  let confirmCancel = $state<Batch>();
  let cancelling = $state(false);
  let controller: AbortController | undefined;
  let detailController: AbortController | undefined;

  const cancelledCount = $derived(batches.filter((batch) => batch.status === 'cancelled').length);
  const rows = $derived(
    batches.filter(
      (batch) => showCancelled || batch.status !== 'cancelled',
    ),
  );

  onMount(() => void load());
  onDestroy(() => { controller?.abort(); detailController?.abort(); });

  async function load(): Promise<void> {
    controller?.abort();
    detailController?.abort();
    loading = true;
    const current = (controller = new AbortController());
    try {
      const { data, error: responseError } = await generatedListDeletions(undefined, {
        ...client,
        signal: current.signal,
      });
      if (!data) throw new Error(messageFor(responseError, "Can't load staged deletions."));
      if (current.signal.aborted) return;
      selectedID = detail = undefined;
      batches = data.manifests ?? [];
      error = '';
    } catch (cause) {
      if (!current.signal.aborted) error = cause instanceof Error ? cause.message : "Can't load staged deletions.";
    } finally {
      if (controller === current) loading = false;
    }
  }

  $effect(() => {
    if (loading) return;
    const list = rows;
    const current = new AbortController();
    void loadAccounts(list, current.signal);
    return () => current.abort();
  });

  async function loadAccounts(list: Batch[], signal: AbortSignal): Promise<void> {
    list = list.filter(batch => !resolvedAccounts.has(batch.id));
    let index = 0;
    await Promise.all(Array.from({ length: Math.min(ACCOUNT_LOOKUPS, list.length) }, async () => {
      while (index < list.length && !signal.aborted) {
        const batch = list[index++];
        try {
          const { data } = await generatedGetDeletion({ id: batch.id }, { ...client, signal });
          if (data && !signal.aborted) {
            resolvedAccounts.add(batch.id);
            accounts = { ...accounts, [batch.id]: data.account ?? (data.summary?.accounts ?? []).join(', ') };
          }
        } catch { /* A row still opens when its account lookup fails. */ }
      }
    }));
  }

  async function open(batch: Batch): Promise<void> {
    detailController?.abort();
    if (selectedID === batch.id) {
      selectedID = detail = undefined;
      return;
    }
    selectedID = batch.id;
    detail = undefined;
    const signal = (detailController = new AbortController()).signal;
    try {
      const { data, error: responseError } = await generatedGetDeletion({ id: batch.id }, { ...client, signal });
      if (!data) throw new Error(messageFor(responseError, "Can't open this batch."));
      if (!signal.aborted && selectedID === batch.id) detail = data;
    } catch (cause) {
      if (selectedID !== batch.id || signal.aborted) return;
      error = cause instanceof Error ? cause.message : "Can't open this batch.";
      selectedID = undefined;
    }
  }

  function close(): void {
    detailController?.abort();
    selectedID = detail = undefined;
  }

  async function cancelManifest(): Promise<void> {
    if (!confirmCancel || cancelling) return;
    const batch = confirmCancel;
    cancelling = true;
    error = '';
    try {
      const { data, error: responseError } = await generatedCancelDeletion({ id: batch.id }, client);
      if (!data) throw new Error(messageFor(responseError, "Can't cancel the batch."));
      batches = batches.map((item) => item.id === data.id ? { ...item, status: data.status } : item);
      if (detail?.id === data.id) detail = { ...detail, status: data.status };
      confirmCancel = undefined;
      onChanged();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : "Can't cancel the batch.";
    } finally { cancelling = false; }
  }

  function messageFor(value: unknown, fallback: string): string {
    return typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string'
      ? value.message
      : fallback;
  }
  function count(value: number): string {
    return `${value.toLocaleString()} ${value === 1 ? 'message' : 'messages'}`;
  }
  const cancellable = (status: string): boolean => status === 'pending' || status === 'in_progress';
</script>

<main class="deletions" aria-label="Staged deletions">
  <PageHeader title="Staged deletions">
    {#snippet actions()}
      {#if cancelledCount > 0}
        <Checkbox bind:checked={showCancelled} label={`Show cancelled (${cancelledCount})`} />
      {/if}
    {/snippet}
  </PageHeader>
  {#if error}<div class="notice" role="alert"><span>{error}</span><Button label="Retry" onclick={() => void load()} /></div>{/if}

  {#if loading}
    <p class="muted" role="status">Loading…</p>
  {:else if rows.length === 0}
    <EmptyState title="Nothing staged" />
  {:else}
    <Table ariaLabel="Staged deletions" zebra={false} class="batch-table" unsorted>
      {#snippet header()}
        <TableHeaderCell label="Date" />
        <TableHeaderCell label="Messages" />
        <TableHeaderCell label="Account" />
        <TableHeaderCell label="Status" />
      {/snippet}
      {#each rows as batch (batch.id)}
        {@const chip = batchStatus(batch.status)}
        <tr class:selected={selectedID === batch.id} aria-selected={selectedID === batch.id} onclick={() => void open(batch)}>
          <td>
            <button
              type="button"
              class="row-button"
              aria-label={`Open batch from ${formatDateTime(batch.created_at)}`}
              onclick={(event) => {
                event.stopPropagation();
                void open(batch);
              }}>{formatDateTime(batch.created_at)}</button
            >
          </td>
          <td>{count(batch.message_count)}</td>
          <td>{accounts[batch.id] ?? ''}</td>
          <td><Chip size="sm" tone={chip.tone} dot uppercase={false}>{chip.label}</Chip></td>
        </tr>
      {/each}
    </Table>
  {/if}

  <footer class="end">
    <CommandLine command={DELETE_COMMAND} />
  </footer>
</main>

{#snippet cancelAction()}
  <Button
    tone="danger"
    surface="soft"
    label="Cancel batch"
    onclick={() => confirmCancel = batches.find((batch) => batch.id === selectedID)}
  />
{/snippet}

{#if selectedID && detail}
  {@const chip = batchStatus(detail.status)}
  <SidePanel
    title={`Staged ${formatDateTime(detail.created_at)}`}
    onclose={close}
    footer={cancellable(detail.status) ? cancelAction : undefined}
  >
    <div class="panel">
      <div class="line">
        <Chip size="sm" tone={chip.tone} dot uppercase={false}>{chip.label}</Chip>
        <span class="muted">{detail.account || (detail.summary?.accounts ?? []).join(', ')}</span>
      </div>
      {#if detail.description}<p>{detail.description}</p>{/if}
      <section class="block" aria-label="Messages">
        <h3>{count(detail.message_count)}</h3>
        {#if detail.summary}
          <span class="muted">
            {formatBytes(detail.summary.total_size_bytes)}{#if detail.summary.date_range?.[0]} · {detail.summary.date_range[0].slice(0, 10)} to {detail.summary.date_range[1]?.slice(0, 10)}{/if}
          </span>
          {#if detail.summary.top_senders?.length}
            <ul class="senders">
              {#each detail.summary.top_senders as sender (sender.sender)}
                <li><span>{sender.sender}</span><span class="muted">{sender.count.toLocaleString()}</span></li>
              {/each}
            </ul>
          {/if}
        {/if}
      </section>
      {#if detail.execution}
        <span class="muted">
          {detail.execution.succeeded.toLocaleString()} deleted · {detail.execution.failed.toLocaleString()} failed
        </span>
        {#if detail.execution.failed_ids?.length}
          <section class="block" aria-label="Failed message IDs">
            <h3>Failed message IDs</h3>
            {#each detail.execution.failed_ids as id}<code>{id}</code>{/each}
          </section>
        {/if}
      {/if}
      <div class="id">
        <span class="muted">Batch ID</span>
        <span class="id__value">
          <code>{detail.id}</code>
          <CopyButton text={detail.id} ariaLabel="Copy batch ID" />
        </span>
      </div>
    </div>
  </SidePanel>
{/if}

{#if confirmCancel}
  <Dialog title="Cancel deletion batch?" onclose={() => { if (!cancelling) confirmCancel = undefined; }} primaryLabel={cancelling ? 'Cancelling…' : 'Cancel batch'} primaryDisabled={cancelling} onprimary={() => void cancelManifest()}>
    <p>Cancel deletion of {count(confirmCancel.message_count)} from the source?</p>
    <Button label="Keep staged" disabled={cancelling} onclick={() => confirmCancel = undefined} />
    {#if error}<p role="alert">{error}</p>{/if}
  </Dialog>
{/if}

<style>
  .deletions > :global(.page-header) {
    margin-bottom: calc(var(--space-5) - var(--section-gap));
  }

  .deletions {
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    gap: var(--section-gap);
    padding: var(--space-5) var(--page-gutter) var(--space-4);
  }
  .muted {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  .notice {
    margin: 0;
    padding: var(--pane-pad-y) var(--pane-pad-x);
    border: 1px solid var(--accent-red);
    border-radius: var(--radius-md);
    background: var(--bg-subtle);
    color: var(--text-danger);
  }
  :global(.kit-table-wrapper.batch-table) {
    flex: none;
    overflow: auto;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
  }
  :global(.batch-table .kit-table) {
    background: var(--bg-surface);
  }
  :global(.batch-table tbody td),
  :global(.batch-table thead th) {
    padding-inline: var(--pane-pad-x);
    vertical-align: middle;
  }
  :global(.batch-table tbody tr) {
    cursor: pointer;
  }
  tr.selected {
    background: var(--bg-surface-hover);
  }
  .row-button {
    padding: 0;
    border: 0;
    background: none;
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-medium);
    cursor: pointer;
    text-align: left;
  }
  .row-button:focus-visible {
    outline: var(--focus-ring);
    outline-offset: 2px;
  }
  .end {
    margin-top: auto;
    padding-top: var(--section-gap);
  }
  .panel {
    display: grid;
    gap: var(--section-gap);
    padding: var(--pane-pad-y) var(--pane-pad-x);
  }
  .line {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--control-gap);
  }
  .block {
    display: grid;
    gap: var(--field-gap);
  }
  h3 {
    margin: 0;
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-semibold);
  }
  .senders {
    display: grid;
    gap: var(--label-gap);
    margin: 0;
    padding: 0;
    list-style: none;
    font-size: var(--font-size-sm);
  }
  .senders li {
    display: flex;
    justify-content: space-between;
    gap: var(--control-gap);
  }
  .id {
    display: grid;
    gap: var(--label-gap);
  }
  .id__value {
    display: inline-flex;
    align-items: center;
    gap: var(--control-gap);
    min-width: 0;
  }
  code {
    min-width: 0;
    overflow-wrap: anywhere;
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
  }
</style>
