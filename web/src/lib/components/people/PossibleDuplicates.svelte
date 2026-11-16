<script lang="ts">
  import { Disclosure, Button, EmptyState, SegmentedControl } from '@kenn-io/kit-ui';

  import { tick, untrack } from 'svelte';
  import type { IdentityMatchCandidate } from '../../api/generated/models';
  import {
    type EndpointSummary,
    endpointKey,
  } from '../../people/duplicates';
  import { formatRelativeTime, messageCount } from '../../util/format';
  import { identityBasisLabel } from '../../relationships/identity-chip';
  import IdentityAvatar from '../common/IdentityAvatar.svelte';
  import { IDENTITY_REVIEW_PAGE_LIMIT, type DirectoryReviewController } from '../../directory/review-controller.svelte';
  import IdentityCandidateCard from '../directory/IdentityCandidateCard.svelte';

  /** The names and values that identify a match, joined for one quiet line. */
  function joinParts(parts: Array<string | null | undefined | false>): string {
    return parts.filter(Boolean).join(' · ');
  }

  /** The reason and matched value on a pair card. */
  function basisLine(candidate: {
    basis: string;
    normalized_value?: string;
  }): string {
    const basis = identityBasisLabel(candidate.basis);
    const label = basis.replace(/^./, letter => letter.toUpperCase());
    return candidate.normalized_value ? `${label}: ${candidate.normalized_value}` : label;
  }

  /** A readable reason a conflict can't apply as it stands. */
  function blockerLine(blocker: string | undefined): string {
    switch (blocker) {
      case 'person_merge_required':
        return 'Both are saved as different people, so merging needs a choice of which to keep.';
      case undefined:
      case '':
        return '';
      default:
        return blocker.replaceAll('_', ' ').replace(/^./, (letter) => letter.toUpperCase()) + '.';
    }
  }

  interface Props {
    controller: DirectoryReviewController;
    /** Hides the card for the Undo window, then makes the decision. */
    onDecide: (candidate: IdentityMatchCandidate, decision: 'accept' | 'reject') => void;
  }

  let { controller, onDecide }: Props = $props();
  $effect(() => { void controller.rows; untrack(() => controller.requestSummaries()); });
  const history = $derived(controller.identityState === 'accepted' || controller.identityState === 'rejected');

  // Focus returns to a merged pair once per merge, not on each later reload of its page.
  let focusedMerge: DirectoryReviewController['lastMerge'] = null;
  $effect(() => {
    const merge = controller.lastMerge, id = merge === focusedMerge ? undefined : merge?.candidateID;
    if (!id || controller.loading || !controller.rows.some(row => row.id === id)) return;
    focusedMerge = merge;
    void tick().then(() => document.getElementById(`duplicate-${id}`)?.querySelector<HTMLButtonElement>('.footer button:last-child')?.focus());
  });

  function summaryOf(key: string): EndpointSummary | 'failed' | undefined {
    return controller.summaries.get(key);
  }

  function nameOf(key: string): string {
    const summary = summaryOf(key);
    return summary && summary !== 'failed' ? summary.name : summary === 'failed' ? 'Person details unavailable' : 'Loading person';
  }

  // A match between two saved people still merges: the server asks which one to keep.
  function blocked(candidate: IdentityMatchCandidate): boolean {
    return !candidate.actionable && candidate.blocker !== 'person_merge_required';
  }

  function conflicted(candidate: IdentityMatchCandidate): boolean {
    return candidate.state === 'conflict' || Boolean(candidate.blocker);
  }

  const decidedLabel: Record<string, string> = { accepted: 'Merged', rejected: 'Kept separate' };
</script>

<div class="duplicates">
  <div class="toolbar">
    <SegmentedControl
      ariaLabel="Show"
      value={history ? 'history' : 'waiting'}
      disabled={controller.submitting}
      options={[
        { value: 'waiting', label: 'To review' },
        { value: 'history', label: 'History' },
      ]}
      onchange={(value) => controller.setIdentityState(value === 'history' ? 'accepted' : 'candidate')}
    />
  </div>

  {#if controller.error}
    <EmptyState title={controller.error} /><Button label="Retry" disabled={controller.submitting} onclick={() => void controller.retryPage()} />
  {:else if controller.loading && controller.rows.length === 0}
    <p class="status" role="status">Loading…</p>
  {:else if controller.rows.filter(row => !controller.hidden.has(row.id)).length === 0}
    <EmptyState title={history ? 'Nothing decided yet' : 'No possible duplicates'} />
  {:else}
    <ul class="cards">
      {#each controller.rows.filter(row => !controller.hidden.has(row.id)) as candidate (candidate.id)}
        {@const pair = [endpointKey(candidate, 'left'), endpointKey(candidate, 'right')] as const}
        {@const decided = history || candidate.state in decidedLabel}
        <li id={`duplicate-${candidate.id}`} class="card" tabindex="-1" aria-label={`${nameOf(pair[0])} and ${nameOf(pair[1])}`}>
          <div class="people">
            {#each pair as id (id)}
              {@const summary = summaryOf(id)}
              <div class="person">
                {#if summary && summary !== 'failed'}<IdentityAvatar label={summary.name} seed={id} size={32} />{:else}<span class="avatar-placeholder" aria-hidden="true"></span>{/if}
                <div class="text">
                  {#if summary && summary !== 'failed'}
                    <span class="name">{summary.name}</span>
                    {#if summary.address !== summary.name}<span class="detail">{summary.address}</span>{/if}
                    <span class="detail" data-mono>{joinParts([summary.lastAt ? formatRelativeTime(summary.lastAt) : '', summary.count === undefined ? '' : messageCount(summary.count)])}</span>
                  {:else}
                    <span class="name placeholder" aria-hidden="true">&nbsp;</span>
                    {#if !summary}<span class="detail">Loading…</span>{:else}<Button label="Retry person" size="sm" onclick={() => controller.retrySummary(id)} />{/if}
                    <span class="detail" aria-hidden="true">&nbsp;</span>
                  {/if}
                </div>
              </div>
            {/each}
          </div>
          <div class="footer">
            <span class="basis">
              {#if decided}
                {candidate.application_pending ? 'Merge pending' : decidedLabel[candidate.state] ?? candidate.state} · {formatRelativeTime(candidate.decided_at ?? candidate.updated_at)}
              {:else if conflicted(candidate)}
                {basisLine(candidate)}. {blockerLine(candidate.blocker)}
              {:else}
                {basisLine(candidate)}
              {/if}
            </span>
            {#if !decided}
              <Button
                label="Keep separate"
                disabled={controller.submitting}
                onclick={() => onDecide(candidate, 'reject')}
              />
              <Button
                tone="info"
                surface="solid"
                label="Merge"
                disabled={blocked(candidate) || controller.submitting}
                onclick={() => onDecide(candidate, 'accept')}
              />
            {/if}
          </div>
          {#if !decided && controller.lastMerge?.candidateID === candidate.id}<p class="status" role="status">Profiles merged into {controller.lastMerge.survivor.display_name || `Person ${controller.lastMerge.survivor.id}`}. Choose Merge to record this pair.</p>{/if}
          <Disclosure variant="plain">{#snippet summary()}Evidence and notes{/snippet}
            <IdentityCandidateCard {candidate} pending={controller.isDecisionPending(candidate.id)} />
            {#if !decided}<label>Decision notes<textarea value={controller.getDecisionDraft(candidate.id)} oninput={(event) => controller.setDecisionDraft(candidate.id, event.currentTarget.value)}></textarea></label>{/if}
          </Disclosure>
        </li>
      {/each}
    </ul>
  {/if}
  {#if controller.decisionError}<p role="alert">{controller.decisionError}</p>{/if}
  {#if controller.status && !controller.rows.some(row => row.id === controller.lastMerge?.candidateID)}<p role="status">{controller.status}</p>{/if}
  {#if controller.pageError}<p role="alert">{controller.pageError}<Button label="Retry page" disabled={controller.submitting} onclick={() => void controller.retryPage()} /></p>{/if}
  <nav aria-label="Duplicate pages"><Button label="Previous" disabled={controller.submitting || !controller.hasPreviousPage} onclick={() => void controller.loadPreviousPage()} /><span>Page {Math.ceil(controller.offset / IDENTITY_REVIEW_PAGE_LIMIT) + 1}</span><Button label="Next" disabled={controller.submitting || !controller.hasNextPage} onclick={() => void controller.loadNextPage()} /></nav>
</div>

<style>
  nav { display: flex; align-items: center; gap: var(--control-gap); }
  .duplicates :global(.kit-disclosure), label { display: grid; gap: var(--control-gap); }
  .duplicates {
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    gap: var(--field-gap);
    overflow: auto;
    padding: var(--section-gap) var(--page-gutter);
  }

  .toolbar {
    display: flex;
  }

  .cards {
    display: grid;
    max-width: 880px;
    margin: 0;
    padding: 0;
    gap: var(--field-gap);
    list-style: none;
  }

  .card {
    display: grid;
    gap: var(--field-gap);
    padding: var(--pane-pad-x);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
  }

  .people {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--section-gap);
  }

  .avatar-placeholder { width: 32px; height: 32px; flex-shrink: 0; border-radius: var(--radius-md); background: var(--bg-inset); }
  .placeholder { background: var(--bg-inset); width: 60%; }
  .name, .detail { min-height: 1lh; }

  .person {
    display: flex;
    min-width: 0;
    align-items: flex-start;
    gap: var(--control-gap);
  }

  .text {
    display: flex;
    min-width: 0;
    flex-direction: column;
    gap: var(--space-1);
  }

  .name {
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-medium, 500);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .detail {
    overflow: hidden;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .footer {
    display: flex;
    align-items: center;
    gap: var(--control-gap);
    padding-top: var(--control-gap);
    border-top: 1px solid var(--border-muted);
  }

  .basis {
    min-width: 0;
    flex: 1;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .status {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  @media (max-width: 640px) {
    .footer { flex-wrap: wrap; justify-content: flex-end; }
    .basis { flex-basis: 100%; }
    .people {
      grid-template-columns: 1fr;
    }
  }
</style>
