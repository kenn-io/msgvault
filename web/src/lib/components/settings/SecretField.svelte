<script lang="ts">
  import { Button, IconButton, Modal, TextInput } from '@kenn-io/kit-ui';
  import Pencil from '@lucide/svelte/icons/pencil';
  import Trash2 from '@lucide/svelte/icons/trash-2';

  // One line for a stored secret such as an API key: a read-only box that
  // shows a masked hint of the key ("sk-…x9Q") or "None", a pencil button
  // that opens a dialog to paste a new key, and a trash button that removes
  // the stored one. The daemon never sends the key itself, only the hint.
  let {
    label,
    configured = false,
    hint = '',
    source = undefined,
    disabled = false,
    disabledReason = '',
    saving = false,
    error = '',
    applyNote = '',
    clearLabel,
    onreplace,
    onclear,
  }: {
    /** The setting's label, e.g. "Text embedding API key". */
    label: string;
    /** Whether a key is set at all. */
    configured?: boolean;
    /** Masked hint of the set key; empty when the daemon has none to give. */
    hint?: string;
    /** Where the key comes from; an environment variable cannot be cleared here. */
    source?: 'stored' | 'environment' | 'none' | string;
    disabled?: boolean;
    /** Why the field is disabled; shown under the row. */
    disabledReason?: string;
    saving?: boolean;
    error?: string;
    /** When a new key takes effect; shown in the dialog. */
    applyNote?: string;
    /** Accessible name of the clear button; rendered only with `onclear`. */
    clearLabel?: string;
    /**
     * Accept the key from the dialog. Return true to close the dialog, false
     * to keep it open (an `error` will show inside it).
     */
    onreplace: (value: string) => boolean | Promise<boolean>;
    /** Remove the stored secret; renders a trash icon button. */
    onclear?: () => void;
  } = $props();

  const sentence = $derived(label.charAt(0).toLowerCase() + label.slice(1));
  const blocked = $derived(disabled || saving || disabledReason !== '');
  const verb = $derived(configured ? 'Replace' : 'Add');
  // A set key with no hint still shows as set, just without characters.
  const shown = $derived(configured ? hint || '••••••••' : 'None');

  let open = $state(false);
  let draft = $state('');

  function openDialog() {
    if (blocked) return;
    draft = '';
    open = true;
  }
  function closeDialog() {
    if (saving) return;
    open = false;
    draft = '';
  }
  async function submit() {
    if (draft === '' || blocked) return;
    if (await onreplace(draft)) closeDialog();
  }
</script>

<div class="secret-field">
  <div class="secret-field__row">
    <output class="secret-field__value" class:secret-field__value--none={!configured} aria-label={label}>{shown}</output>
    <IconButton ariaLabel={`${verb} ${sentence}`} size="sm" disabled={blocked} onclick={openDialog}>
      <Pencil size={14} />
    </IconButton>
    {#if onclear && configured}
      <IconButton ariaLabel={clearLabel ?? `Clear ${sentence}`} size="sm" disabled={blocked} onclick={onclear}>
        <Trash2 size={14} />
      </IconButton>
    {/if}
  </div>
  {#if configured && source === 'environment'}
    <small class="secret-field__note">From an environment variable on the daemon host.</small>
  {/if}
  {#if disabledReason}<small class="secret-field__blocked">{disabledReason}</small>{/if}
  {#if error && !open}<small class="secret-field__error" role="alert">{error}</small>{/if}
</div>

{#if open}
  <Modal title={`${verb} ${sentence}`} onclose={closeDialog} closeOnOverlayClick={!saving}>
    <form
      class="secret-field__form"
      aria-busy={saving}
      onsubmit={(event) => {
        event.preventDefault();
        void submit();
      }}
    >
      <label class="secret-field__label">
        <span>New {sentence}</span>
        <TextInput
          type="password"
          autocomplete="new-password"
          ariaLabel={`New ${sentence}`}
          bind:value={draft}
          disabled={saving}
          block
        />
      </label>
      {#if applyNote}<p class="secret-field__apply">{applyNote}</p>{/if}
      {#if error}<p class="secret-field__error" role="alert">{error}</p>{/if}
    </form>
    {#snippet footer()}
      <Button surface="soft" label="Cancel" disabled={saving} onclick={closeDialog} />
      <Button
        tone="info"
        surface="solid"
        label={saving ? 'Saving…' : 'Save'}
        ariaLabel={saving ? 'Saving…' : `Save ${sentence}`}
        disabled={draft === '' || saving}
        onclick={() => void submit()}
      />
    {/snippet}
  </Modal>
{/if}

<style>
  .secret-field {
    display: grid;
    gap: var(--space-1);
    min-width: 0;
    justify-items: end;
  }
  /* One line beside the row's title, as wide as a text input (15rem). The
     box fills whatever the buttons leave, so the trash button appearing
     takes room from the box and never moves the line's left edge. */
  .secret-field__row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    width: 15rem;
    min-width: 0;
  }
  .secret-field__value {
    box-sizing: border-box;
    display: flex;
    align-items: center;
    flex: 1 1 auto;
    min-width: 0;
    height: 28px;
    padding: 0 var(--space-2);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--surface-inset, transparent);
    color: var(--text-primary);
    font-family: var(--font-mono);
    font-size: var(--font-size-sm);
    letter-spacing: 0.02em;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .secret-field__value--none {
    color: var(--text-muted);
    font-family: inherit;
    letter-spacing: normal;
  }
  .secret-field__note,
  .secret-field__apply {
    color: var(--text-muted);
  }
  .secret-field__blocked {
    color: var(--status-warning-ink);
  }
  .secret-field__error {
    color: var(--status-error-ink);
  }
  .secret-field__form {
    display: grid;
    gap: var(--space-3);
    min-width: min(24rem, 80vw);
  }
  .secret-field__label {
    display: grid;
    gap: var(--space-1);
    color: var(--text-secondary);
  }
  .secret-field__apply {
    margin: 0;
    font-size: var(--font-size-xs);
  }
</style>
