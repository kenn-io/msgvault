<script lang="ts">
  import { untrack, type ComponentProps } from 'svelte';
  import DomainCalendar from './DomainCalendar.svelte';
  let props: ComponentProps<typeof DomainCalendar> = $props();
  let year = $state<number | null>(null);
  $effect(() => { void props.domain; untrack(() => { year = null; }); });
</script>
<DomainCalendar {...props} bind:year />
