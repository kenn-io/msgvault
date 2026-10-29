<script lang="ts">
  let { name, address, onOpenPerson }: { name: string; address?: string; onOpenPerson?: (address: string) => void } = $props();
</script>

{#if onOpenPerson}
  <button type="button" onclick={() => onOpenPerson?.(address || name)}>{name}</button>
{:else}
  <span>{name}</span>
{/if}

<style>
  button { padding: 0; border: 0; background: none; color: inherit; font: inherit; text-align: left; cursor: pointer; }
  button:hover { color: var(--accent-primary); text-decoration: underline; }
</style>
