<script lang="ts">
  import { Checkbox, Spinner } from '@kenn-io/kit-ui';
  import { tick } from 'svelte';

  import DetailRow from '../people/DetailRow.svelte';
  import InlineNotice from '../common/InlineNotice.svelte';
  import MergeProfileCards from './MergeProfileCards.svelte';
  import { addressText, distinctLabels, savedAddresses, workspaceLabels } from '../../directory/merge-labels';
  import type { PersonContactPoint } from '../../api/generated/models';
  import type { PersonMergeHistoryController } from '../../directory/person-merge-history-controller.svelte';

  interface Props {
    controller: PersonMergeHistoryController;
    onOpenPerson: (personID: number) => void;
  }

  let { controller, onOpenPerson }: Props = $props();
  const uid = $props.id();
  let splitContent = $state<HTMLDivElement>();
  let retryAction = $state<HTMLSpanElement>();

  const sourceName = $derived(controller.sourcePerson?.display_name?.trim() || 'this profile');
  const labels = $derived(controller.labels?.for === controller.detail ? controller.labels : null);
  const workspaces = $derived(workspaceLabels([...(controller.sourceSaved ?? []), ...(controller.receiptSaved.source ?? []), ...(controller.receiptSaved.created ?? [])], labels?.workspaceLabels));
  /** An address as the person knows it, with its scope when it has one, so two alike addresses read apart. */
  function addressLabel(id: number): string {
    const addresses = labels?.addresses.get(id);
    const ids = [...controller.eligibleParticipantIDs, ...(controller.sourcePerson?.participant_ids ?? []).filter(id => !controller.eligibleParticipantIDs.includes(id))];
    return addresses ? addresses.map(address => addressText(address)).join(', ') || 'No addresses' : `Unknown address ${ids.indexOf(id) + 1}`;
  }
  /** Addresses still on the current profile, including those linked after this merge. */
  function remaining(moved: number[]): number[] {
    return (controller.sourcePerson?.participant_ids ?? []).filter(id => !moved.includes(id));
  }
  /**
   * The receipt line for one profile: its addresses, then what it saves, as read after the split. A linked
   * address the profile also saves reads once, under Saved; the same value on another service or scope stays.
   */
  function receiptLine(lead: string[], ids: number[], saved: PersonContactPoint[] | null): string | undefined {
    const savedKeys = new Set<string>();
    const lines = savedAddresses(saved ?? [], savedKeys, workspaces).map(address => addressText(address)!);
    const linked = ids.flatMap(id => {
      const addresses = labels?.addresses.get(id) ?? [];
      const unsaved = addresses.filter(address => !savedKeys.has(address.identity_key));
      if (unsaved.length === addresses.length) return [participantLabel(id)];
      return unsaved.length ? [unsaved.map(address => addressText(address)).join(', ')] : [];
    });
    return [...lead, ...linked, ...(lines.length ? [`Saved: ${lines.join(', ')}`] : [])].join(' · ') || undefined;
  }
  function card(title: string, ids: number[], empty: string) {
    return { title, empty, addresses: ids.map(id => ({ id: String(id), value: participantLabel(id) })) };
  }
  function count(rows: unknown[] | undefined, verb: string): string {
    const n = rows?.length ?? 0;
    return n ? `${n} ${n === 1 ? 'record' : 'records'} couldn't be ${verb}` : '';
  }
  const committed = $derived(controller.committedResult?.result ?? null);
  /** Labels over every address the profile held before the split, so a choice, its preview, and the receipt read alike. */
  const participantLabels = $derived(distinctLabels([...controller.sourcePerson?.participant_ids ?? [],
    ...committed?.source_person.participant_ids ?? [], ...committed?.new_person.participant_ids ?? []], addressLabel));
  function participantLabel(id: number): string {
    return participantLabels.get(id) ?? addressLabel(id);
  }
  /** Saved addresses by canonical service, scope, and value: a choice that matches one reads as also saved, and the rest list on their own. */
  const sourceSaved = $derived(savedAddresses(controller.sourceSaved ?? [], new Set(), workspaces));
  const savedKeys = $derived(new Set(sourceSaved.map(address => address.identity_key)));
  const choiceKeys = $derived(new Set(controller.eligibleParticipantIDs.flatMap(id => labels?.addresses.get(id) ?? []).map(address => address.identity_key)));
  const savedOnly = $derived(sourceSaved.filter(address => !choiceKeys.has(address.identity_key)).map(address => addressText(address)!));
  function alsoSaved(id: number): boolean {
    return !!labels?.addresses.get(id)?.some(address => savedKeys.has(address.identity_key));
  }
  const partial = $derived(!!committed && ((!committed.exact_reversal && controller.absorbedParticipants.every(participant => controller.selectedParticipantIDs.includes(participant.participant_id))) ||
    (committed.ambiguous_rows?.length ?? 0) > 0 || (committed.unrestored_rows?.length ?? 0) > 0));

  async function retryStaleState(): Promise<void> {
    const loaded = await controller.retryStaleSplitState();
    await tick();
    if (loaded) {
      const target = splitContent?.querySelector<HTMLElement>('input:not(:disabled)');
      if (target?.isConnected) target.focus();
    } else {
      const target = retryAction?.querySelector<HTMLElement>('button');
      if (target?.isConnected) target.focus();
    }
  }
</script>

  <div class="split" bind:this={splitContent} aria-busy={controller.splitBusy}>
    {#if committed}
      {@const remainingName = committed.source_person.display_name?.trim() || 'Remaining profile'}
      <!-- A receipt: the two people that now exist, each with its addresses. -->
      <section class="result">
        <!-- Exact means every record the merge moved was matched. A later edit stays on its record wherever that is now, and a deleted one stays deleted. -->
        {#if committed.exact_reversal && !partial}<p>The merge was fully reversed, and edits and deletions made since then were kept.</p>{/if}
        <div aria-label="Split result people">
          <DetailRow actionLabel={`Open ${remainingName}`} onclick={() => onOpenPerson(committed.source_person.id)} description={receiptLine(['Stays'], committed.source_person.participant_ids, controller.receiptSaved.source)}>{remainingName}</DetailRow>
          <DetailRow actionLabel={`Open ${committed.new_person.display_name?.trim() || 'new person'}`} onclick={() => onOpenPerson(committed.new_person.id)} description={receiptLine(committed.new_person.display_name?.trim() ? ['New person'] : [], committed.new_person.participant_ids, controller.receiptSaved.created)}>{committed.new_person.display_name?.trim() || 'New person'}</DetailRow>
        </div>
        {#if controller.receiptSavedLoading}
          <p class="meta"><Spinner size={12} label="Loading saved addresses" /> Loading saved addresses…</p>
        {:else if controller.receiptSavedError}
          <!-- The split is done; Retry only reads the two profiles again. -->
          <InlineNotice tone="warning" message="The split is done, but saved addresses couldn't be loaded." actionLabel="Retry" onaction={() => void controller.retryReceiptSaved()} details={controller.receiptSavedError} />
        {/if}
        {#if partial || !committed.exact_reversal}
          <ul class="result-details">
            {#if partial}<li>Partly restored</li>{/if}
            {#if count(committed.ambiguous_rows, 'matched')}<li>{count(committed.ambiguous_rows, 'matched')}</li>{/if}
            {#if count(committed.unrestored_rows, 'restored')}<li>{count(committed.unrestored_rows, 'restored')}</li>{/if}
            <li>{committed.uid_alias_disposition === 'retired_uid_alias_retargeted' ? 'Links to the merged-in profile now open the new person' : `Links to the merged-in profile still open ${remainingName}`}</li>
          </ul>
        {/if}
        {#if !controller.committedResult?.receiptETags.source || !controller.committedResult?.receiptETags.created}<InlineNotice tone="info" message="Profile actions will load fresh data." />{/if}
        {#if controller.reconciliationError}<InlineNotice message="Couldn't refresh the profiles." actionLabel="Retry" onaction={() => void controller.retryReconciliation()} details={controller.reconciliationError} />{/if}
      </section>
    {:else if controller.sourceLoading}
      <p><Spinner size={12} label="Loading split source" /> Loading this profile…</p>
    {:else if controller.sourceError}
      <InlineNotice message="Couldn't load this profile." actionLabel="Retry" onaction={() => void controller.retryDetail()} details={controller.sourceError} />
    {:else if controller.sourcePerson}
      {#if labels?.incomplete}<InlineNotice tone="warning" message="Some addresses couldn't be loaded." actionLabel="Retry" onaction={() => void controller.retryDetail()} />{/if}
      {#if controller.splitNeedsFreshState}
        <span bind:this={retryAction}><InlineNotice message="This profile changed since you opened it." actionLabel={controller.splitBusy ? 'Reloading…' : 'Retry'} onaction={() => { if (!controller.splitBusy) void retryStaleState(); }} details={controller.splitError ?? undefined} /></span>
      {:else if controller.isZeroParticipantLineage}
        <p>Splitting restores its profile history.</p>
        {@render savedNotice()}
      {:else if controller.eligibleParticipantIDs.length === 0}
        <p role="status">No addresses in this merge can be split out.</p>
      {:else}
        <!-- The cards below name what moves and what stays, so the boxes need no lead. -->
        <fieldset disabled={controller.splitBusy} aria-label="Addresses to move">
          {#each controller.eligibleParticipantIDs as participantID}
            {@const saved = alsoSaved(participantID)}
            <Checkbox
              class="choice"
              checked={controller.selectedParticipantIDs.includes(participantID)}
              label={participantLabel(participantID)}
              ariaLabel={saved ? participantLabel(participantID) : undefined}
              ariaDescribedby={saved ? `${uid}-saved-${participantID}` : undefined}
              disabled={controller.splitBusy || (!controller.canSelectParticipant(participantID) && !controller.selectedParticipantIDs.includes(participantID))}
              onchange={(checked) => controller.setParticipantSelected(participantID, checked)}
            >{#if saved}<small id={`${uid}-saved-${participantID}`}>Also saved</small>{/if}</Checkbox>
          {/each}
        </fieldset>
        <!-- What the split will leave, updated as boxes change. -->
        <MergeProfileCards label="After the split"
          from={card(`Stays with ${sourceName}`, remaining(controller.selectedParticipantIDs), 'No addresses')}
          to={card('New person', controller.selectedParticipantIDs, 'None chosen yet')} />
        {@render savedNotice()}
      {/if}

      {#if !controller.splitNeedsFreshState && controller.splitError}<InlineNotice message="Couldn't complete this split." details={controller.splitError} />{/if}
    {/if}
  </div>

<!-- Which saved addresses move is the store's decision, so the preview lists what is saved now and the result says where each went. -->
{#snippet savedNotice()}
  {#if controller.sourceSavedError}
    <InlineNotice tone="warning" message="Couldn't load this profile's saved addresses." actionLabel="Retry" onaction={() => void controller.retrySourceSaved()} details={controller.sourceSavedError} />
  {:else if !controller.sourceSaved}
    <p class="meta"><Spinner size={12} label="Loading saved addresses" /> Loading saved addresses…</p>
  {:else if sourceSaved.length}
    <section class="saved" aria-label={`Saved on ${sourceName}`}>
      <p>Saved on {sourceName} now. The result shows where each one ends up.</p>
      {#if savedOnly.length}<ul>{#each savedOnly as line, index (index)}<li>{line}</li>{/each}</ul>{/if}
    </section>
  {/if}
{/snippet}

<style>
  /* The address list is checkbox rows in a box padded 12, 8 apart; the cards follow 12 below it. */
  .split, .result { display: grid; gap: var(--space-box); min-width: 0; }
  .result { gap: var(--space-pane); }
  p { margin: 0; font-size: var(--type-body-size); line-height: var(--type-body-line); }
  fieldset { display: grid; gap: var(--space-group); min-width: 0; margin: 0; padding: var(--space-box); border: var(--border-width) solid var(--border-default); border-radius: var(--radius-sm); }
  .meta, .saved p, small { color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  /* A choice that is also saved puts that note on a line of its own, and the box stays centered on the address's first line. */
  fieldset :global(.choice) { align-items: flex-start; }
  fieldset :global(.choice .kit-checkbox__box) { margin-top: calc((var(--type-body-line) - 16px) / 2); }
  small { display: block; }
  /* The saved list: a Meta lead, then Body lines 4 apart, 8 under it. */
  .saved { display: grid; gap: var(--space-group); min-width: 0; }
  .saved ul { display: grid; gap: var(--space-tight); margin: 0; padding: 0; list-style: none; overflow-wrap: anywhere; font-size: var(--type-body-size); line-height: var(--type-body-line); }
  .result-details { display: grid; margin: 0; padding-left: var(--space-pane); color: var(--text-secondary); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
</style>
