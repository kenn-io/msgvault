<script lang="ts">
  let {
    senderIdentities = [],
    recipientIdentities = []
  }: {
    senderIdentities?: string[];
    recipientIdentities?: string[];
  } = $props();

  const senders = $derived(uniqueSorted(senderIdentities));
  const recipients = $derived(uniqueSorted(recipientIdentities));

  function uniqueSorted(values: string[]): string[] {
    return [...new Set(values.filter(Boolean))].sort((left, right) => {
      if (left < right) return -1;
      if (left > right) return 1;
      return 0;
    });
  }
</script>

{#each senders as identity (identity)}
  <span class="identity-badge identity-badge--sender" data-testid="identity-badge">Sent via: {identity}</span>
{/each}
{#each recipients as identity (identity)}
  <span class="identity-badge identity-badge--recipient" data-testid="identity-badge">Via: {identity}</span>
{/each}

<style>
  .identity-badge {
    display: inline-block;
    max-width: 100%;
    overflow: hidden;
    padding: 1px var(--space-1);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-secondary);
    font-size: var(--font-size-2xs);
    text-overflow: ellipsis;
    vertical-align: middle;
    white-space: nowrap;
  }

  .identity-badge--sender {
    border-left: 2px solid var(--accent-teal);
  }

  .identity-badge--recipient {
    border-left: 2px solid var(--accent-blue);
  }
</style>
