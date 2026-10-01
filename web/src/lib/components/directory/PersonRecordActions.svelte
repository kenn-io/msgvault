<script lang="ts">
  import { Button, Menu, MenuContent, MenuItem, MenuTrigger, TextInput } from '@kenn-io/kit-ui';
  import Ellipsis from '@lucide/svelte/icons/ellipsis';
  import { tick } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import ProfileHistoryDialog from './ProfileHistoryDialog.svelte';

  interface Props {
    client: APIClient;
    controller: DirectoryProfileController;
    personID: number;
  }
  let { client, controller, personID }: Props = $props();
  let historyOpen = $state(false);
  let renaming = $state(false);
  let renameValue = $state('');
  let confirmingDelete = $state(false);

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
    const activeController = controller;
    const result = await activeController.deletePerson();
    if (result === undefined && activeController.draft === null) confirmingDelete = false;
  }
</script>

<Menu align="end">
  <MenuTrigger ariaLabel="More actions" title="More actions">
    <Ellipsis size={16} aria-hidden="true" />
  </MenuTrigger>
  <MenuContent ariaLabel="More actions">
    <MenuItem disabled={!controller.canWritePerson} textValue="Rename person" onselect={beginRename}>
      Rename person
    </MenuItem>
    <MenuItem textValue="View profile history" onselect={() => void openHistory()}>View profile history</MenuItem>
    <MenuItem disabled={!controller.canWritePerson} tone="danger" textValue="Delete person" onselect={beginDelete}>
      Delete person
    </MenuItem>
  </MenuContent>
</Menu>

{#if renaming}
  <div class="person-action" role="group" aria-label="Rename person">
    <label
      >Display name<TextInput
        bind:value={renameValue}
        ariaLabel="Display name"
        block
        disabled={controller.mutationPending}
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
{/if}
{#if confirmingDelete}
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
{/if}

{#if historyOpen}
  <ProfileHistoryDialog
    {client}
    {personID}
    onClose={() => {
      historyOpen = false;
    }}
  />
{/if}

<style>
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
