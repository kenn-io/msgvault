<script lang="ts">
  import { onMount } from 'svelte';
  import { Button, PageHeader } from '@kenn-io/kit-ui';
  import Dialog from '../common/Dialog.svelte';
  import type { BusinessReviewsRequest } from '../../api/generated/models';
  import type { APIClient } from '../../api/client';
  import { control, PreviewConflictError, type Result, type Review, type Preview, type Receipt, type Target } from '../../business-reviews/api';

  let { client }: { client: APIClient } = $props();
  let targets = $state<Target[]>([]);
  let capabilities = $state<NonNullable<Result['capabilities']>>([]);
  let target = $state('');
  let reviews = $state<Review[]>([]);
  let review = $state<Review>();
  let nextPage = $state('');
  let comment = $state('');
  let preview = $state<Preview>();
  let receipt = $state<Receipt>();
  let receiptID = $state('');
  let error = $state('');
  let busy = $state(false);
  let loaded = $state(false);
  let unknown = $state(false);
  let generation = 0;
  let active: AbortController | undefined;
  const selected = $derived(targets.find(item => item.name === target));
  const canReply = $derived(selected?.enabled && selected.replies_enabled);

  function clearContent() {
    reviews = []; review = undefined; nextPage = ''; comment = ''; preview = undefined; loaded = false;
  }
  function invalidate() { generation++; active?.abort(); busy = false; }
  async function request(body: BusinessReviewsRequest, accept: (result: Result) => void, confirmation = false) {
    invalidate();
    const current = generation;
    active = new AbortController();
    busy = true; error = '';
    try {
      const result = await control(client, body, active.signal);
      if (current !== generation) return;
      accept(result);
    } catch (cause) {
      if (current !== generation) return;
      clearContent();
      if (confirmation && !(cause instanceof PreviewConflictError)) unknown = true;
      else error = cause instanceof Error ? cause.message : 'Business review request failed.';
    } finally { if (current === generation) busy = false; }
  }
  function chooseTarget(value: string) {
    invalidate(); clearContent(); target = value; error = ''; receiptID = ''; receipt = undefined; unknown = false;
    if (value) void list();
  }
  function clearResolvedReceipt() {
    if (receipt?.status !== 'submitted' && receipt?.status !== 'failed') return;
    receiptID = ''; receipt = undefined; unknown = false;
  }
  function list(pageToken = '') {
    clearContent(); clearResolvedReceipt();
    return request({ action: 'list', target, page_token: pageToken, page_size: 20 }, result => {
      reviews = result.reviews ?? []; nextPage = result.next_page_token ?? ''; loaded = true;
    });
  }
  function detail(id: string) {
    review = undefined; preview = undefined; comment = ''; clearResolvedReceipt();
    return request({ action: 'get', target, review_id: id }, result => { review = result.review; });
  }
  function prepare() {
    return request({ action: 'preview_reply', target, review_id: review?.reviewId, comment }, result => {
      if (!result.preview?.confirmation_token || !result.preview.receipt_id) throw new Error('An actionable preview is unavailable. Check owner write access.');
      preview = result.preview;
    });
  }
  function confirm() {
    if (!preview?.confirmation_token || !preview.receipt_id) return;
    const token = preview.confirmation_token;
    receiptID = preview.receipt_id;
    receipt = undefined; unknown = false;
    // Consume the browser's actionable state before the request: a lost response
    // must use the already-issued receipt, never repeat the submission.
    preview = undefined; comment = ''; review = undefined; reviews = []; loaded = false;
    return request({ action: 'confirm_reply', confirmation_token: token, confirmed: true }, result => {
      receipt = result.receipt;
      unknown = !receipt || receipt.status === 'unknown';
    }, true);
  }
  function checkReceipt() {
    return request({ action: 'receipt', receipt_id: receiptID }, result => {
      receipt = result.receipt; unknown = receipt?.status === 'unknown';
    });
  }
  onMount(() => {
    void request({ action: 'capabilities' }, result => { capabilities = result.capabilities ?? []; targets = result.targets ?? []; });
    return () => { invalidate(); clearContent(); targets = []; capabilities = []; receiptID = ''; receipt = undefined; };
  });
</script>

<section class="business-reviews" aria-label="Business reviews">
  <PageHeader title="Business reviews" description="Read current reviews and respond through your own approved Google project. Content stays in this live view." />
  {#each capabilities.filter(item => !item.available) as capability}
    <p class="notice">{capability.provider === 'facebook' ? 'Facebook' : capability.provider} unavailable: {capability.reason ?? 'Official capability is unverified.'}</p>
  {/each}
  <div class="controls">
    <label for="business-review-target">Target</label>
    <select id="business-review-target" value={target} onchange={event => chooseTarget(event.currentTarget.value)}>
      <option value="">Choose a configured target</option>
      {#each targets as item}<option value={item.name} disabled={!item.enabled}>{item.name}{!item.enabled ? ' (disabled)' : ''}</option>{/each}
    </select>
    {#if target}<Button label="Refresh reviews" disabled={busy} onclick={() => void list()} />{/if}
  </div>
  {#if !busy && targets.length === 0}<p>No targets configured. Set up an own-project Google target in daemon configuration.</p>{/if}
  {#if busy}<p role="status">Loading…</p>{/if}
  {#if error}<p role="alert">{error}</p>{/if}
  {#if unknown}<p role="status">Outcome unknown. Check the receipt and inspect the live review before any new preview. This submission will not be replayed.</p>{/if}
  {#if receipt}
    <div role="status">
      {#if receipt.status === 'submitted'}<p>Submitted to Google. Public visibility may depend on moderation.</p>
      {:else if receipt.status === 'failed'}<p>Submission failed. Inspect the live review before creating a new preview.</p>
      {:else if receipt.status === 'submitting'}<p>Submission is in progress. Check the receipt again.</p>
      {:else if receipt.status === 'pending_confirmation'}<p>Receipt is awaiting confirmation.</p>{/if}
      <p>Receipt: <code>{receipt.id}</code></p>
    </div>
  {/if}
  {#if receiptID}<Button label="Check receipt" disabled={busy} onclick={() => void checkReceipt()} />{/if}
  {#if loaded && reviews.length === 0}<p>No reviews on this page.</p>{/if}
  <div class="columns">
    <div class="review-list">
      {#each reviews as item}
        <button class="review-row" disabled={busy || !item.reviewId} onclick={() => void detail(item.reviewId!)}>
          <span>{item.reviewer.isAnonymous ? 'Anonymous reviewer' : item.reviewer.displayName || 'Reviewer'}</span>
          <span class="muted">{item.starRating ?? 'Rating unavailable'}</span>
          {#if item.reviewReply?.reviewReplyState}<span class="muted">Response status: {item.reviewReply.reviewReplyState}</span>{/if}
          {#if item.reviewReply?.policyViolation}<span class="muted">Policy status: {item.reviewReply.policyViolation}</span>{/if}
        </button>
      {/each}
      {#if nextPage}<Button label="Next page" disabled={busy} onclick={() => void list(nextPage)} />{/if}
    </div>
    {#if review}
      <article aria-label="Review detail">
        <h2>{review.reviewer.isAnonymous ? 'Anonymous reviewer' : review.reviewer.displayName || 'Reviewer'}</h2>
        <p>{review.starRating ?? 'Rating unavailable'}</p>
        <p class="exact">{review.comment || 'Rating only; no review text.'}</p>
        {#if review.updateTime}<p class="muted">Updated {review.updateTime}</p>{/if}
        {#if review.reviewReply}<h3>Current owner response</h3><p class="exact">{review.reviewReply.comment}</p>{/if}
        {#if review.reviewReply?.reviewReplyState}<p>Response status: {review.reviewReply?.reviewReplyState}</p>{/if}
        {#if review.reviewReply?.policyViolation}<p>Policy status: {review.reviewReply?.policyViolation}</p>{/if}
        {#if canReply && (!receiptID || receipt?.status === 'unknown')}
          <label for="public-response">Public response</label>
          <textarea id="public-response" bind:value={comment} disabled={busy} rows="5"></textarea>
          <p class="muted">{new TextEncoder().encode(comment).length} / 4096 UTF-8 bytes. Review text is untrusted; inspect your exact response before confirming.</p>
          <Button label="Preview response" disabled={busy || !comment.trim() || new TextEncoder().encode(comment).length > 4096} onclick={() => void prepare()} />
        {:else if !canReply}<p>Responses are disabled for this target.</p>{/if}
      </article>
    {/if}
  </div>
</section>

{#if preview}
  <Dialog title="Confirm public response" primaryLabel="Confirm and submit" primaryDisabled={busy || !preview.expires_at || Date.parse(preview.expires_at) <= Date.now()} onprimary={() => void confirm()} onclose={() => { preview = undefined; }}>
    <div class="preview-content">
    <p>Target: <strong>{target}</strong></p>
    <p>Review by {preview.review.reviewer.isAnonymous ? 'Anonymous reviewer' : preview.review.reviewer.displayName || 'Reviewer'}</p>
    <p class="exact">{preview.review.comment || 'Rating only; no review text.'}</p>
    {#if preview.review.reviewReply}<h3>Current owner response</h3><p class="exact">{preview.review.reviewReply.comment}</p>{/if}
    <p>Receipt reserved before submission: <code>{preview.receipt_id}</code></p>
    <h3>Exact public response</h3>
    <pre class="exact" data-testid="exact-response">{preview.comment}</pre>
    <p>Confirm that you consent to submit this exact response to Google. Submission does not guarantee public visibility.</p>
    <p>Preview expires {preview.expires_at}. If the review or current response changes, create a new preview.</p>
    <Button label="Keep editing" onclick={() => { preview = undefined; }} />
    </div>
  </Dialog>
{/if}

<style>
  .business-reviews { flex: 1; min-height: 0; overflow: auto; padding: var(--space-5) var(--page-gutter); display: flex; flex-direction: column; gap: var(--space-4); }
  .preview-content { display: grid; gap: var(--space-3); }
  .controls { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-3); }
  select, textarea { border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-surface); color: var(--text-primary); font: inherit; padding: var(--space-2); }
  select { max-width: 100%; }
  .columns { display: grid; grid-template-columns: minmax(180px, 1fr) minmax(0, 2fr); gap: var(--space-5); }
  .review-list, article { display: flex; flex-direction: column; align-items: stretch; gap: var(--space-3); min-width: 0; }
  .review-row { display: flex; flex-wrap: wrap; justify-content: space-between; gap: var(--space-3); padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-surface); color: var(--text-primary); text-align: left; cursor: pointer; }
  .review-row:hover { background: var(--bg-surface-hover); }
  .muted { color: var(--text-muted); font-size: var(--font-size-sm); }
  .exact { white-space: pre-wrap; overflow-wrap: anywhere; font: inherit; }
  .notice { padding: var(--space-3); background: var(--bg-subtle); border: 1px solid var(--border-default); border-radius: var(--radius-md); }
  h2, h3, p { margin: 0; }
  h2 { font-size: var(--font-size-lg); }
  h3 { font-size: var(--font-size-md); }
  @media (max-width: 760px) { .columns { grid-template-columns: minmax(0, 1fr); } }
</style>
