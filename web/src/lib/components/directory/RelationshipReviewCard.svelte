<script lang="ts">
  import { Button, Card } from '@kenn-io/kit-ui';

  import type { RelationshipReviewRow } from '../../directory/relationship-review-controller.svelte';

  interface Props {
    review: RelationshipReviewRow;
    onOpenPerson?: (personID: number) => void;
  }

  let { review, onOpenPerson = () => undefined }: Props = $props();
  const headingID = $derived(`imported-relationship-review-${review.id}-heading`);

  function positiveID(value: number | undefined): value is number {
    return value !== undefined && Number.isSafeInteger(value) && value > 0;
  }

  function openPerson(personID: number): void {
    if (positiveID(personID)) onOpenPerson(personID);
  }
</script>

<Card level="default" padding="md">
  <article class="relationship-review" aria-labelledby={headingID} tabindex="-1">
    <header>
      <p class="state">{review.status}</p>
      <h3 id={headingID}>Imported relationship review {review.id}</h3>
    </header>

    <dl class="metadata">
      <div class="raw-value"><dt>Related value</dt><dd>{review.raw_related_value}</dd></div>
      <div><dt>Related type</dt><dd>{review.raw_related_type}</dd></div>
      <div><dt>Value kind</dt><dd>{review.value_kind}</dd></div>
      <div><dt>Status</dt><dd>{review.status}</dd></div>
      <div><dt>Source</dt><dd>{review.source}</dd></div>
      <div><dt>Created</dt><dd><time datetime={review.created_at}>{review.created_at}</time></dd></div>
      <div><dt>Updated</dt><dd><time datetime={review.updated_at}>{review.updated_at}</time></dd></div>
      {#if review.reviewed_at}
        <div><dt>Reviewed</dt><dd><time datetime={review.reviewed_at}>{review.reviewed_at}</time></dd></div>
      {/if}
    </dl>

    {#if positiveID(review.person_id) || positiveID(review.matched_person_id)}
      <div class="actions">
        {#if positiveID(review.person_id)}
          <Button label="Open owner profile" size="sm" onclick={() => openPerson(review.person_id)} />
        {/if}
        {#if positiveID(review.matched_person_id)}
          <Button label="Open matched profile" size="sm" onclick={() => openPerson(review.matched_person_id!)} />
        {/if}
      </div>
    {/if}
  </article>
</Card>

<style>
  .relationship-review { display: grid; gap: var(--space-4); min-width: 0; }
  header { display: grid; gap: var(--space-1); }
  h3, p, dl, dd { margin: 0; }
  h3 { color: var(--text-primary); font-size: var(--font-size-lg); }
  .state { color: var(--text-muted); font-size: var(--font-size-xs); font-weight: var(--font-weight-semibold, 600); text-transform: uppercase; letter-spacing: 0.04em; }
  .metadata { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: var(--space-3) var(--space-5); }
  .metadata > div { display: grid; gap: var(--space-1); min-width: 0; }
  .raw-value { grid-column: 1 / -1; }
  dt { color: var(--text-muted); font-size: var(--font-size-xs); }
  dd { color: var(--text-secondary); overflow-wrap: anywhere; word-break: break-word; }
  .actions { display: flex; flex-wrap: wrap; gap: var(--space-2); }

  @media (max-width: 760px) {
    .metadata { grid-template-columns: minmax(0, 1fr); }
    .raw-value { grid-column: auto; }
    .actions { align-items: stretch; flex-direction: column; }
  }
</style>
