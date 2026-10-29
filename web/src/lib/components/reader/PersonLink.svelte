<script lang="ts">
  let { name, participantID, onOpenPerson }: { name: string; participantID?: number; onOpenPerson?: (participantID: number) => void } = $props();
</script>

{#if onOpenPerson && participantID && participantID > 0}
  <button type="button" onclick={() => onOpenPerson?.(participantID!)}>{name}</button>
{:else}
  <span>{name}</span>
{/if}

<style>
  button { padding: 0; border: 0; background: none; color: inherit; font: inherit; text-align: left; cursor: pointer; }
  button:hover { color: var(--accent-primary); text-decoration: underline; }
</style>
