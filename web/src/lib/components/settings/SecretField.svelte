<script lang="ts">
  import { Button, IconButton, TextInput } from '@kenn-io/kit-ui';
  import Trash2 from '@lucide/svelte/icons/trash-2';

  // One line for a stored secret such as an API key: what is stored now, a
  // box for a new value, and the actions that apply. The daemon only reports
  // whether a secret exists, so the box is always empty until you type.
  let {
    label,
    status,
    unset = false,
    value = $bindable(''),
    disabled = false,
    disabledReason = '',
    saving = false,
    error = '',
    clearLabel,
    oninput,
    onsave,
    onclear,
  }: {
    /** The setting's label, e.g. "Text embedding API key". */
    label: string;
    /** What is stored now: "Not set", "Stored credential", … */
    status: string;
    /** Render the status in the muted "nothing here" tone. */
    unset?: boolean;
    value?: string;
    disabled?: boolean;
    /** Why the field is disabled; shown under the row. */
    disabledReason?: string;
    saving?: boolean;
    error?: string;
    /** Accessible name of the clear button; rendered only with `onclear`. */
    clearLabel?: string;
    oninput?: (value: string) => void;
    /** Save the typed value now; renders a Save button. */
    onsave?: () => void;
    /** Remove the stored secret; renders a trash icon button. */
    onclear?: () => void;
  } = $props();

  const sentence = $derived(label.charAt(0).toLowerCase() + label.slice(1));
  const blocked = $derived(disabled || saving || disabledReason !== '');
</script>

<div class="secret-field">
  <div class="secret-field__row">
    <span class="secret-field__status" class:secret-field__status--unset={unset}>{status}</span>
    <label class="secret-field__input">
      <span class="kit-sr-only">New {sentence}</span>
      <TextInput
        type="password"
        autocomplete="new-password"
        placeholder={`New ${sentence}`}
        bind:value
        disabled={blocked}
        {oninput}
        block
      />
    </label>
    {#if onsave}
      <Button
        disabled={blocked || value === ''}
        label={saving ? 'Saving…' : 'Save'}
        ariaLabel={saving ? 'Saving…' : `Save ${sentence}`}
        onclick={onsave}
      />
    {/if}
    {#if onclear}
      <IconButton ariaLabel={clearLabel ?? `Clear ${sentence}`} size="sm" disabled={blocked} onclick={onclear}>
        <Trash2 size={14} />
      </IconButton>
    {/if}
  </div>
  {#if disabledReason}<small class="secret-field__blocked">{disabledReason}</small>{/if}
  {#if error}<small class="secret-field__error" role="alert">{error}</small>{/if}
</div>

<style>
  .secret-field {
    display: grid;
    gap: var(--space-1);
    min-width: 0;
    justify-items: end;
  }
  /* One line beside the row's title: status, the box, then the actions. */
  .secret-field__row {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: var(--space-2);
    min-width: 0;
  }
  .secret-field__status {
    color: var(--text-secondary);
    white-space: nowrap;
  }
  .secret-field__status--unset {
    color: var(--text-muted);
  }
  .secret-field__input {
    display: block;
    flex: 0 0 13rem;
    min-width: 0;
  }
  .secret-field__blocked {
    color: var(--status-warning-ink);
  }
  .secret-field__error {
    color: var(--status-error-ink);
  }
  @media (max-width: 640px) {
    .secret-field {
      justify-items: start;
    }
    .secret-field__row {
      flex-wrap: wrap;
      justify-content: flex-start;
    }
    .secret-field__input {
      flex: 1 1 11rem;
      max-width: 20rem;
    }
  }
</style>
