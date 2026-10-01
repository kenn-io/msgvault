<script lang="ts">
  import { Button, EmptyState, SelectDropdown, Spinner } from '@kenn-io/kit-ui';
  import { tick } from 'svelte';

  import { reviewStateChip } from '../../directory/labels';
  import type {
    RelationshipReviewController,
    RelationshipReviewState
  } from '../../directory/relationship-review-controller.svelte';
  import RelationshipReviewCard from './RelationshipReviewCard.svelte';

  interface Props {
    controller: RelationshipReviewController;
    onOpenPerson?: (personID: number) => void;
  }

  let { controller, onOpenPerson = () => undefined }: Props = $props();
  let queueHeading = $state<HTMLHeadingElement>();
  let retryAction = $state<HTMLSpanElement>();
  let lastFocusedContext = -1;
  let lastSettledContext = -1;
  const stateOptions = [
    { value: 'pending', label: 'Pending' },
    { value: 'accepted', label: 'Accepted' },
    { value: 'rejected', label: 'Rejected' }
  ];

  function selectState(value: string): void {
    controller.setState(value as RelationshipReviewState);
  }

  async function focusContext(token: number): Promise<void> {
    await tick();
    if (!controller.isContextCurrent(token)) return;
    if (queueHeading?.isConnected) queueHeading.focus();
  }

  async function retry(): Promise<void> {
    const token = controller.contextToken;
    await controller.retry();
    await tick();
    if (!controller.isContextCurrent(token)) return;
    const target = controller.error
      ? retryAction?.querySelector<HTMLButtonElement>('button')
      : queueHeading;
    if (target?.isConnected) target.focus();
  }

  $effect(() => {
    const active = controller.active;
    const token = controller.contextToken;
    const loading = controller.loading;
    if (!active) return;
    if (token !== lastFocusedContext) {
      lastFocusedContext = token;
      void focusContext(token);
    }
    if (!loading && token !== lastSettledContext) {
      lastSettledContext = token;
      void focusContext(token);
    }
  });
</script>

<section class="relationship-queue" aria-labelledby="relationship-review-heading" data-review-section>
  <h2 bind:this={queueHeading} id="relationship-review-heading" class="kit-sr-only review-heading" tabindex="-1">Imported relationships</h2>
  <div class="toolbar">
    <SelectDropdown
      title="Imported relationship review state"
      value={controller.state}
      options={stateOptions.map((option) => ({ ...option, triggerLabel: `Show: ${option.label}` }))}
      onchange={selectState}
      disabled={controller.loading}
    />
    <p class="queue-note">Imported relationship reviews are read-only in the browser until generated decision operations are available.</p>
  </div>

  {#if controller.loading}
    <p class="loading" aria-busy="true">
      <Spinner size={12} label="Loading imported relationship reviews" />
      Loading imported relationship reviews…
    </p>
  {:else if controller.error}
    <div class="message" role="alert">
      <p>{controller.error}</p>
      <span bind:this={retryAction}>
        <Button
          label="Retry imported relationship reviews"
          size="sm"
          onclick={() => void retry()}
        />
      </span>
    </div>
  {:else if controller.rows.length === 0}
    <EmptyState
      title={`No imported relationship reviews in ${reviewStateChip(controller.state).label}.`}
      description="Choose another review state or return after another contact import."
    />
  {:else}
    <div class="review-list">
      {#each controller.rows as review}
        <RelationshipReviewCard {review} {onOpenPerson} />
      {/each}
    </div>
  {/if}
</section>

<style>
  .relationship-queue { display: grid; gap: var(--space-4); min-width: 0; }
  .toolbar { display: flex; align-items: center; gap: var(--space-3); flex-wrap: wrap; }
  h2, p { margin: 0; }
  .queue-note { color: var(--text-muted); }
  .loading { display: flex; align-items: center; gap: var(--space-2); color: var(--text-muted); }
  .message { display: grid; justify-items: start; gap: var(--space-2); padding: var(--space-3); border-left: 2px solid var(--accent-red); color: var(--text-secondary); }
  [data-review-section] { position: relative; border-radius: var(--radius-md); }
  [data-review-section]:has(> .review-heading:focus-visible) {
    outline: 2px solid var(--focus-color); outline-offset: 4px;
  }
  .review-list { display: grid; gap: var(--space-4); min-width: 0; }

  @media (max-width: 760px) {
    .toolbar { align-items: stretch; flex-direction: column; }
  }
</style>
