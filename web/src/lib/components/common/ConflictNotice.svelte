<script lang="ts">
  import type { Snippet } from 'svelte';
  import InlineNotice from './InlineNotice.svelte';
  import { conflictSaved } from '../../directory/labels';

  let { noun, record, saved, children, message, actionLabel, onaction }: {
    noun: string;
    /** The saved version beside the form, label to value. */
    record: Record<string, string>;
    saved: { updated_at: string; source?: string; updated_by?: string };
    /** More of the saved version, after its rows. */
    children?: Snippet;
    message: string;
    actionLabel?: string;
    onaction?: () => void;
  } = $props();
  const provenance = $derived(conflictSaved(saved.updated_at, saved.updated_by === 'user' ? undefined : saved.source));
</script>

<InlineNotice tone="warning" message={`This ${noun} changed since you opened it.`} details={message || undefined} {actionLabel} {onaction}>
  <section aria-label={`Saved ${noun}`}>
    <dl>{#each Object.entries(record) as [label, value] (label)}<div><dt>{label}</dt><dd>{value || 'None'}</dd></div>{/each}</dl>
    {@render children?.()}
    <p class="provenance" title={provenance.title}>{provenance.text}</p>
  </section>
</InlineNotice>

<style>
  /* The saved version as L3 label | value rows on the 160 column; when it was saved (Meta) 8 below. */
  section { display: grid; gap: var(--space-group); min-width: 0; }
  dl { display: grid; margin: 0; }
  dl div { display: grid; grid-template-columns: var(--detail-label-width) minmax(0, 1fr); align-items: baseline; gap: var(--space-group); padding-block: var(--row-pad-y); }
  dt { color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  dd { margin: 0; overflow-wrap: anywhere; font-size: var(--type-body-size); line-height: var(--type-body-line); }
  .provenance { margin: 0; color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
</style>
