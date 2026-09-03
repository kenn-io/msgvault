<script lang="ts">
  import { appShortcuts, Button, Modal } from '@kenn-io/kit-ui';
  import { onDestroy, onMount } from 'svelte';

  import type { CardDAVConflictChoice } from '../../carddav/conflicts-controller.svelte';

  let {
    conflictID,
    choice,
    pending,
    error,
    onConfirm,
    onClose
  }: {
    conflictID: number;
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
  const side = $derived(choice === 'keep_local' ? 'local' : 'remote');
  const title = $derived(`Keep ${side} CardDAV card`);
  const actionLabel = $derived(busy ? `Keeping ${side} card…` : `Keep ${side} card`);

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
      submitError = 'Unable to resolve this CardDAV conflict.';
    } finally {
      submitting = false;
    }
  }
</script>

<Modal
  {title}
  ariaLabel={title}
  closeLabel="Close CardDAV conflict decision"
  closable={!busy}
  closeOnOverlayClick={!busy}
  onclose={requestClose}
  maxWidth="min(560px, calc(100vw - 32px))"
>
  <!-- svelte-ignore a11y_no_noninteractive_tabindex (focusable content makes the narrow scroll region keyboard accessible) -->
  <div class="decision" role="group" aria-label="CardDAV conflict decision details" aria-busy={busy} tabindex="0">
    <p>Conflict {conflictID}: keep the {side} side for the whole card.</p>
    <p>A Deleted side is a tombstone. Choosing it deletes the whole card; choosing a present side restores that whole card.</p>
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
