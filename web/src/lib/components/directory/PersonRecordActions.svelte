<script lang="ts">
  import { Button, Menu, MenuContent, MenuItem, MenuTrigger, TextInput } from '@kenn-io/kit-ui';
  import Ellipsis from '@lucide/svelte/icons/ellipsis';
  import { tick } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import PersonTrackingControl from './PersonTrackingControl.svelte';
  import CardDAVPublicationControl from './CardDAVPublicationControl.svelte';
  import type { PersonSplitCommittedContext } from '../../directory/person-merge-history-controller.svelte';
  import { Modal } from '@kenn-io/kit-ui';
  import ProfileHistoryDialog from './ProfileHistoryDialog.svelte';
  import { joinDefinitions } from './attribute-value';

  interface Props {
    client: APIClient;
    controller: DirectoryProfileController;
    personID: number;
    onDeleted?: (personID: number) => void;
    onOpenPerson?: (id: number) => void;
    onAnnounce?: (text: string) => void;
    onOpenCardDAVConflict?: (id: number) => void;
    onOpenCardDAVSettings?: () => void;
    onSplitCommitted?: (context: PersonSplitCommittedContext) => void | Promise<void>;
  }
  let { client, controller, personID, onDeleted, onOpenPerson, onAnnounce, onOpenCardDAVConflict, onOpenCardDAVSettings, onSplitCommitted }: Props = $props();
  let maintenance = $state<'tracking' | 'publication'>();
  let historyOpen = $state(false);
  let renaming = $state(false);
  let renameValue = $state('');
  let confirmingDelete = $state(false);
  const trackingDefinitions = $derived(joinDefinitions(controller.definitions, controller.attributes?.attributes ?? []).map(field => field.definition));

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

<Menu align="end">
  <MenuTrigger ariaLabel="More actions" title="More actions">
    <Ellipsis size={16} aria-hidden="true" />
  </MenuTrigger>
  <MenuContent ariaLabel="More actions">
    <MenuItem disabled={!controller.canWritePerson} textValue="Rename" onselect={beginRename}>
      Rename
    </MenuItem>
    <MenuItem textValue="Keep this profile updated" onselect={async () => { await tick(); maintenance = 'tracking'; }}>Keep this profile updated</MenuItem>
    <MenuItem textValue="Publish to address book" onselect={async () => { await tick(); maintenance = 'publication'; }}>Publish to address book</MenuItem>
    <MenuItem textValue="History" onselect={() => void openHistory()}>History</MenuItem>
    <MenuItem disabled={!controller.canWritePerson} tone="danger" textValue="Delete" onselect={beginDelete}>
      Delete
    </MenuItem>
  </MenuContent>
</Menu>

{#if renaming}
  <Modal title="Rename person" onclose={() => { if (!controller.mutationPending) renaming = false; }}>
  <div class="person-action" role="group" aria-label="Rename person">
    <label
      >Display name<TextInput
        bind:value={renameValue}
        ariaLabel="Display name"
        block
        disabled={controller.mutationPending || controller.reloadPending}
      /></label
    >
    <div class="action-buttons">
      <Button
        label="Cancel rename"
        size="sm"
        disabled={controller.mutationPending}
        onclick={() => {
          renaming = false;
        }}
      />
      <Button
        label={controller.mutationPending ? 'Renaming…' : 'Save display name'}
        size="sm"
        disabled={!controller.canWritePerson}
        onclick={() => void saveRename()}
      />
    </div>
  </div>
  {#if controller.conflict}<p role="alert">{controller.conflict.message}</p><Button label="Reload" onclick={() => void controller.reload()} />{/if}
  </Modal>
{/if}
{#if confirmingDelete}
  <Modal title="Delete person" onclose={() => { if (!controller.mutationPending) confirmingDelete = false; }}>
  <div class="person-action delete-confirm" role="group" aria-label="Confirm deleting person">
    <span>Permanently delete this durable person profile?</span>
    <div class="action-buttons">
      <Button
        label="Cancel delete"
        size="sm"
        disabled={controller.mutationPending}
        onclick={() => {
          confirmingDelete = false;
        }}
      />
      <Button
        label={controller.mutationPending ? 'Deleting…' : 'Confirm delete person'}
        size="sm"
        tone="danger"
        surface="solid"
        disabled={!controller.canWritePerson}
        onclick={() => void deletePerson()}
      />
    </div>
  </div>
  {#if controller.conflict}<p role="alert">{controller.conflict.message}</p><Button label="Reload" onclick={() => void controller.reload()} />{/if}
  </Modal>
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

{#if maintenance === 'tracking'}
  <div class="profile-updates">
  <Modal
    title={controller.person?.display_name ? `Keep ${controller.person.display_name}'s profile updated` : 'Keep this profile updated'}
    closeLabel="Close profile updates"
    closable
    width="min(560px, calc(100vw - 32px))"
    maxWidth="min(560px, calc(100vw - 32px))"
    maxHeight="min(720px, calc(100svh - 64px))"
    onclose={() => maintenance = undefined}
  >
    <PersonTrackingControl {client} {personID} {onAnnounce} definitions={trackingDefinitions} />
    {#snippet footer()}<Button label="Close" onclick={() => maintenance = undefined} />{/snippet}
  </Modal>
  </div>
{:else if maintenance === 'publication'}
  <Modal ariaLabel="Publish to address book" onclose={() => maintenance = undefined}>
    <CardDAVPublicationControl {client} {personID} onOpenConflict={onOpenCardDAVConflict} onOpenSettings={onOpenCardDAVSettings} {onAnnounce} />
  </Modal>
{/if}

<style>
  .profile-updates :global(.kit-modal-title) {
    overflow-wrap: anywhere;
    display: -webkit-box;
    -webkit-box-orient: vertical;
    -webkit-line-clamp: 2;
    line-clamp: 2;
    overflow: hidden;
  }

  /* Full width, so inside the person header's wrapping row each group drops below the actions. */
  .person-action {
    flex: 1 0 100%;
    display: grid;
    gap: var(--space-2);
    padding: var(--space-3);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
  }
  .action-buttons,
  .delete-confirm {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .delete-confirm {
    justify-content: space-between;
    background: var(--bg-inset);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }
</style>
