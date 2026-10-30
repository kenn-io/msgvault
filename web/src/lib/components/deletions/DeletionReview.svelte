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
  import { formatBytes, formatDateTime } from '../../util/format';
  import Dialog from '../common/Dialog.svelte';

  let {
    client,
    selection,
    onclose,
    onStaged = () => undefined,
  }: {
    client: APIClient;
    selection: Selection;
    /** Closes review or its staged result. */
    onclose: () => void;
    /** Runs once the batch is staged, so the sidebar can show Staged deletions. */
    onStaged?: (batchID: string) => void;
  } = $props();

  let review = $state<Preflight>();
  let result = $state<StageDeletionResponse>();
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
    preview = undefined;
    review = undefined;
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
    preview = undefined;
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
    preview = undefined;
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
      result = data;
      onStaged(data.id);
    } catch (cause) {
      error = cause instanceof Error ? cause.message : "Can't stage these messages.";
    } finally {
      pending = false;
    }
  }

  function resultSummary(value: StageDeletionResponse): string {
    const staged = value.message_count;
    const skipped = value.skipped_count ?? Math.max((value.matched_count ?? staged) - staged, 0);
    const matched = value.matched_count ?? staged + skipped;
    return `${value.dry_run ? 'Dry run' : 'Staged'}: Matched: ${matched.toLocaleString()} · Staged: ${staged.toLocaleString()} · Skipped: ${skipped.toLocaleString()}${value.account ? ` in ${value.account}` : ''}`;
  }

  function messageFor(value: unknown, fallback: string): string {
    return typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string'
      ? value.message
      : fallback;
  }
  const noun = (value: number): string => (value === 1 ? 'message' : 'messages');
</script>

  <Dialog
    title="Review deletion"
    {onclose}
    primaryLabel={result ? 'Done' : `Stage ${deletable.toLocaleString()} ${noun(deletable)}`}
    primaryDisabled={!result && (pending || !review || deletable === 0 || Boolean(blocked))}
    onprimary={() => result ? onclose() : void stage()}
  >
    {#if result}
      <p role="status">{resultSummary(result)}</p>
      <p>Batch ID: <code>{result.id}</code></p>
      <p>Run <code>msgvault delete-staged</code> separately to delete these messages from the source.</p>
    {:else}
    <p>Stage eligible Gmail and Microsoft mail. The archive stays intact. Run <code>msgvault delete-staged</code> separately to execute deletion.</p>
    <Button label="Dry run" disabled={pending || !review || deletable === 0 || Boolean(blocked)} onclick={() => void dryRun()} />
    {#if pending && !review}
      <p class="muted" role="status">Checking…</p>
    {:else if review}
      <div class="counts" role="status">
        <strong>{review.count.toLocaleString()} {noun(review.count)} matched · {formatBytes(review.estimated_bytes)}</strong>
        <span>{deletable.toLocaleString()} eligible · {skipped.toLocaleString()} skipped</span>
        {#if selection.mode === 'all_matching' && selection.exclusions?.length}<span>After {selection.exclusions.length.toLocaleString()} {selection.exclusions.length === 1 ? 'exclusion' : 'exclusions'}.</span>{/if}
        <span>Review expires <time datetime={review.expires_at}>{formatDateTime(review.expires_at, 'long')}</time></span>
        {#if review.search_deletion_scope === 'active'}<span>Semantic search covers active messages only.</span>{/if}
        {#if blocked}<span class="muted">{preflightReasonLabel('stage_deletion', blocked)}</span>{/if}
        {#if preview}
          <span class="muted">
            {resultSummary(preview)}
          </span>
        {/if}
      </div>
    {/if}
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
