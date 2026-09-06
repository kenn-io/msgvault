<script lang="ts">
  import { Button, Spinner } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import {
    PersonMergeHistoryController,
    type PersonSplitCommittedContext
  } from '../../directory/person-merge-history-controller.svelte';
  import PersonSplitModal from './PersonSplitModal.svelte';

  interface Props {
    client: APIClient;
    personID: number;
    onOpenPerson?: (personID: number) => void;
    onSplitCommitted?: (context: PersonSplitCommittedContext) => void | Promise<void>;
  }

  let {
    client,
    personID,
    onOpenPerson = () => undefined,
    onSplitCommitted = () => undefined
  }: Props = $props();
  const controller = new PersonMergeHistoryController(
    untrack(() => client),
    untrack(() => personID),
    (context) => onSplitCommitted(context)
  );
  let activePersonID = untrack(() => personID);
  let splitTrigger = $state<HTMLButtonElement>();

  $effect(() => {
    if (personID !== activePersonID) {
      activePersonID = personID;
      controller.setPerson(personID);
    }
  });

  onMount(() => void controller.loadHistory());
  onDestroy(() => controller.destroy());

  async function openSplit(event: MouseEvent): Promise<void> {
    splitTrigger = event.currentTarget as HTMLButtonElement;
    await controller.openSplit();
  }

  async function closeSplit(): Promise<void> {
    controller.closeSplit();
    await tick();
    if (splitTrigger?.isConnected) splitTrigger.focus();
  }

  function rowActionCounts(counts: Record<string, number>): string {
    const entries = Object.entries(counts).sort(([left], [right]) => left.localeCompare(right));
    return entries.length ? entries.map(([action, count]) => `${action}: ${count}`).join(', ') : 'None';
  }

  function disposition(splitID: number | undefined): string {
    return splitID === undefined ? 'Not split' : `Split ${splitID}`;
  }

  function snapshotText(value: unknown): string {
    const encoded = JSON.stringify(value, null, 2);
    return encoded === undefined ? String(value) : encoded;
  }
</script>

<section class="merge-history" aria-labelledby={`person-${personID}-merge-history-heading`}>
  <div class="section-heading">
    <div>
      <h3 id={`person-${personID}-merge-history-heading`}>Merge history</h3>
      <p>Inspect durable merge provenance and explicitly restore eligible absorbed lineage.</p>
    </div>
  </div>

  {#if controller.historyLoading && controller.history.length === 0}
    <p role="status"><Spinner size={12} label="Loading merge history" /> Loading merge history…</p>
  {:else if controller.historyError && controller.history.length === 0}
    <div class="message" role="alert"><p>{controller.historyError}</p><Button label="Retry merge history" size="sm" onclick={() => void controller.retryHistory()} /></div>
  {:else}
    {#if controller.historyError}
      <div class="message" role="alert"><p>{controller.historyError}</p><Button label="Retry merge history" size="sm" onclick={() => void controller.retryHistory()} /></div>
    {/if}
    {#if controller.history.length === 0}
      <p>No merge history on this page.</p>
    {:else}
      <div class="table-scroll">
        <table aria-label="Person merge history">
          <thead><tr><th scope="col">Merge</th><th scope="col">Created</th><th scope="col">Survivor</th><th scope="col">Absorbed</th><th scope="col">Current</th><th scope="col">Participants</th><th scope="col">Rows</th><th scope="col">Row actions</th><th scope="col">Review</th><th scope="col">Splits</th><th scope="col">Action</th></tr></thead>
          <tbody>
            {#each controller.history as item (item.merge.id)}
              <tr>
                <th scope="row">{item.merge.id}</th><td>{item.merge.created_at}</td>
                <td>Person {item.merge.survivor_person_id}</td><td>Person {item.merge.absorbed_person_id}</td>
                <td>{item.merge.current_person_id ? `Person ${item.merge.current_person_id}` : 'None'}</td>
                <td>{item.participant_count}</td><td>{item.row_count}</td><td>{rowActionCounts(item.row_action_counts)}</td>
                <td>{item.pending_candidate_count} pending</td><td>{item.split_count}</td>
                <td><Button label={`Inspect merge ${item.merge.id}`} size="sm" onclick={() => void controller.selectMerge(item.merge.id)} /></td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
    <div class="pagination" aria-label="Merge history pagination">
      <Button label="Previous merge history page" size="sm" disabled={!controller.hasPreviousHistoryPage || controller.historyLoading} onclick={() => void controller.previousHistoryPage()} />
      <span>Offset {controller.historyOffset}</span>
      <Button label="Next merge history page" size="sm" disabled={!controller.hasNextHistoryPage || controller.historyLoading} onclick={() => void controller.nextHistoryPage()} />
    </div>
  {/if}

  {#if controller.detailLoading}
    <p role="status"><Spinner size={12} label="Loading merge detail" /> Loading merge detail…</p>
  {:else if controller.detailError && !controller.detail}
    <div class="message" role="alert"><p>{controller.detailError}</p><Button label="Retry merge detail" size="sm" onclick={() => void controller.retryDetail()} /></div>
  {:else if controller.detail}
    <section class="merge-detail" aria-labelledby={`merge-${controller.detail.merge.id}-detail-heading`}>
      <div class="section-heading">
        <div><h4 id={`merge-${controller.detail.merge.id}-detail-heading`}>Merge {controller.detail.merge.id} detail</h4><p>Recorded by {controller.detail.merge.actor} at {controller.detail.merge.created_at}.</p></div>
        {#if controller.canOfferSplit}<Button label="Split merged profile" tone="info" onclick={openSplit} />
        {:else if !controller.detail.merge.current_person_id}<p>No current source profile is recorded, so this merge cannot be split.</p>{/if}
      </div>
      {#if controller.detailError}<p class="error" role="alert">{controller.detailError}</p>{/if}

      <div class="table-scroll">
        <table aria-label="Merge participants"><thead><tr><th scope="col">Participant</th><th scope="col">Origin</th><th scope="col">Disposition</th></tr></thead>
          <tbody>{#each controller.detail.participants ?? [] as participant}<tr><th scope="row">{participant.participant_id}</th><td>{participant.origin_side}</td><td>{disposition(participant.split_id)}</td></tr>{/each}</tbody>
        </table>
      </div>
      <div class="table-scroll">
        <table aria-label="Merge row dispositions"><thead><tr><th scope="col">Table</th><th scope="col">Action</th><th scope="col">Origin</th><th scope="col">Provenance</th><th scope="col">Participant</th><th scope="col">Disposition</th></tr></thead>
          <tbody>{#each controller.detail.rows ?? [] as row}<tr><th scope="row">{row.table_name}</th><td>{row.action}</td><td>{row.origin_side}</td><td>{row.provenance_kind}</td><td>{row.participant_id ?? 'None'}</td><td>{disposition(row.split_id)}</td></tr>{/each}</tbody>
        </table>
      </div>
      <div class="table-scroll">
        <table aria-label="Prior splits"><thead><tr><th scope="col">Split</th><th scope="col">Source</th><th scope="col">Created person</th><th scope="col">Revision change</th><th scope="col">Restoration</th><th scope="col">Actor</th><th scope="col">Created</th></tr></thead>
          <tbody>{#each controller.detail.splits ?? [] as split}<tr><th scope="row">{split.id}</th><td>Person {split.source_person_id}</td><td>Person {split.new_person_id}</td><td>{split.source_revision_before} → {split.source_revision_after}</td><td>{split.exact_reversal ? 'Exact' : 'Partial'}</td><td>{split.actor}</td><td>{split.created_at}</td></tr>{/each}</tbody>
        </table>
      </div>
      <div class="table-scroll">
        <table aria-label="Merge review candidates"><thead><tr><th scope="col">Candidate</th><th scope="col">Person</th><th scope="col">Definition</th><th scope="col">Survivor value</th><th scope="col">Absorbed value</th><th scope="col">Resolution</th><th scope="col">State</th><th scope="col">Reviewed</th><th scope="col">Reviewer</th><th scope="col">Created</th></tr></thead>
          <tbody>{#each controller.detail.review_candidates ?? [] as candidate}<tr><th scope="row">{candidate.id}</th><td>{candidate.person_id}</td><td>{candidate.definition_id}</td><td>{candidate.survivor_value_id}</td><td>{candidate.absorbed_value_id}</td><td>{candidate.resolution_value_id ?? 'None'}</td><td>{candidate.state}</td><td>{candidate.reviewed_at ?? 'Not reviewed'}</td><td>{candidate.reviewed_by ?? 'None'}</td><td>{candidate.created_at}</td></tr>{/each}</tbody>
        </table>
      </div>

      {#if controller.snapshot}
        <p>Verified snapshot version {controller.snapshot.version}. SHA-256 {controller.snapshot.sha256}.</p>
        <!-- Keyboard focus lets a user scroll explicitly revealed content without a pointer. -->
        <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
        <div class="snapshot" role="region" aria-label="Verified merge snapshot content" tabindex="0"><pre>{snapshotText(controller.snapshot.snapshot)}</pre></div>
      {:else if controller.snapshotLoading}<p role="status">Loading verified snapshot…</p>
      {:else}
        {#if controller.snapshotError}<p class="error" role="alert">{controller.snapshotError}</p>{/if}
        <Button label="View verified snapshot" size="sm" onclick={() => void controller.revealSnapshot()} />
      {/if}
    </section>
  {/if}
</section>

{#if controller.splitOpen}
  <PersonSplitModal {controller} {onOpenPerson} onClose={() => void closeSplit()} />
{/if}

<style>
  .merge-history, .merge-detail { display: grid; gap: var(--space-3); }
  .merge-history { padding-top: var(--space-3); border-top: var(--border-width) solid var(--border-default); }
  .section-heading, .pagination { display: flex; justify-content: space-between; align-items: center; gap: var(--space-3); flex-wrap: wrap; }
  h3, h4, p, pre { margin: 0; }
  .section-heading p, .pagination { color: var(--text-muted); font-size: var(--font-size-sm); }
  .table-scroll, .snapshot { max-width: 100%; overflow: auto; }
  table { width: 100%; border-collapse: collapse; font-size: var(--font-size-sm); }
  th, td { padding: var(--space-2); border-bottom: var(--border-width) solid var(--border-default); text-align: left; vertical-align: top; }
  thead th { color: var(--text-muted); white-space: nowrap; }
  .snapshot { max-height: 22rem; padding: var(--space-3); border: var(--border-width) solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-inset); }
  .snapshot:focus-visible { outline: var(--focus-ring); outline-offset: var(--focus-ring-offset, 2px); }
  .snapshot pre { white-space: pre-wrap; overflow-wrap: anywhere; }
  .message { display: flex; gap: var(--space-2); align-items: center; }
  .error { color: var(--text-danger); }
</style>
