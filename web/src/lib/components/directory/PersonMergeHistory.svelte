<script lang="ts">
  import { formatDateTime, formatShortDate } from '../../util/format';
  import { Button, Disclosure, Spinner } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, untrack, type Snippet } from 'svelte';

  import { addressText, absorbedCardTitle, mergeAddresses, mergeSentence, type MergedInAddress } from '../../directory/merge-labels';
  import type { APIClient } from '../../api/client';
  import {
    PersonMergeHistoryController,
    type PersonSplitCommittedContext
  } from '../../directory/person-merge-history-controller.svelte';
  import HistorySection from './HistorySection.svelte';
  import { actorName, provenance } from './history-format';
  import MergeProfileCards from './MergeProfileCards.svelte';
  import { reviewStateChip } from '../../directory/labels';
  import PersonSplitSteps from './PersonSplitSteps.svelte';
  import Dialog from '../common/Dialog.svelte';
  import InlineNotice from '../common/InlineNotice.svelte';
  import EmptyLine from '../common/EmptyLine.svelte';
  import Pager from '../common/Pager.svelte';
  import DetailRow from '../people/DetailRow.svelte';

  interface Props {
    client: APIClient;
    personID: number;
    /** Field history, under the merges, with what this page's merges brought in. */
    fields?: Snippet<[(mergeID: number) => void, MergedInAddress[]]>;
    onClose?: () => void;
    definitions?: { id: number; label: string; is_sensitive?: boolean }[];
    onOpenPerson?: (personID: number) => void;
    onSplitCommitted?: (context: PersonSplitCommittedContext) => void | Promise<void>;
  }

  let {
    client, personID, fields, onClose = () => undefined,
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
  let snapshotOpen = $state(false);

  $effect(() => {
    if (personID !== activePersonID) {
      activePersonID = personID;
      controller.setPerson(personID);
    }
  });

  onMount(() => void controller.loadHistory());
  onDestroy(() => controller.destroy());

  function openMerge(mergeID: number): void {
    snapshotOpen = false;
    void controller.selectMerge(mergeID);
  }
  function back(): void {
    if (controller.splitOpen) controller.closeSplit();
    else {
      controller.selectedMergeID = null;
      controller.detail = null;
      snapshotOpen = false;
    }
  }

  const labels = $derived(controller.labels?.for === controller.detail ? controller.labels : null);
  const absorbedAddresses = $derived(controller.detail ? mergeAddresses(controller.detail, 'absorbed') : []);
  const absorbedAddress = $derived(addressText(absorbedAddresses[0]));
  function mergeTitle(merge: { id: number; absorbed_person_id: number; survivor_person_id: number }): string {
    if (!controller.historyRows.has(merge.id)) return 'Loading merge…';
    const address = controller.detail?.merge.id === merge.id && labels ? absorbedAddress : controller.historyRows.get(merge.id)?.absorbedAddress;
    const names = controller.historyRows.get(merge.id)?.names;
    return mergeSentence(names?.get(merge.absorbed_person_id), names?.get(merge.survivor_person_id), address);
  }
  /** "Nov 6 · 1 address moved in · split back once": what the merge brought, not both sides' total. */
  function mergeMeta(item: { merge: { id: number; created_at: string }; split_count: number; pending_candidate_count: number }) {
    const row = controller.historyRows.get(item.merge.id);
    const count = row?.incomplete ? undefined : row?.count;
    return {
      text: [formatShortDate(item.merge.created_at), count !== undefined ? `${count} ${count === 1 ? 'address' : 'addresses'} moved in` : '',
        item.split_count ? `split back ${item.split_count === 1 ? 'once' : `${item.split_count} times`}` : '',
        item.pending_candidate_count ? `${item.pending_candidate_count} waiting for review` : ''].filter(Boolean).join(' · '),
      title: `Merged ${formatDateTime(item.merge.created_at)}`
    };
  }

  // The merge step: the absorbed card is named as the title names it, and a card headed by its address doesn't list it again.
  const absorbedNames = $derived(controller.detail ? [labels?.names.get(controller.detail.merge.absorbed_person_id), labels?.names.get(controller.detail.merge.survivor_person_id)] as const : [undefined, undefined] as const);
  const absorbedTitle = $derived(controller.detail ? absorbedCardTitle(absorbedNames[0], absorbedNames[1], absorbedAddress) : '');
  const survivorAddresses = $derived(controller.detail ? mergeAddresses(controller.detail, 'survivor') : []);
  const onCards = (split: { id: number }) => [...absorbedAddresses, ...survivorAddresses].some(address => address.splitID === split.id);
  const tookAll = (split: { id: number }) => controller.absorbedParticipants.every(participant => participant.split_id === split.id);
  /** Splits the cards can't show: one that took no address listed on a card. */
  const otherSplits = $derived((controller.detail?.splits ?? []).filter(split => !onCards(split)));
  /**
   * Each split of the cards' addresses, after the merge line: "split back to Neri Bloom", "on Nov 7" only on another day.
   * Unless a lone split took exactly the merged-in addresses, it names the ones it took, as "split back to Neri Bloom (neri@example.org)".
   */
  const cardSplits = $derived.by(() => {
    const detail = controller.detail;
    if (!detail) return [];
    const splits = (detail.splits ?? []).filter(onCards);
    const moved = (split: { id: number }) => [...absorbedAddresses, ...survivorAddresses].filter(address => address.splitID === split.id);
    return splits.map(split => ({
      id: split.id, personID: split.new_person_id, name: labels?.names.get(split.new_person_id),
      addresses: splits.length > 1 || !tookAll(split) ? moved(split).map(addressText).join(', ') : '',
      by: split.actor === detail.merge.actor ? '' : ` by ${actorName(split.actor)}`,
      rest: `${formatShortDate(split.created_at) === formatShortDate(detail.merge.created_at) ? '' : ` on ${formatShortDate(split.created_at)}`}${!split.exact_reversal && tookAll(split) ? ' · partly restored' : ''}`,
      title: `Split ${formatDateTime(split.created_at)}`,
    }));
  });
  /** Why Split… is absent, as the merge line's last segment. */
  const splitUnavailable = $derived.by(() => {
    const detail = controller.detail;
    if (!detail || controller.canOfferSplit) return '';
    const absorbed = (detail.participants ?? []).filter(participant => participant.origin_side === 'absorbed');
    if (detail.splits?.length && absorbed.every(participant => participant.split_id !== undefined)) return 'nothing left to split';
    return detail.merge.current_person_id ? 'nothing left to split' : 'the profile it merged into is gone';
  });
  function splitLine(split: { id: number; exact_reversal: boolean; actor: string; created_at: string }) {
    const line = provenance(split.actor, split.created_at);
    return { text: !split.exact_reversal && tookAll(split) ? `${line.text} · partly restored` : line.text, title: line.title };
  }
  function reviewRows(pending: boolean) {
    return (controller.detail?.review_candidates ?? []).filter(candidate => (candidate.state === 'pending') === pending).map(candidate => ({
      id: candidate.id, value: definitions.find(definition => definition.id === candidate.definition_id)?.label ?? 'Field',
      provenance: !pending && candidate.reviewed_by && candidate.reviewed_at ? provenance(candidate.reviewed_by, candidate.reviewed_at, undefined, { action: reviewStateChip(candidate.state).label }) : undefined,
    }));
  }
  const title = $derived(controller.committedResult ? 'Split complete'
    : controller.splitOpen ? `Split ${controller.sourcePerson?.display_name?.trim() || 'profile'}`
    : controller.selectedMergeID !== null ? (controller.detail ? mergeTitle(controller.detail.merge) : 'Merge')
    : 'History');
  const splitDisabled = $derived(controller.splitNeedsFreshState || !controller.sourcePerson ||
    controller.selectedParticipantIDs.some(id => !controller.canSelectParticipant(id)) ||
    (!controller.isZeroParticipantLineage && controller.selectedParticipantIDs.length === 0));

  /** When the merge recorded the profiles: "Nov 6, 9:37 AM", with the year only when it isn't this year. */
  function capturedAt(at: string): string {
    const date = new Date(at);
    if (Number.isNaN(date.valueOf())) return at;
    return `${formatShortDate(at)}, ${new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' }).format(date)}`;
  }
  function snapshotText(value: unknown): string {
    const encoded = JSON.stringify(value, null, 2);
    return encoded === undefined ? String(value) : encoded;
  }
</script>

<Dialog {title} width={controller.splitOpen || controller.committedResult ? 'medium' : 'large'} onclose={onClose}
  onback={controller.selectedMergeID !== null && !controller.committedResult ? back : undefined}
  backLabel={controller.splitOpen ? `Back to ${controller.detail ? mergeTitle(controller.detail.merge) : 'Merge'}` : 'Back to History'}
  busy={controller.splitPending}
  primaryLabel={controller.committedResult ? 'Done' : controller.splitOpen ? 'Split into new person' : undefined}
  primaryTone={controller.committedResult ? 'info' : 'danger'} cancellable={false}
  primaryDisabled={!controller.committedResult && splitDisabled}
  onprimary={() => { if (controller.committedResult) onClose(); else if (controller.confirmSplit()) void controller.submitSplit(); }}>
  {#if controller.splitOpen}
    <PersonSplitSteps {controller} {onOpenPerson} />
  {:else if controller.selectedMergeID === null}
    <div class="history-step">
      <!-- Merges lead: they change who a profile is, so they sit above field history. -->
      <section class="merges" aria-labelledby="merges-heading">
        <h3 id="merges-heading">Merges</h3>
        {#if controller.historyError}<InlineNotice message="Couldn't load merge history." actionLabel="Retry" onaction={() => void controller.retryHistory()} details={controller.historyError} />{/if}
        {#if controller.historyLoading && !controller.history.length}<p role="status">Loading merges…</p>{/if}
        {#each controller.history as item (item.merge.id)}
          <DetailRow actionLabel={`Open ${mergeTitle(item.merge)} · ${formatShortDate(item.merge.created_at)}`} onclick={() => openMerge(item.merge.id)} description={mergeMeta(item)}>
            {mergeTitle(item.merge)}
          </DetailRow>
        {/each}
        {#if !controller.historyLoading && !controller.historyError && !controller.history.length}<EmptyLine message="No merges yet" />{/if}
        <Pager offset={controller.historyOffset} count={controller.history.length} hasNext={controller.hasNextHistoryPage}
          disabled={controller.historyLoading} onprevious={() => void controller.previousHistoryPage()} onnext={() => void controller.nextHistoryPage()} label="Merge history pages" />
      </section>
      {@render fields?.(openMerge, controller.mergedIn)}
    </div>
  {:else if controller.detailLoading}
    <p role="status"><Spinner size={12} label="Loading merge" /> Loading merge…</p>
  {:else if !controller.detail}
    <InlineNotice message="Couldn't load this merge." actionLabel="Retry" onaction={() => void controller.retryDetail()} details={controller.detailError ?? undefined} />
  {:else}
    {@const detail = controller.detail}
    {@const merged = provenance(detail.merge.actor, detail.merge.created_at, undefined, { action: 'Merged' })}
    <section class="merge-step" aria-label="Merge detail">
      {#if controller.detailError}<InlineNotice message="Couldn't load this merge." actionLabel="Retry" onaction={() => void controller.retryDetail()} details={controller.detailError} />{/if}
      {#if labels?.incomplete}<InlineNotice tone="warning" message="Some names and addresses couldn't be loaded." actionLabel="Retry" onaction={() => void controller.retryDetail()} />{/if}
      <!-- Who was merged into whom, with what each brought, then when. -->
      <div class="merge-summary">
        {#if !labels}<p role="status"><Spinner size={12} label="Loading merge" /> Loading merge…</p>
        {:else}
        <MergeProfileCards label="Merged profiles"
          from={{ title: absorbedTitle, addresses: absorbedAddresses }}
          to={{ title: labels?.names.get(detail.merge.survivor_person_id) ?? 'This profile', addresses: survivorAddresses }} />
        {/if}
        <div class="merge-line">
          <p><span title={merged.title}>{merged.text}</span>{#if labels}{#each cardSplits as split (split.id)}{' · '}<span title={split.title}>split back{split.by} to {#if split.name !== undefined}<button class="link" type="button" onclick={() => onOpenPerson(split.personID)}>{split.name || 'another profile'}</button>{:else}another profile{/if}{#if split.addresses}{` (${split.addresses})`}{/if}{split.rest}</span>{/each}{/if}{#if splitUnavailable}{` · ${splitUnavailable}`}{/if}</p>
          {#if controller.canOfferSplit}<Button label="Split…" tone="info" onclick={() => void controller.openSplit()} />{/if}
        </div>
      </div>
      {#if labels && otherSplits.length}
        <section aria-labelledby="splits-heading">
          <h3 id="splits-heading">Splits</h3>
          {#each otherSplits as split (split.id)}{@const name = labels.names.get(split.new_person_id)}<DetailRow actionLabel={`Open ${name || 'restored profile'}`} onclick={name === undefined ? undefined : () => onOpenPerson(split.new_person_id)} description={splitLine(split)}>{name || 'A profile'}</DetailRow>{/each}
        </section>
      {/if}
      {#if detail.review_candidates?.length}<HistorySection title={detail.review_candidates.some(candidate => candidate.state === 'pending') ? 'Needs review' : 'Review decisions'} rows={reviewRows(true)} earlier={reviewRows(false)} earlierNoun="decision" />{/if}
      <!-- The merge as stored, last: when, its digest, and the record itself. -->
      <Disclosure variant="plain" class="fold" title="Profile at merge time" bind:open={snapshotOpen} ontoggle={(open) => { if (open) void controller.revealSnapshot(); }}>
        {#if controller.snapshot}
          <div class="snapshot-meta">
            <p title={`Captured ${formatDateTime(detail.merge.created_at, 'long')}`}>Captured {capturedAt(detail.merge.created_at)}</p>
            <p title={`SHA-256 ${controller.snapshot.sha256}`}>SHA-256 {controller.snapshot.sha256.slice(0, 8)}…</p>
          </div>
          <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
          <div class="snapshot" role="region" aria-label="Raw values at merge time" tabindex="0"><pre>{snapshotText(controller.snapshot.snapshot)}</pre></div>
        {:else if controller.snapshotLoading}<p role="status">Loading…</p>{/if}
        {#if controller.snapshotError}<InlineNotice message="Couldn't load the profile at merge time." actionLabel="Retry" onaction={() => void controller.revealSnapshot()} details={controller.snapshotError} />{/if}
      </Disclosure>
    </section>
  {/if}
</Dialog>

<style>
  /* History's step and the merge step: groups 24 apart, each heading 8 above its rows. The merge's line sits 12 under its cards, as the cards sit 12 apart. */
  .history-step, .merge-step { display: grid; gap: var(--space-section); min-width: 0; }
  .merges, section { display: grid; min-width: 0; }
  .merge-summary { display: grid; gap: var(--space-box); min-width: 0; }
  .merge-line { display: flex; justify-content: space-between; align-items: center; gap: var(--space-box); flex-wrap: wrap; }
  h3, p, pre { margin: 0; }
  h3 { margin-bottom: var(--space-group); font-size: var(--type-heading-size); line-height: var(--type-heading-line); }
  p, .snapshot-meta { color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  .merges > :global(.inline-notice) { margin-bottom: var(--space-group); }
  .snapshot-meta { display: grid; margin-bottom: var(--space-group); }
  .link { padding: 0; border: 0; background: none; color: var(--link-ink); font: inherit; cursor: pointer; }
  .link:hover { text-decoration: underline; }
  .link:focus-visible { outline: var(--focus-ring); outline-offset: var(--focus-ring-offset, 2px); }
  /* The raw record scrolls in its own 22rem pane: 12 inside, its right side 8 less to keep the bar's gutter. */
  .snapshot { max-width: 100%; max-height: 22rem; overflow: auto; scrollbar-gutter: stable; padding: var(--space-box) calc(var(--space-box) - var(--scrollbar-gutter-size)) var(--space-box) var(--space-box); border: var(--border-width) solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-inset); }
  .snapshot:focus-visible { outline: var(--focus-ring); outline-offset: var(--focus-ring-offset, 2px); }
  .snapshot pre { white-space: pre-wrap; overflow-wrap: anywhere; font-size: var(--type-small-size); line-height: var(--type-small-line); }
</style>
