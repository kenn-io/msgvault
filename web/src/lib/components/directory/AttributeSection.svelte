<script lang="ts">
  import { valueProvenance } from './history-format';
  import HistorySection, { type HistoryRow } from './HistorySection.svelte';
  import { Button } from '@kenn-io/kit-ui';
  import { untrack } from 'svelte';
  import type { AttributeDefinition, PersonAttributeValue } from '../../api/generated/models';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import { displayAttributeValue, joinDefinitions, isAttributeRevealed } from './attribute-value';

  let { controller }: { controller: DirectoryProfileController } = $props();
  let revealed = $state<Record<string, boolean>>({});
  let owner = untrack(() => controller);
  $effect(() => {
    if (controller === owner) return;
    owner = controller;
    revealed = {};
  });
  const visibleFields = $derived(joinDefinitions(controller.definitions, controller.attributes?.attributes ?? []).filter(field => field.current.length > 0 || field.history.length > 0));
  function isRevealed(definition: AttributeDefinition): boolean { return isAttributeRevealed(definition, revealed); }
  function toggleReveal(definition: AttributeDefinition): void {
    revealed = { ...revealed, [definition.universal_id]: !revealed[definition.universal_id] };
  }
  function rows(definition: AttributeDefinition, values: PersonAttributeValue[]): HistoryRow[] {
    const shown = isRevealed(definition);
    return values.map(value => ({ id: value.id, value: shown ? displayAttributeValue(definition, value.value) : 'Hidden', provenance: valueProvenance(value) }));
  }
</script>

{#each visibleFields as field (field.definition.universal_id)}
  {@const shown = isRevealed(field.definition)}
  {@const first = (field.current[0] ?? field.history[0])?.id}
  <!-- Reveal sits beside the field's first value; a hidden value reads "Hidden". -->
  {#snippet reveal(row: HistoryRow)}
    {#if field.definition.is_sensitive && row.id === first}<Button size="sm" class="text-action" label={shown ? 'Hide' : 'Reveal'} ariaLabel={`${shown ? 'Hide' : 'Reveal'} ${field.definition.label} values`} onclick={() => toggleReveal(field.definition)} />{/if}
  {/snippet}
  {#snippet concealed(row: HistoryRow)}<span class:concealed={!shown}>{row.value}</span>{/snippet}
  <HistorySection title={field.definition.label} rows={rows(field.definition, field.current)} earlier={rows(field.definition, field.history)}
    rowActions={field.definition.is_sensitive ? reveal : undefined} value={concealed} />
{/each}

<style>
  .concealed { color: var(--text-muted); }
</style>
