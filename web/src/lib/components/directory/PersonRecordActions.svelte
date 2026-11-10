<script lang="ts">
  import { Menu, MenuContent, MenuItem, MenuTrigger, Modal, TextInput } from '@kenn-io/kit-ui';
  import Ellipsis from '@lucide/svelte/icons/ellipsis';
  import { onDestroy, tick, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import { PersonTrackingController } from '../../directory/person-tracking-controller.svelte';
  import PersonTrackingControl from './PersonTrackingControl.svelte';
  import CardDAVPublicationControl from './CardDAVPublicationControl.svelte';
  import type { PersonSplitCommittedContext } from '../../directory/person-merge-history-controller.svelte';
  import ProfileHistoryDialog from './ProfileHistoryDialog.svelte';
  import Dialog from '../common/Dialog.svelte';
  import InlineNotice from '../common/InlineNotice.svelte';

  interface Props {
    client: APIClient;
    controller: DirectoryProfileController;
    personID: number;
    onDeleted?: (personID: number) => void;
    onOpenPerson?: (id: number) => void;
    /** Set when lookups aren't set up, so Keep this profile updated opens their settings. */
    onSetUpTracking?: () => void;
    onAnnounce?: (text: string) => void;
    onOpenCardDAVConflict?: (id: number) => void;
    onOpenCardDAVSettings?: () => void;
    onSplitCommitted?: (context: PersonSplitCommittedContext) => void | Promise<void>;
  }
  let { client, controller, personID, onDeleted, onOpenPerson, onSetUpTracking, onAnnounce, onOpenCardDAVConflict, onOpenCardDAVSettings, onSplitCommitted }: Props = $props();
  let publishing = $state(false);
  let historyOpen = $state(false);
  let renaming = $state(false);
  let renameValue = $state('');
  let confirmingDelete = $state(false);
  // The menu's content unmounts when it closes; the tracking state lives here so a save outlives it.
  const tracking = new PersonTrackingController(untrack(() => client));
  $effect.pre(() => {
    const nextPersonID = personID;
    untrack(() => { if (tracking.personID !== undefined) void tracking.setPerson(nextPersonID); });
  });
  onDestroy(() => tracking.destroy());

  function beginRename(): void {
    renameValue = controller.person?.display_name ?? '';
    renaming = true;
    confirmingDelete = false;
  }
  function beginDelete(): void {
    confirmingDelete = true;
    renaming = false;
  }
  // The menu returns focus to its trigger after a tick; open the dialog after
  // that so the dialog keeps focus and restores it to the trigger on close.
  async function openHistory(): Promise<void> {
    await tick();
    historyOpen = true;
  }
  /** The server's words go under Details; the sentence says what happened to the change. */
  function conflictSentence(verb: 'save' | 'delete'): string {
    const status = controller.conflict?.status;
    return status === 409 || status === 412 || status === 428 ? 'This person changed since you opened it.' : `Couldn't ${verb} this person.`;
  }
  async function saveRename(): Promise<void> {
    const result = await controller.rename(renameValue.trim() || null);
    if (result === undefined && controller.draft === null) renaming = false;
  }
  async function deletePerson(): Promise<void> {
    const activeController = controller, deletedID = personID;
    const result = await activeController.deletePerson();
    if (result === undefined && activeController.draft === null) { confirmingDelete = false; onDeleted?.(deletedID); }
  }
</script>

<!-- Each open reads the state again, since a split or another tab may have changed it; a save in flight keeps its own. -->
<Menu align="end" onopenchange={(open) => { if (open) void (tracking.personID === personID ? tracking.retryTracking() : tracking.setPerson(personID)); }}>
  <MenuTrigger ariaLabel="More actions" title="More actions">
    <Ellipsis size={16} aria-hidden="true" />
  </MenuTrigger>
  <MenuContent ariaLabel="More actions">
    <MenuItem disabled={!controller.canWritePerson} textValue="Rename…" onselect={beginRename}>Rename…</MenuItem>
    <PersonTrackingControl controller={tracking} {onAnnounce} onSetUp={onSetUpTracking} />
    <MenuItem textValue="Publish to address book…" onselect={async () => { await tick(); publishing = true; }}>Publish to address book…</MenuItem>
    <MenuItem textValue="History" onselect={() => void openHistory()}>History</MenuItem>
    <MenuItem disabled={!controller.canWritePerson} tone="danger" textValue="Delete…" onselect={beginDelete}>Delete…</MenuItem>
  </MenuContent>
</Menu>

{#if renaming}
  <Dialog title="Rename person" width="small" onclose={() => renaming = false}
    primaryLabel={controller.mutationPending ? 'Saving…' : 'Save'} primaryDisabled={!controller.canWritePerson || controller.reloadPending}
    busy={controller.mutationPending} onprimary={() => void saveRename()}>
    <form class="rename" aria-label="Rename person" onsubmit={(event) => { event.preventDefault(); if (controller.canWritePerson && !controller.mutationPending) void saveRename(); }}>
      <label>Display name<TextInput bind:value={renameValue} ariaLabel="Display name" block disabled={controller.mutationPending || controller.reloadPending} /></label>
    </form>
    {#snippet notice()}{#if controller.conflict}<InlineNotice message={conflictSentence('save')} actionLabel="Reload" onaction={() => void controller.reload()} details={controller.conflict.message} />{/if}{/snippet}
  </Dialog>
{/if}
{#if confirmingDelete}
  <Dialog title={`Delete ${controller.person?.display_name?.trim() || 'this person'}?`} width="small" onclose={() => confirmingDelete = false}
    primaryLabel="Delete" primaryTone="danger" primaryDisabled={!controller.canWritePerson}
    busy={controller.mutationPending} onprimary={() => void deletePerson()}>
    <p class="confirm">This permanently removes the person.</p>
    {#snippet notice()}{#if controller.conflict}<InlineNotice message={conflictSentence('delete')} actionLabel="Reload" onaction={() => void controller.reload()} details={controller.conflict.message} />{/if}{/snippet}
  </Dialog>
{/if}

{#if historyOpen}
  <ProfileHistoryDialog
    {client}
    {personID}
    {controller}
    {onOpenPerson}
    {onSplitCommitted}
    onClose={() => {
      historyOpen = false;
    }}
  />
{/if}

{#if publishing}
  <Modal ariaLabel="Publish to address book" onclose={() => publishing = false}>
    <CardDAVPublicationControl {client} {personID} onOpenConflict={onOpenCardDAVConflict} onOpenSettings={onOpenCardDAVSettings} {onAnnounce} />
  </Modal>
{/if}

<style>
  /* A form field: the label (Meta, 600) 4 above the 32 input. */
  .rename label {
    display: grid;
    gap: var(--space-tight);
    font-size: var(--type-meta-size);
    font-weight: 600;
    line-height: var(--type-meta-line);
  }
  .confirm { margin: 0; font-size: var(--type-body-size); line-height: var(--type-body-line); }
</style>
