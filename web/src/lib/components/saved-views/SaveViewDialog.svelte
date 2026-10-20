<script lang="ts">
  import { createSavedView as generatedCreateSavedView } from '../../api/generated/api/api';
  import { appShortcuts, Button, Modal, TextInput } from '@kenn-io/kit-ui';
  import { onMount } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { SavedView } from '../../api/generated/models';
  import type { ExploreURLState } from '../../explore/models';
  import { CURRENT_SCHEMA_VERSION, canonicalSavedViewState } from '../../saved-views/canonical';
  let {
    client,
    state: exploreState,
    onSaved,
    onclose,
  }: {
    client: APIClient;
    state: ExploreURLState;
    onSaved: (view: SavedView) => void;
    onclose: () => void;
  } = $props();
  let name = $state('');
  let nameInput = $state<HTMLInputElement>();
  // Not TextInput's autofocus: it focuses before Modal's focus trap records the
  // element to restore on close, so closing would drop focus to the body.
  // The dialog's own scope keeps Escape and the shell shortcuts away from the page behind it.
  onMount(() => {
    nameInput?.focus();
    return appShortcuts.pushScope('save-view-dialog');
  });
  let description = $state('');
  let saving = $state(false);
  let error = $state('');
  async function save(): Promise<void> {
    if (!name.trim() || saving) return;
    saving = true;
    error = '';
    try {
      const { data, error: responseError } = await generatedCreateSavedView(
        {
          name: name.trim(),
          ...(description.trim() ? { description: description.trim() } : {}),
          canonical_state: canonicalSavedViewState(exploreState),
          schema_version: CURRENT_SCHEMA_VERSION,
        },
        client,
      );
      if (!data) throw new Error(messageFor(responseError));
      onSaved(data);
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to save this view.';
    } finally {
      saving = false;
    }
  }
  function messageFor(value: unknown): string {
    const hasMessage = typeof value === 'object' && value !== null && 'message' in value;
    return hasMessage && typeof value.message === 'string'
      ? value.message
      : 'Unable to save this view.';
  }
  function requestClose(): void {
    if (!saving) onclose();
  }
</script>

<Modal title="Save view" onclose={requestClose} closeOnOverlayClick={!saving}>
  <form
    aria-busy={saving}
    onsubmit={(event) => {
      event.preventDefault();
      void save();
    }}
  >
    <label
      >Name<TextInput
        ariaLabel="Name"
        bind:value={name}
        bind:inputEl={nameInput}
        autocomplete="off"
        block
      /></label
    >
    <label
      >Description<TextInput
        ariaLabel="Description"
        placeholder="Optional"
        bind:value={description}
        autocomplete="off"
        block
      /></label
    >
    {#if exploreState.workspace === 'files'}
      <p class="note">Filename, type, and file sort aren’t saved with the view.</p>
    {/if}
    {#if error}<p class="error" role="alert">{error}</p>{/if}
    <div class="actions">
      <Button surface="soft" label="Cancel" disabled={saving} onclick={requestClose} />
      <Button
        type="submit"
        tone="info"
        surface="solid"
        label="Save"
        disabled={saving || !name.trim()}
      />
    </div>
  </form>
</Modal>

<style>
  form {
    display: grid;
    gap: var(--space-4);
    min-width: min(28rem, 80vw);
  }
  label {
    display: grid;
    gap: var(--space-1);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  p {
    margin: 0;
    font-size: var(--font-size-sm);
  }
  .note {
    color: var(--text-muted);
  }
  .error {
    color: var(--text-danger);
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
  }
</style>
