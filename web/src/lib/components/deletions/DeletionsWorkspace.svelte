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
  import { Button, Checkbox, Chip, CopyButton, Table, TableHeaderCell } from '@kenn-io/kit-ui';
  import { onMount } from 'svelte';
  import type { DeletionsController } from '../../deletions/controller.svelte';
  import type {
    DeletionManifestSummary as GeneratedDeletionManifestSummary,
    DeletionManifestDetail,
  } from '../../api/generated/models';
  import { formatBytes, formatDateTime } from '../../util/format';
  import { EmptyState } from '@kenn-io/kit-ui';
  import SidePanel from '../common/SidePanel.svelte';
  import Dialog from '../common/Dialog.svelte';
  import PageHeader from '../shell/PageHeader.svelte';
  import CommandLine from '../sources/CommandLine.svelte';

  type Batch = GeneratedDeletionManifestSummary;
  const DELETE_COMMAND = 'msgvault delete-staged';
  let { controller, onChanged = () => undefined }: { controller: DeletionsController; onChanged?: () => void } = $props();
  const batches = $derived(controller.batches);
  const loading = $derived(controller.loading);
  let selectedID = $state<string>();
  let panelDetail = $state<DeletionManifestDetail>();
  let showCancelled = $state(false);
  let error = $state('');
  let confirmCancel = $state<Batch>();
  let cancelling = $state(false);
  let detailGeneration = 0;

  const cancelledCount = $derived(batches.filter((batch) => batch.status === 'cancelled').length);
  const rows = $derived(
    batches.filter(
      (batch) => showCancelled || batch.status !== 'cancelled',
    ),
  );

  onMount(() => void load());
  async function load(): Promise<void> { await controller.refresh(true); }
  $effect(() => {
    if (!loading) void Promise.all(rows.map(batch => controller.getDetail(batch.id)));
  });
  const selectedBatch = $derived(batches.find(batch => batch.id === selectedID));
  const shownDetail = $derived(selectedID ? controller.details[selectedID] ?? panelDetail : undefined);
  async function open(batch: Batch): Promise<void> {
    const generation = ++detailGeneration;
    if (selectedID === batch.id) { selectedID = undefined; return; }
    selectedID = batch.id;
    panelDetail = undefined;
    const data = await controller.getDetail(batch.id, { retry: true, fresh: true });
    if (generation !== detailGeneration) return;
    error = data ? '' : "Can't open this batch."; panelDetail = data; selectedID = data ? batch.id : undefined;
  }
  function close(): void { detailGeneration++; selectedID = undefined; }

  async function cancelManifest(): Promise<void> {
    if (!confirmCancel || cancelling) return;
    const batch = confirmCancel;
    cancelling = true;
    error = '';
    try {
      await controller.cancel(batch.id);
      confirmCancel = undefined;
      onChanged();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : "Can't cancel the batch.";
    } finally { cancelling = false; }
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
  {#if error || controller.error}<div class="notice" role="alert"><span>{error || controller.error}</span><Button label="Retry" onclick={() => { error = ''; void load(); }} /></div>{/if}

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
          <td>{controller.details[batch.id]?.account ?? (controller.details[batch.id]?.summary?.accounts ?? []).join(', ')}</td>
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

{#if selectedID && shownDetail && selectedBatch}
  {@const chip = batchStatus(selectedBatch.status)}
  <SidePanel
    title={`Staged ${formatDateTime(shownDetail.created_at)}`}
    onclose={close}
    footer={cancellable(selectedBatch.status) ? cancelAction : undefined}
  >
    <div class="panel">
      <div class="line">
        <Chip size="sm" tone={chip.tone} dot uppercase={false}>{chip.label}</Chip>
        <span class="muted">{shownDetail.account || (shownDetail.summary?.accounts ?? []).join(', ')}</span>
      </div>
      {#if shownDetail.description}<p>{shownDetail.description}</p>{/if}
      <section class="block" aria-label="Messages">
        <h3>{count(shownDetail.message_count)}</h3>
        {#if shownDetail.summary}
          <span class="muted">
            {formatBytes(shownDetail.summary.total_size_bytes)}{#if shownDetail.summary.date_range?.[0]} · {shownDetail.summary.date_range[0].slice(0, 10)} to {shownDetail.summary.date_range[1]?.slice(0, 10)}{/if}
          </span>
          {#if shownDetail.summary.top_senders?.length}
            <ul class="senders">
              {#each shownDetail.summary.top_senders as sender (sender.sender)}
                <li><span>{sender.sender}</span><span class="muted">{sender.count.toLocaleString()}</span></li>
              {/each}
            </ul>
          {/if}
        {/if}
      </section>
      {#if shownDetail.execution}
        <span class="muted">
          {shownDetail.execution.succeeded.toLocaleString()} deleted · {shownDetail.execution.failed.toLocaleString()} failed
        </span>
        {#if shownDetail.execution.failed_ids?.length}
          <section class="block" aria-label="Failed message IDs">
            <h3>Failed message IDs</h3>
            {#each shownDetail.execution.failed_ids as id}<code>{id}</code>{/each}
          </section>
        {/if}
      {/if}
      <div class="id">
        <span class="muted">Batch ID</span>
        <span class="id__value">
          <code>{shownDetail.id}</code>
          <CopyButton text={shownDetail.id} ariaLabel="Copy batch ID" />
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
    --page-header-parent-gap: var(--section-gap);
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
    min-height: 0;
    flex: 0 1 auto;
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
