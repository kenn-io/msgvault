<script lang="ts">
  import { onMount } from 'svelte';
  import { Button, PageHeader } from '@kenn-io/kit-ui';
  import Dialog from '../common/Dialog.svelte';
  import type { GoogleReviewsRequest } from '../../api/generated/models';
  import type { APIClient } from '../../api/client';
  import { control, searchArchive, PreviewConflictError, ReceiptUnavailableError, type ArchiveSearchResponse, type BackfillState, type Result, type Review, type Preview, type Receipt, type Target } from '../../google-reviews/api';

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
  let receiptUnavailable = $state(false);
  let receiptReviewID = $state('');
  let receiptTarget = $state('');
  let recoveryInspected = $state(false);
  let error = $state('');
  let busy = $state(false);
  let loaded = $state(false);
  let unknown = $state(false);
  let archiveQuery = $state('');
  let archiveResult = $state<ArchiveSearchResponse>();
  let archiveSync = $state<BackfillState>();
  let archivePage = $state(1);
  let archiveBusy = $state(false);
  let archiveError = $state('');
  let generation = 0;
  let active: AbortController | undefined;
  let archiveGeneration = 0;
  let archiveController: AbortController | undefined;
  const selected = $derived(targets.find(item => item.name === target));
  const canReply = $derived(selected?.enabled && selected.replies_enabled);
  const matchingListReview = $derived(review?.reviewId ? reviews.find(item => item.reviewId === review?.reviewId) : undefined);
  const archivePages = $derived(archiveResult ? Math.max(1, Math.ceil(archiveResult.total / archiveResult.page_size)) : 1);
  const archiveQueryBytes = $derived(new TextEncoder().encode(archiveQuery).length);

  function archiveStatus(status: string): string {
    switch (status) {
      case 'not_started': return 'Not started';
      case 'running': return 'Importing';
      case 'api_exhausted': return 'No more pages';
      default: return 'Interrupted';
    }
  }

  function clearContent() {
    reviews = []; review = undefined; nextPage = ''; comment = ''; preview = undefined; loaded = false;
  }
  function invalidate() { generation++; active?.abort(); busy = false; }
  function invalidateArchive() { archiveGeneration++; archiveController?.abort(); archiveBusy = false; }
  async function request(body: GoogleReviewsRequest, accept: (result: Result) => void, confirmation = false) {
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
      if (body.action === 'receipt' && cause instanceof ReceiptUnavailableError) {
        receiptUnavailable = true;
        unknown = true;
      } else if (confirmation && !(cause instanceof PreviewConflictError)) unknown = true;
      else error = cause instanceof Error ? cause.message : 'Google Reviews request failed.';
    } finally { if (current === generation) busy = false; }
  }
  function chooseTarget(value: string) {
    invalidate(); invalidateArchive(); clearContent(); target = value; error = ''; archiveError = ''; archiveResult = undefined; archiveSync = undefined; archivePage = 1; receiptID = ''; receipt = undefined; receiptUnavailable = false; receiptReviewID = ''; receiptTarget = ''; recoveryInspected = false; unknown = false;
    if (value) { void list(); void refreshArchiveStatus(); }
  }

  async function refreshArchiveStatus() {
    if (!target) return;
    invalidateArchive();
    const current = archiveGeneration;
    archiveController = new AbortController(); archiveBusy = true; archiveError = '';
    try {
      const result = await control(client, { action: 'archive_status', target }, archiveController.signal);
      if (current === archiveGeneration) archiveSync = result.archive_sync;
    } catch (cause) {
      if (current === archiveGeneration) archiveError = cause instanceof Error ? cause.message : 'Google Reviews archive status failed.';
    } finally { if (current === archiveGeneration) archiveBusy = false; }
  }

  async function startBackfill() {
    if (!target) return;
    invalidateArchive();
    const current = archiveGeneration;
    archiveController = new AbortController(); archiveBusy = true; archiveError = '';
    try {
      const result = await control(client, { action: 'backfill', target }, archiveController.signal);
      if (current === archiveGeneration) archiveSync = result.archive_sync;
    } catch (cause) {
      if (current === archiveGeneration) archiveError = cause instanceof Error ? cause.message : 'Google Reviews backfill could not start.';
    } finally { if (current === archiveGeneration) archiveBusy = false; }
  }

  async function findArchive(page = 1) {
    if (!archiveQuery.trim() || archiveQueryBytes > 256) return;
    invalidateArchive();
    const current = archiveGeneration;
    archiveController = new AbortController(); archiveBusy = true; archiveError = '';
    try {
      const result = await searchArchive(client, { q: archiveQuery, target: target || undefined, page, page_size: 20 }, archiveController.signal);
      if (current === archiveGeneration) { archiveResult = result; archivePage = page; }
    } catch (cause) {
      if (current === archiveGeneration) { archiveResult = undefined; archiveError = cause instanceof Error ? cause.message : 'Google Reviews archive search failed.'; }
    } finally { if (current === archiveGeneration) archiveBusy = false; }
  }
  function clearResolvedReceipt() {
    if (receipt?.status !== 'submitted' && receipt?.status !== 'failed') return;
    receiptID = ''; receipt = undefined; receiptUnavailable = false; receiptReviewID = ''; receiptTarget = ''; recoveryInspected = false; unknown = false;
  }
  function list(pageToken = '') {
    clearContent(); clearResolvedReceipt();
    return request({ action: 'list', target, page_token: pageToken, page_size: 20 }, result => {
      reviews = result.reviews ?? []; nextPage = result.next_page_token ?? ''; loaded = true;
    });
  }
  function detail(id: string) {
    review = undefined; preview = undefined; comment = ''; clearResolvedReceipt();
    return request({ action: 'get', target, review_id: id }, result => {
      review = result.review;
      if (receiptID && target === receiptTarget && id === receiptReviewID && result.review?.reviewId === id) {
        recoveryInspected = true;
      }
    });
  }
  function prepare() {
    return request({ action: 'preview_reply', target, review_id: review?.reviewId, comment }, result => {
      if (!result.preview?.confirmation_token || !result.preview.receipt_id) throw new Error('Preview unavailable. Check owner permission to submit replies.');
      preview = result.preview;
    });
  }
  function confirm() {
    if (!preview?.confirmation_token || !preview.receipt_id) return;
    const token = preview.confirmation_token;
    receiptID = preview.receipt_id;
    receipt = undefined; receiptUnavailable = false; receiptReviewID = preview.review.reviewId ?? ''; receiptTarget = target; recoveryInspected = false; unknown = false;
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
      receipt = result.receipt; receiptUnavailable = false; unknown = receipt?.status === 'unknown';
    });
  }
  onMount(() => {
    void request({ action: 'capabilities' }, result => { capabilities = result.capabilities ?? []; targets = result.targets ?? []; });
    return () => { invalidate(); invalidateArchive(); clearContent(); targets = []; capabilities = []; receiptID = ''; receipt = undefined; archiveResult = undefined; archiveSync = undefined; };
  });
</script>

<section class="google-reviews" aria-label="Google Reviews">
  <PageHeader title="Google Reviews" description="Use your own approved Google project." />
  {#each capabilities.filter(item => !item.available) as capability}
    <p class="notice">{capability.provider === 'facebook' ? 'Facebook' : capability.provider} unavailable: {capability.reason ?? 'Official capability is unverified.'}</p>
  {/each}
  <div class="controls">
    <label for="google-review-target">Target</label>
    <select id="google-review-target" value={target} onchange={event => chooseTarget(event.currentTarget.value)}>
      <option value="">None</option>
      {#each targets as item}<option value={item.name} disabled={!item.enabled}>{item.name}{!item.enabled ? ' (disabled)' : ''}</option>{/each}
    </select>
    {#if target}<Button label="Refresh reviews" disabled={busy} onclick={() => void list()} />{/if}
  </div>
  {#if !busy && targets.length === 0}<p>No targets configured. Set up an own-project Google target in daemon configuration.</p>{/if}
  {#if busy}<p role="status">Loading…</p>{/if}
  {#if error}<p role="alert">{error}</p>{/if}
  {#if receiptUnavailable && recoveryInspected}<p role="status">Receipt unavailable. The live review was inspected. A new reply needs a fresh preview.</p>
  {:else if receiptUnavailable}<p role="status">Receipt unavailable. The original confirmation remains consumed. Inspect the live review before creating a fresh preview.</p>
  {:else if unknown && recoveryInspected}<p role="status">Outcome unknown. The live review was inspected. A new reply needs a fresh preview. This submission will not be replayed.</p>
  {:else if unknown}<p role="status">Outcome unknown. Check the receipt and inspect the live review before any new preview. This submission will not be replayed.</p>{/if}
  {#if receipt}
    <div role="status">
      {#if receipt.status === 'submitted'}<p>Submitted to Google. Public visibility may depend on moderation.</p>
      {:else if receipt.status === 'failed'}<p>Submission failed. Inspect the live review before creating a new preview.</p>
      {:else if receipt.status === 'submitting'}<p>Submission is in progress.</p>
      {:else if receipt.status === 'pending_confirmation'}<p>Receipt is awaiting confirmation.</p>{/if}
      <p>Receipt: <code>{receipt.id}</code></p>
    </div>
  {/if}
  {#if receiptID && !receiptUnavailable}<Button label="Check receipt" disabled={busy} onclick={() => void checkReceipt()} />{/if}
  {#if receiptUnavailable && !recoveryInspected && receiptReviewID && target === receiptTarget}<Button label="Inspect live review" disabled={busy} onclick={() => void detail(receiptReviewID)} />{/if}
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
        {#if !matchingListReview || matchingListReview.starRating !== review.starRating}<p>{review.starRating ?? 'Rating unavailable'}</p>{/if}
        <p class="exact">{review.comment || 'Rating only; no review text.'}</p>
        {#if review.updateTime}<p class="muted">Updated {review.updateTime}</p>{/if}
        {#if review.reviewReply}<h3>Current owner response</h3><p class="exact">{review.reviewReply.comment}</p>{/if}
        {#if review.reviewReply?.reviewReplyState && matchingListReview?.reviewReply?.reviewReplyState !== review.reviewReply.reviewReplyState}<p>Response status: {review.reviewReply?.reviewReplyState}</p>{/if}
        {#if review.reviewReply?.policyViolation && matchingListReview?.reviewReply?.policyViolation !== review.reviewReply.policyViolation}<p>Policy status: {review.reviewReply?.policyViolation}</p>{/if}
        {#if canReply && (!receiptID || ((receiptUnavailable || unknown || receipt?.status === 'unknown') && recoveryInspected && target === receiptTarget && review.reviewId === receiptReviewID))}
          <label for="public-response">Public response</label>
          <textarea id="public-response" bind:value={comment} disabled={busy} rows="5"></textarea>
          <p class="muted">{new TextEncoder().encode(comment).length} / 4096 UTF-8 bytes. Review text is untrusted; inspect your exact response before confirming.</p>
          <Button label="Preview response" disabled={busy || !comment.trim() || new TextEncoder().encode(comment).length > 4096} onclick={() => void prepare()} />
        {:else if !canReply}<p>Responses are disabled for this target.</p>{/if}
      </article>
    {/if}
  </div>
  <section class="archive" aria-label="Google Reviews archive">
    <div class="archive-heading">
      <div>
        <h2>Review archive</h2>
        {#if !archiveSync?.coverage_note}<p class="muted">Backfill follows Google's API pages; page exhaustion does not guarantee lifetime coverage.</p>{/if}
      </div>
      {#if target}
        <Button label="Backfill available history" disabled={archiveBusy || !selected?.enabled} onclick={() => void startBackfill()} />
        <Button label="Refresh archive status" disabled={archiveBusy} onclick={() => void refreshArchiveStatus()} />
      {/if}
    </div>
    {#if archiveSync}
      <p role="status">Archive status: <strong>{archiveStatus(archiveSync.status)}</strong>. Pages fetched: {archiveSync.pages_fetched}; reviews fetched: {archiveSync.reviews_fetched}; archived: {archiveSync.archived_review_count}{#if archiveSync.total_review_count} of Google's reported {archiveSync.total_review_count}{/if}.</p>
      {#if archiveSync.coverage_note}<p class="muted">{archiveSync.coverage_note}</p>{/if}
      {#if archiveSync.last_error_code}<p role="status">{archiveSync.last_error_code === 'cursor_cycle' ? 'Google repeated a page. Restart from the first page; existing reviews are kept.' : 'The import was interrupted. Start backfill again to resume.'}</p>{/if}
    {/if}
    <div class="archive-search">
      <label for="google-review-archive-query">Search archived reviews</label>
      <input id="google-review-archive-query" bind:value={archiveQuery} maxlength="256" onkeydown={event => { if (event.key === 'Enter') { event.preventDefault(); void findArchive(1); } }} />
      <span class="muted">{archiveQueryBytes} / 256 UTF-8 bytes</span>
      <Button label="Search archive" disabled={archiveBusy || !archiveQuery.trim() || archiveQueryBytes > 256} onclick={() => void findArchive(1)} />
    </div>
    {#if archiveError}<p role="alert">{archiveError}</p>{/if}
    {#if archiveBusy}<p role="status">Searching archive…</p>{/if}
    {#if archiveResult}
      {#if archiveResult.total > 0}<p>{archiveResult.total} matching archived review{archiveResult.total === 1 ? '' : 's'}.</p>{/if}
      {#if archiveResult.results.length === 0}<p>No archived reviews match this query.</p>{/if}
      <div class="archive-results">
        {#each archiveResult.results as item}
          <article class="archive-result">
            <p><strong>{item.review.reviewer.isAnonymous ? 'Anonymous reviewer' : item.review.reviewer.displayName || 'Reviewer'}</strong> · {item.review.starRating || 'Rating unavailable'} · {item.target}</p>
            <p class="exact">{item.review.comment || 'Rating only; no review text.'}</p>
            {#if item.review.reviewReply}<p><strong>Owner response:</strong> <span class="exact">{item.review.reviewReply.comment}</span></p>{/if}
            <p class="muted">Created {item.review.createTime || 'unknown'} · Archived {item.archived_at}</p>
          </article>
        {/each}
      </div>
      {#if archivePages > 1}<div class="archive-pagination"><Button label="Previous" disabled={archiveBusy || archivePage <= 1} onclick={() => void findArchive(archivePage - 1)} /><span>Page {archivePage} of {archivePages}</span><Button label="Next" disabled={archiveBusy || archivePage >= archivePages} onclick={() => void findArchive(archivePage + 1)} /></div>{/if}
    {/if}
  </section>
</section>

{#if preview}
  <Dialog title="Confirm public response" primaryLabel="Submit" primaryDisabled={busy || !preview.expires_at || Date.parse(preview.expires_at) <= Date.now()} onprimary={() => void confirm()} onclose={() => { preview = undefined; }}>
    <div class="preview-content">
    <p>Target: <strong>{target}</strong></p>
    <p>Review by {preview.review.reviewer.isAnonymous ? 'Anonymous reviewer' : preview.review.reviewer.displayName || 'Reviewer'}</p>
    <p class="exact">{preview.review.comment || 'Rating only; no review text.'}</p>
    {#if preview.review.reviewReply}<h3>Current owner response</h3><p class="exact">{preview.review.reviewReply.comment}</p>{/if}
    <p>Receipt reserved before submission: <code>{preview.receipt_id}</code></p>
    <h3>Your response</h3>
    <pre class="exact" data-testid="exact-response">{preview.comment}</pre>
    <p>Confirm that you consent to submit this exact response to Google. Submission does not guarantee public visibility.</p>
    <p>Preview expires {preview.expires_at}. If the review or current response changes, create a new preview.</p>
    <Button label="Keep editing" onclick={() => { preview = undefined; }} />
    </div>
  </Dialog>
{/if}

<style>
  .google-reviews { flex: 1; min-height: 0; overflow: auto; padding: var(--space-5) var(--page-gutter); display: flex; flex-direction: column; gap: var(--space-4); }
  .preview-content { display: grid; gap: var(--space-3); }
  .controls { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-3); }
  select, textarea, input { border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-surface); color: var(--text-primary); font: inherit; padding: var(--space-2); }
  select { max-width: 100%; }
  .columns { display: grid; grid-template-columns: minmax(180px, 1fr) minmax(0, 2fr); gap: var(--space-5); }
  .review-list, article { display: flex; flex-direction: column; align-items: stretch; gap: var(--space-3); min-width: 0; }
  .review-row { display: flex; flex-wrap: wrap; justify-content: space-between; gap: var(--space-3); padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-surface); color: var(--text-primary); text-align: left; cursor: pointer; }
  .review-row:hover { background: var(--bg-surface-hover); }
  .muted { color: var(--text-muted); font-size: var(--font-size-sm); }
  .exact { white-space: pre-wrap; overflow-wrap: anywhere; font: inherit; }
  .notice { padding: var(--space-3); background: var(--bg-subtle); border: 1px solid var(--border-default); border-radius: var(--radius-md); }
  .archive { border-top: 1px solid var(--border-default); padding-top: var(--space-5); display: flex; flex-direction: column; gap: var(--space-3); }
  .archive-heading, .archive-search, .archive-pagination { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-3); }
  .archive-heading { justify-content: space-between; }
  .archive-heading > div { display: grid; gap: var(--space-2); }
  .archive-search input { min-width: min(24rem, 100%); flex: 1; }
  .archive-results { display: grid; gap: var(--space-3); }
  .archive-result { border: 1px solid var(--border-default); border-radius: var(--radius-md); padding: var(--space-3); display: grid; gap: var(--space-2); }
  .archive-pagination { justify-content: flex-end; }
  h2, h3, p { margin: 0; }
  h2 { font-size: var(--font-size-lg); }
  h3 { font-size: var(--font-size-md); }
  @media (max-width: 760px) { .columns { grid-template-columns: minmax(0, 1fr); } }
</style>
