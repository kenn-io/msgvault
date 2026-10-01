<script lang="ts">
  import { Button, EmptyState, SegmentedControl, SelectDropdown, Spinner } from '@kenn-io/kit-ui';
  import { tick } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { DirectoryReviewKind, IdentityReviewState } from '../../explore/models';
  import type { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';
  import type {
    DirectoryReviewContextSnapshot,
    DirectoryReviewController,
    IdentityMatchCandidate
  } from '../../directory/review-controller.svelte';
  import IdentityCandidateCard from './IdentityCandidateCard.svelte';
  import IdentityDecisionModal from './IdentityDecisionModal.svelte';
  import FactReviewPanel from './FactReviewPanel.svelte';
  import RelationshipReviewQueue from './RelationshipReviewQueue.svelte';
  import PageHeader from '../shell/PageHeader.svelte';
  import type { RelationshipReviewController } from '../../directory/relationship-review-controller.svelte';
  import PersonBindingConflictModal from './PersonBindingConflictModal.svelte';
  import type { PersonMergeSuccess, ValidatedPersonMergeRequired } from '../../directory/person-merge';

  interface Props {
    controller: DirectoryReviewController;
    relationshipController: RelationshipReviewController;
    factController?: FactLedgerController;
    directoryPersonID?: number | null;
    client: APIClient;
    onSelectFactPerson?: (personID: number) => void;
    onOpenPerson?: (personID: number) => void;
    onAnnounce?: (message: string) => void;
  }

  let {
    controller,
    relationshipController,
    factController = undefined,
    directoryPersonID = null,
    client,
    onSelectFactPerson = () => undefined,
    onOpenPerson = () => undefined,
    onAnnounce = () => undefined
  }: Props = $props();
  type ActiveModal =
    | { kind: 'decision'; candidate: IdentityMatchCandidate; decision: 'accept' | 'reject'; context: DirectoryReviewContextSnapshot }
    | { kind: 'merge'; candidate: IdentityMatchCandidate; context: DirectoryReviewContextSnapshot; conflict: ValidatedPersonMergeRequired };
  let activeDecision = $state<ActiveModal>();
  let identityReviewHeading = $state<HTMLHeadingElement>();

  const reviewKindOptions = [
    { value: 'identity', label: 'Identity matches' },
    { value: 'fact', label: 'Facts' },
    { value: 'relationship', label: 'Imported relationships' }
  ];
  const identityStateOptions = [
    { value: 'candidate', label: 'Candidate' },
    { value: 'conflict', label: 'Conflict' },
    { value: 'accepted', label: 'Accepted' },
    { value: 'rejected', label: 'Rejected' }
  ];

  function selectReviewKind(value: string): void {
    const kind = value as DirectoryReviewKind;
    controller.setReviewKind(kind);
    relationshipController?.applyContext(kind === 'relationship', relationshipController.state, false);
  }

  function selectIdentityState(value: string): void {
    controller.setIdentityState(value as IdentityReviewState);
  }

  function openDecision(candidate: IdentityMatchCandidate, decision: 'accept' | 'reject'): void {
    activeDecision = { kind: 'decision', candidate, decision, context: controller.reviewContextSnapshot() };
  }

  function resolveMerge(conflict: ValidatedPersonMergeRequired): void {
    if (!activeDecision || activeDecision.kind !== 'decision') return;
    activeDecision = {
      kind: 'merge', candidate: activeDecision.candidate, context: activeDecision.context, conflict
    };
  }

  function completeMerge(success: PersonMergeSuccess): void {
    if (!activeDecision || activeDecision.kind !== 'merge') return;
    const origin = activeDecision;
    void controller.completePersonMerge(origin.candidate.id, origin.context, success);
    activeDecision = undefined;
    const name = success.survivor.display_name?.trim() || `Person ${success.survivor.id}`;
    onAnnounce(`People merged into ${name}. Identity cache ${success.result.cache_state}.`);
    onOpenPerson(success.survivor.id);
  }

  async function focusCurrentReviewSurface(): Promise<void> {
    await tick();
    const target = controller.reviewKind === 'fact'
      ? document.getElementById('fact-review-heading')
      : controller.reviewKind === 'relationship'
        ? document.getElementById('relationship-review-heading')
        : identityReviewHeading;
    if (target?.isConnected) target.focus();
  }

  async function invalidateDecision(): Promise<void> {
    if (!activeDecision) return;
    activeDecision = undefined;
    await focusCurrentReviewSurface();
  }

  async function closeDecision(): Promise<void> {
    const closed = activeDecision;
    activeDecision = undefined;
    if (!closed) return;
    await tick();
    const card = document.getElementById(`identity-match-${closed.candidate.id}-card`);
    const label = closed.kind === 'decision' && closed.decision === 'reject' ? 'Keep separate' : 'Link identities';
    const action = Array.from(card?.querySelectorAll<HTMLButtonElement>('button') ?? [])
      .find((button) => button.textContent?.trim() === label);
    const target = action ?? card;
    if (target?.isConnected) {
      target.focus();
      return;
    }
    await focusCurrentReviewSurface();
  }

  $effect(() => {
    const decision = activeDecision;
    if (decision && !controller.isReviewContextCurrent(decision.context)) {
      void invalidateDecision();
    }
  });
</script>

<main class="review-centre" aria-label="Reviews">
  <PageHeader title="Reviews" description="Decide which identities and facts belong together.">
    {#snippet view()}
      <SegmentedControl
        options={reviewKindOptions}
        value={controller.reviewKind}
        onchange={selectReviewKind}
        ariaLabel="Review type"
        disabled={!!activeDecision}
      />
    {/snippet}
  </PageHeader>

  {#if controller.reviewKind === 'identity'}
    <section class="identity-review" aria-labelledby="identity-review-heading" data-review-section>
      <h2 bind:this={identityReviewHeading} id="identity-review-heading" class="kit-sr-only review-heading" tabindex="-1">Identity matches</h2>
      <div class="review-toolbar">
        <SelectDropdown
          title="Identity review state"
          value={controller.identityState}
          options={identityStateOptions.map((option) => ({ ...option, triggerLabel: `Show: ${option.label}` }))}
          onchange={selectIdentityState}
          disabled={!!activeDecision}
        />
      </div>

      {#if controller.status}
        <p class="status" role="status" aria-live="polite">{controller.status}</p>
      {/if}

      {#if controller.loading && controller.rows.length === 0}
        <p class="loading">
          <Spinner size={12} label="Loading identity matches" /> Loading identity matches…
        </p>
      {:else if controller.error}
        <div class="message" role="alert">
          <p>{controller.error}</p>
          <Button label="Retry identity matches" size="sm" onclick={() => void controller.retryPage()} />
        </div>
      {:else}
        {#if controller.pageError}
          <div class="message" role="alert">
            <p>{controller.pageError}</p>
            <Button label="Retry identity matches" size="sm" onclick={() => void controller.retryPage()} />
          </div>
        {/if}

        {#if controller.rows.length === 0}
          <EmptyState
            title="No identity matches in this queue."
            description="Choose another review state or return when new evidence is available."
          />
        {:else}
          <div class="queue" aria-busy={controller.loading}>
            {#if controller.loading}
              <div class="loading-overlay">
                <Spinner size={12} label="Loading next review page" /> Loading page…
              </div>
            {/if}
            <div class="candidate-list">
              {#each controller.rows as row (row.id)}
                <IdentityCandidateCard
                  candidate={row}
                  pending={controller.isDecisionPending(row.id)}
                  onAccept={() => openDecision(row, 'accept')}
                  onReject={() => openDecision(row, 'reject')}
                />
              {/each}
            </div>
          </div>
        {/if}

        {#if controller.rows.length > 0 || controller.hasPreviousPage}
          <nav class="pagination" aria-label="Identity review pages">
            <Button
              label="Previous page"
              size="sm"
              disabled={controller.loading || !controller.hasPreviousPage}
              onclick={() => void controller.loadPreviousPage()}
            />
            <span>Offset {controller.offset}</span>
            <Button
              label="Next page"
              size="sm"
              disabled={controller.loading || !controller.hasNextPage}
              onclick={() => void controller.loadNextPage()}
            />
          </nav>
        {/if}
      {/if}
    </section>
  {:else if controller.reviewKind === 'fact'}
    {#if factController}
      <FactReviewPanel
        controller={factController}
        personID={directoryPersonID}
        {client}
        {onSelectFactPerson}
        {onOpenPerson}
      />
    {/if}
  {:else}
    <RelationshipReviewQueue controller={relationshipController} {onOpenPerson} />
  {/if}
</main>

{#if activeDecision?.kind === 'decision'}
  <IdentityDecisionModal
    {controller}
    candidate={activeDecision.candidate}
    decision={activeDecision.decision}
    reviewContext={activeDecision.context}
    onClose={() => void closeDecision()}
    onContextInvalidated={() => void invalidateDecision()}
    onResolveMerge={resolveMerge}
  />
{:else if activeDecision?.kind === 'merge'}
  <PersonBindingConflictModal
    client={controller.apiClient}
    conflict={activeDecision.conflict}
    onOpenProfile={onOpenPerson}
    onSuccess={completeMerge}
    onClose={() => void closeDecision()}
  />
{/if}

<style>
  .review-centre { display: grid; gap: var(--space-5); padding: var(--space-5) var(--page-gutter) var(--space-4); }
  .review-toolbar { display: flex; align-items: center; gap: var(--space-3); flex-wrap: wrap; }
  h2, p { margin: 0; }
  .identity-review { display: grid; gap: var(--space-4); }
  [data-review-section] { position: relative; border-radius: var(--radius-md); }
  [data-review-section]:has(> .review-heading:focus-visible) {
    outline: 2px solid var(--focus-color); outline-offset: 4px;
  }
  .status { color: var(--text-secondary); }
  .loading { display: flex; align-items: center; gap: var(--space-2); color: var(--text-muted); }
  .message { display: grid; justify-items: start; gap: var(--space-2); padding: var(--space-3); border-left: 2px solid var(--accent-red); color: var(--text-secondary); }
  .queue { position: relative; min-width: 0; }
  .candidate-list { display: grid; gap: var(--space-4); }
  .loading-overlay { position: sticky; z-index: 1; top: var(--space-2); display: flex; align-items: center; justify-content: center; gap: var(--space-2); width: fit-content; margin: 0 auto calc(-1 * var(--space-8)); padding: var(--space-2) var(--space-4); border: var(--border-width) solid var(--border-default); border-radius: var(--radius-pill); background: var(--bg-surface); box-shadow: var(--shadow-sm); color: var(--text-muted); }
  .pagination { display: flex; align-items: center; justify-content: center; gap: var(--space-3); color: var(--text-muted); font-size: var(--font-size-sm); }
</style>
