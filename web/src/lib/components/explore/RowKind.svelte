<script lang="ts">
  let { kind, messageType }: { kind: string; messageType: string } = $props();

  const presentation = $derived.by(() => {
    const normalizedKind = kind.toLowerCase();
    if (normalizedKind === 'email') return { icon: '✉', label: 'Email item' };
    if (normalizedKind === 'conversation') return { icon: '◌', label: 'Conversation item' };
    if (normalizedKind === 'event') return { icon: '□', label: 'Calendar event' };
    if (normalizedKind === 'meeting') return { icon: '◫', label: 'Meeting item' };
    if (normalizedKind === 'file') return { icon: '▱', label: 'File item' };
    const normalized = messageType.toLowerCase();
    if (normalized === 'email') return { icon: '✉', label: 'Email item' };
    if (normalized === 'chat' || normalized === 'text') {
      return { icon: '◌', label: 'Conversation item' };
    }
    if (normalized === 'calendar') return { icon: '□', label: 'Calendar event' };
    if (normalized === 'meeting') return { icon: '◫', label: 'Meeting item' };
    return { icon: '◇', label: 'Archive item' };
  });
</script>

<span class="row-kind" aria-label={presentation.label} title={presentation.label}>
  <span class="row-kind__icon" aria-hidden="true">{presentation.icon}</span>
  <span class="row-kind__label">{presentation.label.replace(' item', '').replace('Archive', 'Item')}</span>
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
