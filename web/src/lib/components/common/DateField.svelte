<script lang="ts">
  import { FormField } from '@kenn-io/kit-ui';
  import type { Snippet } from 'svelte';
  import { dateNote, intervalDateError } from '../../directory/dates';
  let { label = 'End date', value = $bindable(''), start = '', disabled = false, error = $bindable<string | null>(null), note, showNote = true, onfocuschange, children }: {
    label?: string; value?: string; start?: string; disabled?: boolean; error?: string | null; note?: string; showNote?: boolean;
    onfocuschange?: (focused: boolean) => void; children?: Snippet;
  } = $props();
  const id = $props.id();
  let focused = $state(false);
  $effect(() => { error = disabled ? null : intervalDateError(value, start); });
</script>

<div class="field" onfocusin={event => { focused = (event.target as HTMLInputElement).type === 'text'; onfocuschange?.(focused); }} onfocusout={() => { focused = false; onfocuschange?.(false); }}>
  <FormField field={{ id, label, value, error: error ?? undefined, disabled }} oninput={next => value = next} />
  {#if showNote && !error}<p class="note">{note ?? dateNote(value, focused)}</p>{/if}
  {@render children?.()}
</div>

<style>
  .field { display: grid; gap: var(--space-tight); align-content: start; min-width: 0; }
  .note { min-height: var(--type-small-line); margin: 0; color: var(--text-muted); font-size: var(--type-small-size); line-height: var(--type-small-line); }
</style>
