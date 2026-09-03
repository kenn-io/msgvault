<script lang="ts">
  import { appShortcuts, Button, Checkbox, Modal, Spinner } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick } from 'svelte';

  import type { PersonMergeHistoryController } from '../../directory/person-merge-history-controller.svelte';

  interface Props {
    controller: PersonMergeHistoryController;
    onClose: () => void;
    onOpenPerson: (personID: number) => void;
  }

  let { controller, onClose, onOpenPerson }: Props = $props();
  let releaseShortcutScope: (() => void) | undefined;
  let splitContent = $state<HTMLDivElement>();
  let retryAction = $state<HTMLSpanElement>();

  const sourceName = $derived(controller.sourcePerson?.display_name?.trim() ||
    (controller.sourcePerson ? `Person ${controller.sourcePerson.id}` : 'current source profile'));
  const selectionLabel = $derived(controller.isZeroParticipantLineage
    ? 'the zero-participant lineage'
    : controller.selectedParticipantIDs.map((id) => `Participant ${id}`).join(', ') || 'the selected lineage');
  const confirmationLabel = $derived(`I confirm splitting ${selectionLabel} from ${sourceName}${controller.sourcePerson ? ` (Person ${controller.sourcePerson.id})` : ''}.`);
  const committed = $derived(controller.committedResult?.result ?? null);
  const partial = $derived(!!committed && (!committed.exact_reversal ||
    (committed.ambiguous_rows?.length ?? 0) > 0 || (committed.unrestored_rows?.length ?? 0) > 0));

  onMount(() => {
    releaseShortcutScope = appShortcuts.pushScope('person-split-modal');
  });

  onDestroy(() => releaseShortcutScope?.());

  function requestClose(): void {
    if (controller.splitBusy || committed) return;
    onClose();
  }

  function setConfirmed(value: boolean): void {
    if (value) controller.confirmSplit();
    else controller.clearSplitConfirmation();
  }

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

<Modal
  title="Split merged profile"
  ariaLabel="Split merged profile"
  closeLabel="Close split merged profile"
  closable={!controller.splitBusy && !committed}
  closeOnOverlayClick={!controller.splitBusy && !committed}
  onclose={requestClose}
  maxWidth="min(720px, calc(100vw - 32px))"
>
  <div class="split" bind:this={splitContent} aria-busy={controller.splitBusy}>
    {#if committed}
      <section class="result" aria-labelledby="split-completed-heading">
        <h3 id="split-completed-heading">Split completed</h3>
        {#if partial}
          <p><strong>Partial restoration.</strong> The server created a separate person, but some historical rows were ambiguous or could not be restored exactly.</p>
        {:else}
          <p>The server exactly restored the selected lineage into a separate person.</p>
        {/if}
        <div class="people" aria-label="Split result people">
          <article>
            <h4>{committed.source_person.display_name?.trim() || `Person ${committed.source_person.id}`}</h4>
            <p>Source person {committed.source_person.id}</p>
            <Button label={`Open source profile ${committed.source_person.display_name?.trim() || `Person ${committed.source_person.id}`} (Person ${committed.source_person.id})`} disabled={controller.splitPending} onclick={() => onOpenPerson(committed.source_person.id)} />
          </article>
          <article>
            <h4>{committed.new_person.display_name?.trim() || `Person ${committed.new_person.id}`}</h4>
            <p>Restored person {committed.new_person.id}</p>
            <Button label={`Open restored profile ${committed.new_person.display_name?.trim() || `Person ${committed.new_person.id}`} (Person ${committed.new_person.id})`} disabled={controller.splitPending} onclick={() => onOpenPerson(committed.new_person.id)} />
          </article>
        </div>
        {#if partial}
          <dl class="result-details">
            <div><dt>Ambiguous rows</dt><dd>{committed.ambiguous_rows?.length ?? 0}</dd></div>
            <div><dt>Unrestored rows</dt><dd>{committed.unrestored_rows?.length ?? 0}</dd></div>
            <div><dt>UID alias disposition</dt><dd>{committed.uid_alias_disposition}</dd></div>
          </dl>
        {/if}
        {#if !controller.committedResult?.receiptETags.source || !controller.committedResult?.receiptETags.created}
          <p class="notice" role="status">The operation receipt did not include both valid revision tags. Profile actions always load fresh server data.</p>
        {/if}
        {#if controller.reconciliationError}<p class="error" role="alert">{controller.reconciliationError}</p>{/if}
      </section>
    {:else if controller.sourceLoading && !controller.sourcePerson}
      <p><Spinner size={12} label="Loading split source" /> Loading the current source profile…</p>
    {:else if controller.sourceError}
      <div role="alert" class="error"><p>{controller.sourceError}</p><Button label="Retry source profile" onclick={() => void controller.openSplit()} /></div>
    {:else if controller.sourcePerson}
      <p>The merge currently belongs to <strong>{sourceName} (Person {controller.sourcePerson.id})</strong>. The server remains authoritative about participant eligibility when the request is submitted.</p>

      {#if controller.splitNeedsFreshState}
        <div class="stale" role="alert">
          <p>This split state is stale and cannot be submitted. Reload the exact source profile and merge detail before confirming again.</p>
          {#if controller.splitError}<p>{controller.splitError}</p>{/if}
          {#if controller.splitBusy}
            <p><Spinner size={12} label="Reloading split source and merge detail" /> Reloading source and merge detail…</p>
          {:else}
            <span bind:this={retryAction}><Button label="Retry source and merge detail" onclick={() => void retryStaleState()} /></span>
          {/if}
        </div>
      {:else if controller.isZeroParticipantLineage}
        <p>This merge recorded no absorbed-lineage participants. You may explicitly confirm the empty lineage to restore the absorbed profile.</p>
      {:else if controller.eligibleParticipantIDs.length === 0}
        <p role="status">No unsplit absorbed-lineage participants are currently eligible.</p>
      {:else}
        <fieldset disabled={controller.splitBusy}>
          <legend>Absorbed-lineage participants</legend>
          {#each controller.eligibleParticipantIDs as participantID}
            <Checkbox
              checked={controller.selectedParticipantIDs.includes(participantID)}
              label={`Participant ${participantID}`}
              disabled={controller.splitBusy}
              onchange={(checked) => controller.setParticipantSelected(participantID, checked)}
            />
          {/each}
        </fieldset>
      {/if}

      {#if !controller.splitNeedsFreshState && (controller.isZeroParticipantLineage || controller.selectedParticipantIDs.length > 0)}
        <Checkbox
          checked={controller.confirmedParticipantIDs !== null}
          label={confirmationLabel}
          disabled={controller.splitBusy}
          onchange={setConfirmed}
        />
      {/if}
      {#if !controller.splitNeedsFreshState && controller.splitError}<p class="error" role="alert">{controller.splitError}</p>{/if}
    {/if}
  </div>

  {#snippet footer()}
    {#if !committed}
      <Button surface="soft" label="Cancel" disabled={controller.splitBusy} onclick={requestClose} />
      <Button
        tone="info"
        surface="solid"
        label="Create restored person"
        disabled={controller.splitBusy || controller.splitNeedsFreshState || controller.confirmedParticipantIDs === null}
        onclick={() => void controller.submitSplit()}
      />
    {/if}
  {/snippet}
</Modal>

<style>
  .split, .result { display: grid; gap: var(--space-4); min-width: min(36rem, calc(100vw - 64px)); }
  p, h3, h4, dl, dd { margin: 0; }
  fieldset { display: grid; gap: var(--space-2); margin: 0; padding: var(--space-3); border: var(--border-width) solid var(--border-default); border-radius: var(--radius-sm); }
  legend { padding-inline: var(--space-1); font-weight: var(--font-weight-medium, 500); }
  .people { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: var(--space-3); }
  article { display: grid; gap: var(--space-2); align-content: start; padding: var(--space-3); border: var(--border-width) solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-inset); }
  .result-details { display: grid; gap: var(--space-2); }
  .result-details div { display: grid; grid-template-columns: minmax(10rem, 1fr) 2fr; gap: var(--space-2); }
  dt { color: var(--text-muted); }
  .error { color: var(--text-danger); }
  .stale { display: grid; gap: var(--space-2); padding: var(--space-3); border: var(--border-width) solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-inset); }
  .notice { color: var(--text-secondary); }
  @media (max-width: 640px) { .people { grid-template-columns: 1fr; } }
</style>
