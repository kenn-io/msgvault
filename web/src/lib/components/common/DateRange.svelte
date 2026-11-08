<script lang="ts">
  import DateField from './DateField.svelte';
  import { Checkbox } from '@kenn-io/kit-ui';
  import { DATE_HINT, dateNote } from '../../directory/dates';
  let { start = $bindable(''), end = $bindable(''), current = $bindable(false),
    showCurrent = true, disabled = false, invalid = $bindable(false) }: {
    start?: string; end?: string; current?: boolean; showCurrent?: boolean; disabled?: boolean; invalid?: boolean;
  } = $props();
  const endOpen = $derived(!(showCurrent && current));
  let startError = $state<string | null>(null);
  let endError = $state<string | null>(null);
  // Each field's hint's place holds the forms while it's empty or being typed, then the date in words.
  // Two empty fields share one hint: it sits under the one being typed, or under Start.
  // With Current, an empty End date says nothing until it's typed in, so Current can take its line.
  let focused = $state<'start' | 'end'>();
  const startNote = $derived(focused === 'end' && !start.trim() ? '' : dateNote(start, focused === 'start'));
  const endNote = $derived(!endOpen || endError ? '' : !end.trim() && focused !== 'end' && (showCurrent || startNote === DATE_HINT) ? '' : dateNote(end, focused === 'end'));
  // End's line is free when it shows no reading and no error.
  const endLineFree = $derived(!endNote && !endError);
  $effect(() => { invalid = !!(startError || endError); });
</script>

<div class="date-range form-pair">
  <DateField label="Start date" bind:value={start} {disabled} bind:error={startError} note={startNote}
    onfocuschange={value => focused = value ? 'start' : undefined} />
  <DateField bind:value={end} {start} disabled={disabled || !endOpen} bind:error={endError}
    note={endNote} showNote={!(showCurrent && endLineFree)} holdNoteLine={false} onfocuschange={value => focused = value ? 'end' : undefined}>
    {#if showCurrent}<div class="current" class:below={!endLineFree}><Checkbox label="Current" checked={current} {disabled} onchange={(value) => current = value} /></div>{/if}
  </DateField>
</div>

<style>
  /* Start and End are equal columns (.form-pair). Each field's hint line sits 4 below it. */
  /* In the hint line, Current is 4 below End date like the hint. Under a reading or an error, it's 8 below it. */
  .current.below { margin-top: var(--space-tight); }
</style>
