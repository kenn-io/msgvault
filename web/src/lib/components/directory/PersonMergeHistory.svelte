<script lang="ts">
  import { formatDateTime as historyTime } from '../../util/format';
  import { Button, Spinner } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import {
    PersonMergeHistoryController,
    type PersonSplitCommittedContext
  } from '../../directory/person-merge-history-controller.svelte';
  import HistorySection from './HistorySection.svelte';
  import { historySource } from './history-format';
  import { reviewStateChip } from '../../directory/labels';
  import PersonSplitModal from './PersonSplitModal.svelte';

  interface Props {
    client: APIClient;
    personID: number;
    definitions?: { id: number; label: string }[];
    onOpenPerson?: (personID: number) => void;
    onSplitCommitted?: (context: PersonSplitCommittedContext) => void | Promise<void>;
  }

  let {
    client,
    personID,
    definitions = [],
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

  const labels = $derived(controller.labels?.for === controller.detail ? controller.labels : null);
  const survivor = $derived(controller.detail ? labels?.names.get(controller.detail.merge.survivor_person_id) : undefined);
  function addressRows(side: string) {
    return (controller.detail?.participants ?? []).filter(participant => participant.origin_side === side).flatMap(participant => {
      const addresses = labels?.addresses.get(participant.participant_id) ?? [];
      return (addresses.length ? addresses : [{ title: `Participant ${participant.participant_id}`, detail: '' }]).map((address, index) => ({
        id: `${participant.participant_id}-${index}`, value: address.title, detail: address.detail, meta: splitMeta(participant.split_id)
      }));
    });
  }
  function splitMeta(splitID: number | undefined) {
    if (splitID === undefined) return '';
    const split = controller.detail?.splits?.find(split => split.id === splitID);
    return split ? `Split out to ${labels?.names.get(split.new_person_id) ?? 'a profile'}` : 'Split out';
  }
  function reviewRows(pending: boolean) {
    return (controller.detail?.review_candidates ?? []).filter(candidate => (candidate.state === 'pending') === pending).map(candidate => ({
      id: candidate.id, value: definitions.find(definition => definition.id === candidate.definition_id)?.label ?? 'Field',
      meta: pending ? 'Waiting for review' : [reviewStateChip(candidate.state).label, candidate.reviewed_by ? historySource(candidate.reviewed_by) : '', candidate.reviewed_at ? historyTime(candidate.reviewed_at) : ''].filter(Boolean).join(' · ')
    }));
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
            <span>{item.merge.survivor_person_id === personID ? 'Another profile merged into this profile' : 'Two profiles merged'}
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
    <section class="merge-detail" aria-label="Merge detail">
      <div class="section-heading">
        {#if labels}<div><h4>{survivor ? `Another profile merged into ${survivor}` : 'Two profiles merged'}</h4><p>{historySource(controller.detail.merge.actor)} · {historyTime(controller.detail.merge.created_at)}</p></div>
        {:else}<p role="status"><Spinner size={12} label="Loading merge detail" /> Loading merge detail…</p>{/if}
        {#if controller.canOfferSplit}<Button label="Split merged profile" tone="info" onclick={openSplit} />
        {:else if !controller.detail.merge.current_person_id}<p>No current source profile is recorded, so this merge cannot be split.</p>{/if}
      </div>
      {#if controller.detailError}<p class="error" role="alert">{controller.detailError}</p>{/if}

      {#if labels}
        {#if labels.incomplete}<div class="message"><p class="muted">Some names and addresses couldn't be loaded.</p><Button label="Retry" size="sm" onclick={() => void controller.retryDetail()} /></div>{/if}
        {#each ['absorbed', 'survivor'] as side}
          {@const rows = addressRows(side)}
          {#if rows.length}<HistorySection level={5} title={side === 'absorbed' ? 'Message addresses from the merged-in profile' : `Message addresses from ${survivor ?? 'the surviving profile'}`} {rows} />{/if}
        {/each}
        {#if !controller.detail.participants?.length}<p class="muted">No message addresses were linked to this merge.</p>{/if}
        {#if controller.detail.splits?.length}
          <HistorySection level={5} title="Splits" rows={controller.detail.splits.map(split => ({ id: split.id, value: labels?.names.get(split.new_person_id) ?? 'A profile', detail: split.exact_reversal ? 'Exact restore' : 'Partial restore', meta: `${historySource(split.actor)} · ${historyTime(split.created_at)}` }))} />
        {/if}
        {#if controller.detail.review_candidates?.length}<HistorySection level={5} title={controller.detail.review_candidates.some(candidate => candidate.state === 'pending') ? 'Needs review' : 'Review decisions'} rows={reviewRows(true)} earlier={reviewRows(false)} earlierNoun="decision" />{/if}
      {/if}

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
  .merge-detail > .section-heading { margin-bottom: calc(var(--space-6) - var(--space-3)); }
  .merges { display: grid; padding: 0; list-style: none; }
  .merges li { display: flex; align-items: center; justify-content: space-between; gap: var(--field-gap); padding: var(--space-2) 0; border-bottom: 1px solid var(--border-muted); }
  .merges span { display: grid; gap: var(--space-1); }
  .merges small, .muted { color: var(--text-muted); font-size: var(--font-size-sm); }
  .section-heading, .pagination { display: flex; justify-content: space-between; align-items: center; gap: var(--space-3); flex-wrap: wrap; }
  h3, h4, p, pre { margin: 0; }
  .section-heading p, .pagination { color: var(--text-muted); font-size: var(--font-size-sm); }
  .snapshot { max-width: 100%; overflow: auto; }
  .snapshot { max-height: 22rem; padding: var(--space-3); border: var(--border-width) solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-inset); }
  .snapshot:focus-visible { outline: var(--focus-ring); outline-offset: var(--focus-ring-offset, 2px); }
  .snapshot pre { white-space: pre-wrap; overflow-wrap: anywhere; }
  .message { display: flex; gap: var(--space-2); align-items: center; }
  .error { color: var(--text-danger); }
</style>
