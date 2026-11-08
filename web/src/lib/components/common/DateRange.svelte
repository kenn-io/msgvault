<script lang="ts">
  import DateField from './DateField.svelte';
  import { Checkbox } from '@kenn-io/kit-ui';
  import { DATE_HINT, dateNote } from '../../directory/dates';
  let { start = $bindable(''), end = $bindable(''), current = $bindable(false),
    showCurrent = true, disabled = false, invalid = $bindable(false), endAfter, endForValidation }: {
    start?: string; end?: string; current?: boolean; showCurrent?: boolean; disabled?: boolean; invalid?: boolean;
    /** The start End is checked against, when the write keeps a different one than Start shows. */
    endAfter?: string; endForValidation?: string;
  } = $props();
  const endOpen = $derived(!(showCurrent && current));
  let startError = $state<string | null>(null);
  let endError = $state<string | null>(null);
  // Each field's hint's place holds the forms while it's empty or being typed, then the date in words.
  // Two empty fields share one hint: it sits under the one being typed, or under Start.
  // With Current, an empty End date says nothing until it's typed in, and Current sits under its held line.
  let focused = $state<'start' | 'end'>();
  const startNote = $derived(focused === 'end' && !start.trim() ? '' : dateNote(start, focused === 'start'));
  const endNote = $derived(!endOpen || endError ? '' : !end.trim() && focused !== 'end' && (showCurrent || startNote === DATE_HINT) ? '' : dateNote(end, focused === 'end'));
  $effect(() => { invalid = !!(startError || endError); });
</script>

<div class="date-range form-pair">
  <DateField label="Start date" bind:value={start} {disabled} bind:error={startError} note={startNote} holdNoteLine={false}
    onfocuschange={value => focused = value ? 'start' : undefined} />
  <DateField bind:value={end} start={endAfter ?? start} validationValue={endForValidation} disabled={disabled || !endOpen} bind:error={endError}
    note={endNote} holdNoteLine={showCurrent} onfocuschange={value => focused = value ? 'end' : undefined}>
    {#if showCurrent}<div class="current"><Checkbox label="Current" checked={current} {disabled} onchange={(value) => current = value} /></div>{/if}
  </DateField>
</div>

<style>
  /* Start and End are equal columns (.form-pair). Each field's hint line sits 4 below it. */
  /* Current is 8 below End's hint line, held even when empty so Current stays put, or 8 below its error. */
  .current { margin-top: var(--space-tight); }
</style>
