<script lang="ts">
  import { appShortcuts, Button, Modal } from '@kenn-io/kit-ui';
  import { onDestroy, onMount } from 'svelte';

  import type { CardDAVConflictChoice, CardDAVConflictSummary } from '../../carddav/conflicts-controller.svelte';

  let {
    addressBookName,
    localState,
    remoteState,
    choice,
    pending,
    error,
    onConfirm,
    onClose
  }: {
    addressBookName: string;
    localState: CardDAVConflictSummary['state'];
    remoteState: CardDAVConflictSummary['state'];
    choice: CardDAVConflictChoice;
    pending: boolean;
    error: string | null;
    onConfirm: () => void | Promise<void>;
    onClose: () => void;
  } = $props();

  let submitting = $state(false);
  let submitError = $state<string | null>(null);
  let releaseShortcutScope: (() => void) | undefined;

  const busy = $derived(pending || submitting);
  const source = $derived(choice === 'keep_local' ? 'msgvault' : 'address book');
  const title = $derived(choice === 'keep_local'
    ? localState === 'deleted' ? 'Delete from address book'
      : remoteState === 'deleted' ? 'Restore in address book' : `Use ${source} version`
    : remoteState === 'deleted' ? 'Keep contact deleted'
      : localState === 'deleted' && remoteState === 'present' ? 'Keep in address book' : `Use ${source} version`);
  const actionLabel = $derived(busy ? 'Saving…' : title);
  const consequence = $derived(choice === 'keep_local'
    ? localState === 'deleted'
      ? `Delete this contact from ${addressBookName}. Other details in msgvault are kept.`
      : remoteState === 'deleted'
        ? `Restore this contact in ${addressBookName} using the msgvault version.`
        : `Replace this contact in ${addressBookName} with the msgvault version.`
    : remoteState === 'deleted'
      ? `Keep this contact deleted in ${addressBookName} and stop syncing it. msgvault keeps your edits and links to messages or notes. A contact used only by this address book is removed from msgvault.`
      : localState === 'deleted' && remoteState === 'present'
        ? `Keep this contact in ${addressBookName} and resume syncing its details with msgvault.`
        : `Use the contact details from ${addressBookName} in msgvault.`);

  onMount(() => {
    releaseShortcutScope = appShortcuts.pushScope('carddav-conflict-decision-modal');
  });

  onDestroy(() => releaseShortcutScope?.());

  function requestClose(): void {
    if (busy) return;
    onClose();
  }

  async function confirm(): Promise<void> {
    if (busy) return;
    submitting = true;
    submitError = null;
    try {
      await onConfirm();
    } catch {
      submitError = 'Unable to save your choice. Try again.';
    } finally {
      submitting = false;
    }
  }
</script>

<Modal
  {title}
  ariaLabel={title}
  closeLabel="Close contact choice"
  closable={!busy}
  closeOnOverlayClick={!busy}
  onclose={requestClose}
  maxWidth="min(560px, calc(100vw - 32px))"
>
  <!-- svelte-ignore a11y_no_noninteractive_tabindex (focusable content makes the narrow scroll region keyboard accessible) -->
  <div class="decision" role="group" aria-label="Contact choice details" aria-busy={busy} tabindex="0">
    <p>{consequence}</p>
    {#if localState !== 'deleted' && remoteState !== 'deleted'}
      <p>This choice includes contact details that aren't shown here.</p>
    {/if}
    {#if error || submitError}<p class="error" role="alert">{error ?? submitError}</p>{/if}
  </div>

  {#snippet footer()}
    <Button surface="soft" label="Cancel" disabled={busy} onclick={requestClose} />
    <Button tone="info" surface="solid" label={actionLabel} disabled={busy} onclick={() => void confirm()} />
  {/snippet}
</Modal>

<style>
  .decision { display: grid; gap: var(--space-4); min-width: min(28rem, calc(100vw - 64px)); }
  p { margin: 0; }
  .error { color: var(--text-danger); }
</style>
