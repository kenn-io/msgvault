<script lang="ts">
  import { formatDateTime as historyTime } from '../../util/format';
  import { Button, Spinner, Table } from '@kenn-io/kit-ui';
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
    <h3 id={`person-${personID}-merge-history-heading`}>Merges</h3>
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
      <p class="muted">No merges</p>
    {:else}
      <ul class="merges" aria-label="Person merge history">
        {#each controller.history as item (item.merge.id)}
          <li>
            <span>Person {item.merge.absorbed_person_id} merged into person {item.merge.survivor_person_id}
              <small>{historyTime(item.merge.created_at)} · {item.participant_count} {item.participant_count === 1 ? 'address' : 'addresses'}{item.split_count ? ` · split ${item.split_count}×` : ''}{item.pending_candidate_count ? ` · ${item.pending_candidate_count} to review` : ''}</small>
            </span>
            <Button label="Inspect" ariaLabel={`Inspect merge ${item.merge.id}`} size="sm" onclick={() => void controller.selectMerge(item.merge.id)} />
          </li>
        {/each}
      </ul>
    {/if}
    {#if controller.hasPreviousHistoryPage || controller.hasNextHistoryPage}<div class="pagination" aria-label="Merge history pagination">
      <Button label="Previous merge history page" size="sm" disabled={!controller.hasPreviousHistoryPage || controller.historyLoading} onclick={() => void controller.previousHistoryPage()} />
      <span>Offset {controller.historyOffset}</span>
      <Button label="Next merge history page" size="sm" disabled={!controller.hasNextHistoryPage || controller.historyLoading} onclick={() => void controller.nextHistoryPage()} />
    </div>{/if}
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

      <!-- Keyboard focus lets a user scroll a table wider than its pane without a pointer. -->
      <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
      <div class="table-scroll" role="region" aria-label="Merge participants table" tabindex="0">
        <Table unsorted bare ariaLabel="Merge participants"><thead><tr><th scope="col">Participant</th><th scope="col">Origin</th><th scope="col">Disposition</th></tr></thead>
          <tbody>{#each controller.detail.participants ?? [] as participant}<tr><th scope="row">{participant.participant_id}</th><td>{participant.origin_side}</td><td>{disposition(participant.split_id)}</td></tr>{/each}</tbody>
        </Table>
      </div>
      <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
      <div class="table-scroll" role="region" aria-label="Merge row dispositions table" tabindex="0">
        <Table unsorted bare ariaLabel="Merge row dispositions"><thead><tr><th scope="col">Table</th><th scope="col">Action</th><th scope="col">Origin</th><th scope="col">Provenance</th><th scope="col">Participant</th><th scope="col">Disposition</th></tr></thead>
          <tbody>{#each controller.detail.rows ?? [] as row}<tr><th scope="row">{row.table_name}</th><td>{row.action}</td><td>{row.origin_side}</td><td>{row.provenance_kind}</td><td>{row.participant_id ?? 'None'}</td><td>{disposition(row.split_id)}</td></tr>{/each}</tbody>
        </Table>
      </div>
      <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
      <div class="table-scroll" role="region" aria-label="Prior splits table" tabindex="0">
        <Table unsorted bare ariaLabel="Prior splits"><thead><tr><th scope="col">Split</th><th scope="col">Source</th><th scope="col">Created person</th><th scope="col">Revision change</th><th scope="col">Restoration</th><th scope="col">Actor</th><th scope="col">Created</th></tr></thead>
          <tbody>{#each controller.detail.splits ?? [] as split}<tr><th scope="row">{split.id}</th><td>Person {split.source_person_id}</td><td>Person {split.new_person_id}</td><td>{split.source_revision_before} → {split.source_revision_after}</td><td>{split.exact_reversal ? 'Exact' : 'Partial'}</td><td>{split.actor}</td><td>{split.created_at}</td></tr>{/each}</tbody>
        </Table>
      </div>
      <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
      <div class="table-scroll" role="region" aria-label="Merge review candidates table" tabindex="0">
        <Table unsorted bare ariaLabel="Merge review candidates"><thead><tr><th scope="col">Candidate</th><th scope="col">Person</th><th scope="col">Definition</th><th scope="col">Survivor value</th><th scope="col">Absorbed value</th><th scope="col">Resolution</th><th scope="col">State</th><th scope="col">Reviewed</th><th scope="col">Reviewer</th><th scope="col">Created</th></tr></thead>
          <tbody>{#each controller.detail.review_candidates ?? [] as candidate}<tr><th scope="row">{candidate.id}</th><td>{candidate.person_id}</td><td>{candidate.definition_id}</td><td>{candidate.survivor_value_id}</td><td>{candidate.absorbed_value_id}</td><td>{candidate.resolution_value_id ?? 'None'}</td><td>{candidate.state}</td><td>{candidate.reviewed_at ?? 'Not reviewed'}</td><td>{candidate.reviewed_by ?? 'None'}</td><td>{candidate.created_at}</td></tr>{/each}</tbody>
        </Table>
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
  .merge-history { gap: var(--space-2); }
  .merges { display: grid; padding: 0; list-style: none; }
  .merges li { display: flex; align-items: center; justify-content: space-between; gap: var(--field-gap); padding: var(--space-2) 0; border-bottom: 1px solid var(--border-muted); }
  .merges span { display: grid; gap: var(--space-1); }
  .merges small, .muted { color: var(--text-muted); font-size: var(--font-size-sm); }
  .section-heading, .pagination { display: flex; justify-content: space-between; align-items: center; gap: var(--space-3); flex-wrap: wrap; }
  h3, h4, p, pre { margin: 0; }
  .section-heading p, .pagination { color: var(--text-muted); font-size: var(--font-size-sm); }
  .table-scroll, .snapshot { max-width: 100%; overflow: auto; }
  .table-scroll :global(table) { width: 100%; border-collapse: collapse; font-size: var(--font-size-sm); }
  .table-scroll :global(th),
  .table-scroll :global(td) { padding: var(--space-2); border-bottom: var(--border-width) solid var(--border-default); text-align: left; vertical-align: top; }
  .table-scroll :global(thead th) { color: var(--text-muted); white-space: nowrap; }
  .snapshot { max-height: 22rem; padding: var(--space-3); border: var(--border-width) solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-inset); }
  .table-scroll:focus-visible, .snapshot:focus-visible { outline: var(--focus-ring); outline-offset: var(--focus-ring-offset, 2px); }
  .snapshot pre { white-space: pre-wrap; overflow-wrap: anywhere; }
  .message { display: flex; gap: var(--space-2); align-items: center; }
  .error { color: var(--text-danger); }
</style>
