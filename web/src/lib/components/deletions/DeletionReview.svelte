<script lang="ts">
  import {
    stageDeletion as generatedStageDeletion,
  } from '../../api/generated/api/api';
  import { preflightExploreSelection as generatedPreflightExploreSelection } from '../../api/generated/exploration/exploration';
  import { Button } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type {
    ExplorePreflightResponse as Preflight,
    ExploreSelection as Selection,
    StageDeletionResponse,
  } from '../../api/generated/models';
  import { preflightReasonLabel } from '../../explore/labels';
  import { formatBytes } from '../../util/format';
  import Dialog from '../common/Dialog.svelte';

  let {
    client,
    selection,
    onclose,
    onStaged = () => undefined,
  }: {
    client: APIClient;
    selection: Selection;
    /** Runs when the dialog is done, after a successful stage. */
    onclose: () => void;
    /** Runs once the batch is staged, so the sidebar can show Staged deletions. */
    onStaged?: (batchID: string) => void;
  } = $props();

  let review = $state<Preflight>();
  let preview = $state<StageDeletionResponse>();
  let pending = $state(true);
  let error = $state('');

  const deletable = $derived(review?.deletable_count ?? 0);
  const skipped = $derived((review?.count ?? 0) - deletable);
  const blocked = $derived(review?.unavailable_actions.find((item) => item.action === 'stage_deletion')?.reason);

  const controller = new AbortController();
  onDestroy(() => controller.abort());
  const requestClient = untrack(() => ({ ...client, signal: controller.signal }));
  onMount(() => void run());

  async function run(): Promise<void> {
    pending = true;
    error = '';
    try {
      const { data, error: responseError } = await generatedPreflightExploreSelection({ selection }, requestClient);
      if (!data) throw new Error(messageFor(responseError, "Can't check this selection."));
      if (controller.signal.aborted) return;
      review = data;
    } catch (cause) {
      error = cause instanceof Error ? cause.message : "Can't check this selection.";
    } finally {
      pending = false;
    }
  }

  async function dryRun(): Promise<void> {
    if (!review || pending) return;
    pending = true;
    error = '';
    try {
      const { data, error: responseError } = await generatedStageDeletion(
        { selection, operation_token: review.operation_token, dry_run: true },
        requestClient,
      );
      if (!data) throw new Error(messageFor(responseError, "Can't run the preview."));
      if (controller.signal.aborted) return;
      preview = data;
    } catch (cause) {
      error = cause instanceof Error ? cause.message : "Can't run the preview.";
    } finally {
      pending = false;
    }
  }

  // The reviewed operation token binds staging to this selection.
  async function stage(): Promise<void> {
    if (!review || pending) return;
    pending = true;
    error = '';
    try {
      const {
        data,
        error: responseError,
        response,
      } = await generatedStageDeletion(
        {
          selection,
          operation_token: review.operation_token,
          description: selection.mode === 'all_matching' ? 'reviewed matching selection' : 'reviewed explicit selection',
          dry_run: false,
        },
        requestClient,
      );
      if (!data || response.status !== 201 || !data.id) throw new Error(messageFor(responseError, "Can't stage these messages."));
      if (controller.signal.aborted) return;
      onStaged(data.id);
      onclose();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : "Can't stage these messages.";
    } finally {
      pending = false;
    }
  }

  function messageFor(value: unknown, fallback: string): string {
    return typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string'
      ? value.message
      : fallback;
  }
  const noun = (value: number): string => (value === 1 ? 'message' : 'messages');
</script>

  <Dialog
    title="Delete from Gmail"
    {onclose}
    primaryLabel={`Stage ${deletable.toLocaleString()} ${noun(deletable)}`}
    primaryDisabled={pending || !review || deletable === 0 || Boolean(blocked)}
    onprimary={() => void stage()}
  >
    <Button label="Dry run" disabled={pending || !review || deletable === 0 || Boolean(blocked)} onclick={() => void dryRun()} />
    {#if pending && !review}
      <p class="muted" role="status">Checking…</p>
    {:else if review}
      <div class="counts" role="status">
        <strong>{review.count.toLocaleString()} {noun(review.count)} · {formatBytes(review.estimated_bytes)}</strong>
        {#if skipped > 0}<span class="muted">{skipped.toLocaleString()} can't be deleted from Gmail and stay.</span>{/if}
        {#if blocked}<span class="muted">{preflightReasonLabel('stage_deletion', blocked)}</span>{/if}
        {#if preview}
          <span class="muted">
            Dry run: {preview.message_count.toLocaleString()} {noun(preview.message_count)} would be staged.
          </span>
        {/if}
      </div>
    {/if}
    {#if error}
      <p class="error" role="alert">
        {error}
        <Button size="sm" label="Try again" onclick={() => void run()} />
      </p>
    {/if}
  </Dialog>
<style>
  .counts {
    display: grid;
    gap: var(--field-gap);
  }
  .muted {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  .error {
    margin: var(--field-gap) 0 0;
    color: var(--text-danger);
    font-size: var(--font-size-sm);
  }
</style>
