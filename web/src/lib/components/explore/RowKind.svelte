<script lang="ts">
  import { entryKindPresentation } from '../../explore/labels';

  let { kind, messageType }: { kind: string; messageType: string } = $props();

  const presentation = $derived(entryKindPresentation(kind, messageType));
</script>

<span class="row-kind" aria-label={presentation.label} title={presentation.label}>
  <span class="row-kind__icon" aria-hidden="true">{presentation.icon}</span>
  <span class="row-kind__label">{presentation.name}</span>
</span>

<style>
  .row-kind {
    display: inline-flex;
    min-width: 0;
    align-items: center;
    gap: var(--space-2);
    color: var(--text-secondary);
  }

  .row-kind__icon {
    width: 14px;
    color: var(--artifact-ink);
    font-size: var(--font-size-sm);
    line-height: 1;
    text-align: center;
  }

  .row-kind__label {
    overflow: hidden;
    font-size: var(--font-size-xs);
    text-overflow: ellipsis;
    white-space: nowrap;
  }
</style>
