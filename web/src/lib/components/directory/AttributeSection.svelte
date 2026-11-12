<script lang="ts">
  import { valueProvenance } from './history-format';
  import HistorySection, { type HistoryRow } from './HistorySection.svelte';
  import DetailRow from '../people/DetailRow.svelte';
  import ProvenanceDetails from './ProvenanceDetails.svelte';
  import { Button } from '@kenn-io/kit-ui';
  import { untrack } from 'svelte';
  import type { AttributeDefinition, PersonAttributeValue } from '../../api/generated/models';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import { displayAttributeValue, joinDefinitions, isAttributeRevealed } from './attribute-value';

  let { controller, universalIDs, facts = false, pinState = '' }: { controller: DirectoryProfileController; universalIDs?: string[]; facts?: boolean; pinState?: string } = $props();
  let revealed = $state<Record<string, boolean>>({});
  let owner = untrack(() => controller);
  $effect(() => {
    if (controller === owner) return;
    owner = controller;
    revealed = {};
  });
  const visibleFields = $derived(joinDefinitions(controller.definitions, controller.attributes?.attributes ?? []).filter(field => (!universalIDs || universalIDs.includes(field.definition.universal_id)) && (field.current.length > 0 || (!facts && field.history.length > 0))));
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
  {#if facts}
    <section aria-label={field.definition.label}>
      {#each field.current as value (value.id)}
        <DetailRow>
          <strong class="field-name">{field.definition.label}</strong>
          <span>{shown ? displayAttributeValue(field.definition, value.value) : 'Hidden'}</span>
          {#snippet meta()}<span title={valueProvenance(value).title}>{#if pinState}{pinState}{' · '}{/if}{valueProvenance(value).text}</span>{/snippet}
          {#snippet actions()}{#if field.definition.is_sensitive}<Button size="sm" class="text-action" label={shown ? 'Hide' : 'Reveal'} ariaLabel={`${shown ? 'Hide' : 'Reveal'} ${field.definition.label} values`} onclick={() => toggleReveal(field.definition)} />{/if}{/snippet}
          {#snippet details()}<ProvenanceDetails {value} />{/snippet}
        </DetailRow>
      {/each}
    </section>
  {:else}
  {@const first = (field.current[0] ?? field.history[0])?.id}
  <!-- Reveal sits beside the field's first value; a hidden value reads "Hidden". -->
  {#snippet reveal(row: HistoryRow)}
    {#if field.definition.is_sensitive && row.id === first}<Button size="sm" class="text-action" label={shown ? 'Hide' : 'Reveal'} ariaLabel={`${shown ? 'Hide' : 'Reveal'} ${field.definition.label} values`} onclick={() => toggleReveal(field.definition)} />{/if}
  {/snippet}
  {#snippet concealed(row: HistoryRow)}<span class:concealed={!shown}>{row.value}</span>{/snippet}
  <HistorySection title={field.definition.label} rows={rows(field.definition, field.current)} earlier={rows(field.definition, field.history)}
    rowActions={field.definition.is_sensitive ? reveal : undefined} value={concealed} />
  {/if}
{/each}

<style>
  .field-name { display: block; font-size: var(--type-heading-size); font-weight: 600; line-height: var(--type-heading-line); }
  .concealed { color: var(--text-muted); }
</style>
