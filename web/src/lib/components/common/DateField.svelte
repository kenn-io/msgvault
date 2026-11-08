<script lang="ts">
  import { FormField } from '@kenn-io/kit-ui';
  import type { Snippet } from 'svelte';
  import { dateNote, intervalDateError } from '../../directory/dates';
  let { label = 'End date', value = $bindable(''), start = '', validationValue, disabled = false, error = $bindable<string | null>(null), note, holdNoteLine = true, onfocuschange, children }: {
    label?: string; value?: string; start?: string; validationValue?: string; disabled?: boolean; error?: string | null; note?: string;
    /** An empty note keeps its line, so the field below stays put as a note comes and goes. Off, an empty note takes no line on a phone. */
    holdNoteLine?: boolean;
    onfocuschange?: (focused: boolean) => void; children?: Snippet;
  } = $props();
  const id = $props.id();
  let focused = $state(false);
  const shownNote = $derived(note ?? dateNote(value, focused));
  $effect(() => { error = disabled ? null : intervalDateError(validationValue ?? value, start); });
</script>

<div class="field" onfocusin={event => { focused = (event.target as HTMLInputElement).type === 'text'; onfocuschange?.(focused); }} onfocusout={() => { focused = false; onfocuschange?.(false); }}>
  <FormField field={{ id, label, value, error: error ?? undefined, disabled }} oninput={next => value = next} />
  {#if !error}<p class="note" class:spare={!holdNoteLine && !shownNote} class:repeated={!holdNoteLine && !focused && /^\d{4}$/.test(value.trim()) && !shownNote}>{shownNote}</p>{/if}
  {@render children?.()}
</div>

<style>
  .field { display: grid; gap: var(--space-tight); align-content: start; min-width: 0; }
  .note { min-height: var(--type-small-line); margin: 0; color: var(--text-muted); font-size: var(--type-small-size); line-height: var(--type-small-line); }
  .note.repeated { display: none; }
  /* Stacked on a phone, the field has no neighbor to line up with, so an empty line it needn't hold is gone. */
  @media (max-width: 640px) {
    .note.spare { display: none; }
  }
</style>
