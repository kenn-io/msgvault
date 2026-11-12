<script lang="ts">
  import { provenanceDetails } from './history-format';
  let { value }: { value: Parameters<typeof provenanceDetails>[0] } = $props();
</script>

<dl>{#each provenanceDetails(value) as [label, text] (label)}<div><dt>{label}</dt><dd>{text}</dd></div>{/each}</dl>

<style>
  /* The Facts Details list: a 160 label column, 8, then the value, each pair 4 above and below. */
  dl { display: grid; margin: 0; }
  dl > div { display: grid; grid-template-columns: var(--detail-label-width) minmax(0, 1fr); align-items: baseline; column-gap: var(--space-group); padding: var(--space-tight) 0; }
  dt { color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  dd { margin: 0; font-size: var(--type-body-size); line-height: var(--type-body-line); overflow-wrap: anywhere; }
  @media (max-width: 640px) { dl > div { grid-template-columns: minmax(0, 1fr); } }
</style>
